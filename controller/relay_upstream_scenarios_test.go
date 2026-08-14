package controller

// ---------------------------------------------------------------------------
// P0 验收：上游 401 / 429 / 5xx / 超时 / 余额不足 时的任务提交处理结果。
//
// 测试方法：controller 层集成测试，直接调用 RelayTask（走真实的关键处理链路：
// GenRelayInfo → 提交锁 → 渠道选择 → RelayTaskSubmit → DoTaskApiRequest →
// httptest Mock 上游 → 错误分类 → 渠道健康处理（自动禁用）→ outcome_unknown
// 恢复记录 → respondTaskError → 计费退款 → 并发名额释放），不修改任何真实渠道，
// 不向任何真实火山方舟 Endpoint 发起请求。
//
// 每个场景断言：
//   - 客户端 HTTP 状态码 / 错误码 / 文案（含脱敏）
//   - 上游 POST 次数（明确失败与结果未知均不得重复 POST）
//   - 本地任务记录是否创建
//   - task_submit_recovery / outcome_unknown 恢复记录是否生成
//   - 用户预扣额度是否正确退回或保留
//   - Seedance 并发名额是否释放或由恢复记录继续持有
//   - 渠道是否按当前配置自动禁用
//   - 日志是否包含状态、重试次数与请求串联信息
//   - 响应与日志不泄露 Endpoint / API Key / Bearer Token
// ---------------------------------------------------------------------------

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	relayconstant "github.com/QuantumNous/new-api/relay/constant"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/service"

	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

const (
	scenarioTestModel = "doubao-seedance-2-0-260128"
	scenarioUserID    = 1
	scenarioChannelID = 1
	scenarioUserQuota = 100_000_000
)

// setupScenarioGlobals 保存并恢复本测试修改的全局配置；
// relayTimeout>0 时重建带超时的全局 http client（超时场景必需）。
func setupScenarioGlobals(t *testing.T, retryTimes, relayTimeout int) {
	t.Helper()
	gRedis := common.RedisEnabled
	gMemory := common.MemoryCacheEnabled
	gBatch := common.BatchUpdateEnabled
	gAutoDisable := common.AutomaticDisableChannelEnabled
	gAutoEnable := common.AutomaticEnableChannelEnabled
	gRetry := common.RetryTimes
	gRelayTimeout := common.RelayTimeout
	gMaxConc := constant.SeedanceMaxConcurrentTasks
	gDB := model.DB
	gLogDB := model.LOG_DB

	// 与生产配置一致：渠道开启自动禁用（本测试只验证"按当前配置"的行为）
	common.AutomaticDisableChannelEnabled = true
	common.AutomaticEnableChannelEnabled = false
	// 默认本机测试无 Redis / 内存缓存 / 批量更新：全部走数据库
	common.RedisEnabled = false
	common.MemoryCacheEnabled = false
	common.BatchUpdateEnabled = false
	common.RetryTimes = retryTimes
	common.RelayTimeout = relayTimeout
	// 让 Seedance 并发名额实际落库（>0 才写入 task_concurrency_slots）
	constant.SeedanceMaxConcurrentTasks = 3

	if relayTimeout > 0 {
		// 全局 http client 在创建时读取 RelayTimeout，超时场景必须先重建
		service.InitHttpClient()
	}

	t.Cleanup(func() {
		common.RedisEnabled = gRedis
		common.MemoryCacheEnabled = gMemory
		common.BatchUpdateEnabled = gBatch
		common.AutomaticDisableChannelEnabled = gAutoDisable
		common.AutomaticEnableChannelEnabled = gAutoEnable
		common.RetryTimes = gRetry
		common.RelayTimeout = gRelayTimeout
		constant.SeedanceMaxConcurrentTasks = gMaxConc
		model.DB = gDB
		model.LOG_DB = gLogDB
		if relayTimeout > 0 {
			// 恢复无超时的全局 client，避免影响同包其他测试
			service.InitHttpClient()
		}
	})
}

// setupScenarioDB 建立内存 SQLite 并迁移全部需要的表。
func setupScenarioDB(t *testing.T) {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1) // 内存 SQLite：单连接，避免并发连接各自独立库
	model.DB = db
	model.LOG_DB = db
	require.NoError(t, db.AutoMigrate(
		&model.Channel{},
		&model.User{},
		&model.Token{},
		&model.Task{},
		&model.TaskSubmitRecovery{},
		&model.TaskSubmitLockRow{},
		&model.TaskConcurrencySlot{},
		&model.SeedanceCostControl{},
		&model.Ability{},
		&model.Log{},
	))
}

// seedScenarioData 创建渠道 / 用户 / Token / Root 用户。
func seedScenarioData(t *testing.T, mockURL string, userQuota int) {
	t.Helper()
	autoBan := 1
	baseURL := mockURL
	mapping := ""
	require.NoError(t, model.DB.Create(&model.Channel{
		Id:           scenarioChannelID,
		Type:         constant.ChannelTypeDoubaoVideo, // 54 = Seedance/DoubaoVideo
		Key:          "test-key",
		Status:       common.ChannelStatusEnabled,
		Name:         "scenario-seedance",
		AutoBan:      &autoBan,
		BaseURL:      &baseURL,
		Models:       scenarioTestModel,
		Group:        "default",
		ModelMapping: &mapping,
	}).Error)
	require.NoError(t, model.DB.Create(&model.User{
		Id:       scenarioUserID,
		Username: "scenario_user",
		Password: "password123",
		Role:     common.RoleCommonUser,
		Status:   common.UserStatusEnabled,
		Quota:    userQuota,
		Group:    "default",
		AffCode:  "scenario-aff-1",
	}).Error)
	// Root 用户：渠道禁用时的通知会查询 role=100
	require.NoError(t, model.DB.Create(&model.User{
		Id:       100,
		Username: "scenario_root",
		Password: "password123",
		Role:     common.RoleRootUser,
		Status:   common.UserStatusEnabled,
		Quota:    0,
		Group:    "default",
		AffCode:  "scenario-aff-100",
	}).Error)
	require.NoError(t, model.DB.Create(&model.Token{
		Id:            1,
		UserId:        scenarioUserID,
		Key:           "sk-scenario-token",
		Status:        common.TokenStatusEnabled,
		Name:          "scenario-token",
		RemainQuota:   scenarioUserQuota,
		Group:         "default",
		ExpiredTime:   -1,
		UnlimitedQuota: false,
	}).Error)
}

// newScenarioCtx 构造等价于中间件设置完毕的 gin 上下文（用户 / Token / 渠道 / 模型）。
func newScenarioCtx(t *testing.T, w *httptest.ResponseRecorder, mockURL, requestBody string) *gin.Context {
	t.Helper()
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/videos", strings.NewReader(requestBody))
	c.Request.Header.Set("Content-Type", "application/json")

	// 用户 / Token 上下文（等价 TokenAuth + SetupContextForToken + UserCache.WriteContext）
	common.SetContextKey(c, constant.ContextKeyUserId, scenarioUserID)
	common.SetContextKey(c, constant.ContextKeyUserName, "scenario_user")
	common.SetContextKey(c, constant.ContextKeyUserGroup, "default")
	common.SetContextKey(c, constant.ContextKeyUsingGroup, "default")
	common.SetContextKey(c, constant.ContextKeyUserQuota, scenarioUserQuota)
	common.SetContextKey(c, constant.ContextKeyUserEmail, "scenario@example.com")
	common.SetContextKey(c, constant.ContextKeyTokenGroup, "default")
	common.SetContextKey(c, constant.ContextKeyTokenId, 1)
	common.SetContextKey(c, constant.ContextKeyTokenKey, "sk-scenario-token")
	common.SetContextKey(c, constant.ContextKeyTokenUnlimited, false)
	common.SetContextKey(c, constant.ContextKeyRequestStartTime, time.Now())
	common.SetContextKey(c, constant.ContextKeyUserSetting, dto.UserSetting{
		AcceptUnsetRatioModel: true, // 测试模型未配置价格，接受未设置倍率
		BillingPreference:     "wallet_only",
	})
	common.SetContextKey(c, constant.ContextKeyOriginalModel, scenarioTestModel)
	c.Set("token_name", "scenario-token")
	c.Set("relay_mode", relayconstant.RelayModeVideoSubmit)

	// 渠道上下文（等价 Distribute → SetupContextForSelectedChannel）
	common.SetContextKey(c, constant.ContextKeyChannelId, scenarioChannelID)
	common.SetContextKey(c, constant.ContextKeyChannelType, constant.ChannelTypeDoubaoVideo)
	common.SetContextKey(c, constant.ContextKeyChannelName, "scenario-seedance")
	common.SetContextKey(c, constant.ContextKeyChannelBaseUrl, mockURL)
	common.SetContextKey(c, constant.ContextKeyChannelKey, "test-key")
	common.SetContextKey(c, constant.ContextKeyChannelAutoBan, true)
	common.SetContextKey(c, constant.ContextKeyChannelSetting, dto.ChannelSettings{})
	common.SetContextKey(c, constant.ContextKeyChannelOtherSetting, dto.ChannelOtherSettings{})
	common.SetContextKey(c, constant.ContextKeyChannelModelMapping, "")
	common.SetContextKey(c, constant.ContextKeyChannelStatusCodeMapping, "")
	common.SetContextKey(c, constant.ContextKeyChannelIsMultiKey, false)
	common.SetContextKey(c, constant.ContextKeyChannelMultiKeyIndex, 0)
	c.Set("original_model", scenarioTestModel)
	return c
}

// captureLogs 将 gin 日志出口切换到缓冲区，返回恢复函数。
func captureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	common.LogWriterMu.Lock()
	oldWriter := gin.DefaultWriter
	oldErrWriter := gin.DefaultErrorWriter
	buf := &bytes.Buffer{}
	gin.DefaultWriter = buf
	gin.DefaultErrorWriter = buf
	common.LogWriterMu.Unlock()
	t.Cleanup(func() {
		common.LogWriterMu.Lock()
		gin.DefaultWriter = oldWriter
		gin.DefaultErrorWriter = oldErrWriter
		common.LogWriterMu.Unlock()
	})
	return buf
}

// runScenario 执行一次完整的 RelayTask 提交。
// mock: 上游 Mock 处理器；requestBody: 客户端请求体；userQuota: 用户初始余额。
func runScenario(t *testing.T, mock http.HandlerFunc, requestBody string, userQuota, retryTimes, relayTimeout int) (*httptest.ResponseRecorder, *mockUpstreamRecorder) {
	t.Helper()
	setupScenarioGlobals(t, retryTimes, relayTimeout)
	setupScenarioDB(t)

	up := newMockUpstream(t, mock)
	seedScenarioData(t, up.server.URL, userQuota)

	w := httptest.NewRecorder()
	c := newScenarioCtx(t, w, up.server.URL, requestBody)

	RelayTask(c)
	return w, up
}

// ---------------------------------------------------------------------------
// Mock 上游：统计 POST 次数与请求头，验证请求确实到达（而非拨号失败）
// ---------------------------------------------------------------------------

type mockUpstreamRecorder struct {
	server       *httptest.Server
	mu           sync.Mutex
	postCount    int
	auths        []string
	clientReqIDs []string
	idemKeys     []string
}

func newMockUpstream(t *testing.T, handler http.HandlerFunc) *mockUpstreamRecorder {
	t.Helper()
	r := &mockUpstreamRecorder{}
	r.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		r.mu.Lock()
		r.auths = append(r.auths, req.Header.Get("Authorization"))
		r.clientReqIDs = append(r.clientReqIDs, req.Header.Get("X-Client-Request-Id"))
		r.idemKeys = append(r.idemKeys, req.Header.Get("Idempotency-Key"))
		if req.Method == http.MethodPost {
			r.postCount++
		}
		r.mu.Unlock()
		handler(w, req)
	}))
	t.Cleanup(r.server.Close)
	return r
}

func (r *mockUpstreamRecorder) snapshot(t *testing.T) (postCount int, auths, clientReqIDs, idemKeys []string) {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.postCount, append([]string(nil), r.auths...), append([]string(nil), r.clientReqIDs...),
		append([]string(nil), r.idemKeys...)
}

// ---------------------------------------------------------------------------
// 测试用例
// ---------------------------------------------------------------------------

const scenarioRequestBody = `{"model":"doubao-seedance-2-0-260128","prompt":"一只柯基在草地上奔跑"}`

// 401：上游返回 401，且错误体包含 Endpoint ID / API Key 形态的敏感内容，
// 用于验证响应与日志的脱敏。
func TestRelayTaskUpstream401(t *testing.T) {
	w, up := runScenario(t, func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprint(w, `{"error":{"message":"Authentication failed for ep-20250101-abc123 with key sk-test-secret"}}`)
	}, scenarioRequestBody, scenarioUserQuota, 2, 0)

	// 客户端：401 + fail_to_fetch_task + 脱敏文案
	require.Equal(t, http.StatusUnauthorized, w.Code, "上游 401 必须透传 HTTP 401")
	body := w.Body.String()
	require.Contains(t, body, `"code":"fail_to_fetch_task"`)
	assert.Contains(t, body, "ep-***", "Endpoint ID 必须脱敏")
	assert.Contains(t, body, "sk-***", "API Key 必须脱敏")
	assert.NotContains(t, body, "ep-20250101-abc123", "不得泄露真实 Endpoint ID")
	assert.NotContains(t, body, "sk-test-secret", "不得泄露真实 API Key")
	assert.NotContains(t, body, "Bearer test-key", "不得泄露 Bearer Token")

	// 上游 POST 次数：明确失败（confirmed_failure），即使配置了重试也不得重复 POST
	postCount, auths, clientReqIDs, idemKeys := up.snapshot(t)
	require.Equal(t, 1, postCount, "401 明确失败不得重复 POST")
	assert.Equal(t, []string{"Bearer test-key"}, auths, "必须携带渠道 API Key")
	assert.NotEmpty(t, clientReqIDs[0], "必须携带 X-Client-Request-Id")
	assert.Empty(t, idemKeys[0], "不得发送未声明支持的 Idempotency-Key")

	// 本地任务记录 / 恢复记录
	var taskCount, recoveryCount int64
	require.NoError(t, model.DB.Model(&model.Task{}).Count(&taskCount).Error)
	require.NoError(t, model.DB.Model(&model.TaskSubmitRecovery{}).Count(&recoveryCount).Error)
	assert.Zero(t, taskCount, "401 失败不得创建本地任务记录")
	assert.Zero(t, recoveryCount, "401 是明确失败，不得生成恢复记录")

	// 用户预扣额度：失败后退款
	require.Eventually(t, func() bool {
		q, err := model.GetUserQuota(scenarioUserID, false)
		return err == nil && q == scenarioUserQuota
	}, 3*time.Second, 20*time.Millisecond, "401 失败后用户预扣额度必须退回")

	// 并发名额：明确失败无任务/无恢复记录 → 释放
	count, err := model.GetRunningCountForUser(scenarioUserID)
	require.NoError(t, err)
	assert.Zero(t, count, "401 明确失败后并发名额必须释放")

	// 渠道按当前配置自动禁用（401 在默认自动禁用状态码范围内）
	require.Eventually(t, func() bool {
		ch, err := model.GetChannelById(scenarioChannelID, true)
		return err == nil && ch.Status == common.ChannelStatusAutoDisabled
	}, 3*time.Second, 20*time.Millisecond, "401 必须触发渠道自动禁用")
}

// 429：上游返回 429 → 客户端 429；不重复 POST；不自动禁用渠道（默认配置仅 401 禁）。
func TestRelayTaskUpstream429(t *testing.T) {
	w, up := runScenario(t, func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		fmt.Fprint(w, `{"error":{"message":"rate limit exceeded"}}`)
	}, scenarioRequestBody, scenarioUserQuota, 2, 0)

	require.Equal(t, http.StatusTooManyRequests, w.Code, "上游 429 必须透传 HTTP 429")
	body := w.Body.String()
	require.Contains(t, body, `"code":"fail_to_fetch_task"`)
	assert.Contains(t, body, "当前分组上游负载已饱和", "429 文案需改写为分组负载提示")

	postCount, _, _, _ := up.snapshot(t)
	require.Equal(t, 1, postCount, "429 明确失败不得重复 POST")

	var taskCount, recoveryCount int64
	require.NoError(t, model.DB.Model(&model.Task{}).Count(&taskCount).Error)
	require.NoError(t, model.DB.Model(&model.TaskSubmitRecovery{}).Count(&recoveryCount).Error)
	assert.Zero(t, taskCount)
	assert.Zero(t, recoveryCount)

	require.Eventually(t, func() bool {
		q, err := model.GetUserQuota(scenarioUserID, false)
		return err == nil && q == scenarioUserQuota
	}, 3*time.Second, 20*time.Millisecond, "429 失败后预扣额度必须退回")

	count, err := model.GetRunningCountForUser(scenarioUserID)
	require.NoError(t, err)
	assert.Zero(t, count, "429 明确失败后并发名额必须释放")

	ch, err := model.GetChannelById(scenarioChannelID, true)
	require.NoError(t, err)
	assert.Equal(t, common.ChannelStatusEnabled, ch.Status, "默认配置下 429 不应自动禁用渠道")
}

// 5xx（500 / 503）：客户端透传 5xx；不重复 POST；不自动禁用。
func TestRelayTaskUpstream5xx(t *testing.T) {
	for _, statusCode := range []int{http.StatusInternalServerError, http.StatusServiceUnavailable} {
		t.Run(fmt.Sprintf("status_%d", statusCode), func(t *testing.T) {
			w, up := runScenario(t, func(w http.ResponseWriter, r *http.Request) {
				io.Copy(io.Discard, r.Body)
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(statusCode)
				fmt.Fprint(w, `{"error":{"message":"upstream internal error"}}`)
			}, scenarioRequestBody, scenarioUserQuota, 2, 0)

			require.Equal(t, statusCode, w.Code, "上游 %d 必须透传", statusCode)
			body := w.Body.String()
			require.Contains(t, body, `"code":"fail_to_fetch_task"`)

			postCount, _, _, _ := up.snapshot(t)
			require.Equal(t, 1, postCount, "%d 明确失败不得重复 POST", statusCode)

			var taskCount, recoveryCount int64
			require.NoError(t, model.DB.Model(&model.Task{}).Count(&taskCount).Error)
			require.NoError(t, model.DB.Model(&model.TaskSubmitRecovery{}).Count(&recoveryCount).Error)
			assert.Zero(t, taskCount)
			assert.Zero(t, recoveryCount)

			require.Eventually(t, func() bool {
				q, err := model.GetUserQuota(scenarioUserID, false)
				return err == nil && q == scenarioUserQuota
			}, 3*time.Second, 20*time.Millisecond, "%d 失败后预扣额度必须退回", statusCode)

			count, err := model.GetRunningCountForUser(scenarioUserID)
			require.NoError(t, err)
			assert.Zero(t, count, "%d 明确失败后并发名额必须释放", statusCode)

			ch, err := model.GetChannelById(scenarioChannelID, true)
			require.NoError(t, err)
			assert.Equal(t, common.ChannelStatusEnabled, ch.Status, "默认配置下 5xx 不应自动禁用渠道")
		})
	}
}

// 上游余额不足（自动禁用关键词）：400 + "Your credit balance is too low"
// → 客户端 400、不重复 POST、渠道按关键词自动禁用。
func TestRelayTaskUpstreamBalanceInsufficient(t *testing.T) {
	w, up := runScenario(t, func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"error":{"message":"Your credit balance is too low"}}`)
	}, scenarioRequestBody, scenarioUserQuota, 2, 0)

	require.Equal(t, http.StatusBadRequest, w.Code, "上游余额不足必须透传上游状态码")
	body := w.Body.String()
	require.Contains(t, body, `"code":"fail_to_fetch_task"`)
	assert.Contains(t, body, "Your credit balance is too low")

	postCount, _, _, _ := up.snapshot(t)
	require.Equal(t, 1, postCount, "明确失败不得重复 POST")

	var taskCount, recoveryCount int64
	require.NoError(t, model.DB.Model(&model.Task{}).Count(&taskCount).Error)
	require.NoError(t, model.DB.Model(&model.TaskSubmitRecovery{}).Count(&recoveryCount).Error)
	assert.Zero(t, taskCount)
	assert.Zero(t, recoveryCount)

	require.Eventually(t, func() bool {
		q, err := model.GetUserQuota(scenarioUserID, false)
		return err == nil && q == scenarioUserQuota
	}, 3*time.Second, 20*time.Millisecond, "上游余额不足失败后预扣额度必须退回")

	count, err := model.GetRunningCountForUser(scenarioUserID)
	require.NoError(t, err)
	assert.Zero(t, count, "明确失败后并发名额必须释放")

	// 关键词 "Your credit balance is too low" 在 AutomaticDisableKeywords 中 → 自动禁用
	require.Eventually(t, func() bool {
		ch, err := model.GetChannelById(scenarioChannelID, true)
		return err == nil && ch.Status == common.ChannelStatusAutoDisabled
	}, 3*time.Second, 20*time.Millisecond, "余额不足关键词必须触发渠道自动禁用")
}

// 请求发送后响应超时：模拟"请求已发出、但未及时取得响应"。
// 必须停止自动 POST，返回 502 task_submit_outcome_unknown，生成恢复记录并持有名额。
func TestRelayTaskUpstreamTimeout(t *testing.T) {
	// 1s 客户端超时；Mock 读完整请求体后挂起（请求已送达），直到客户端断开。
	w, up := runScenario(t, func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body) // 证明请求体已完整送达
		<-r.Context().Done()        // 客户端 1s 超时断开后返回
	}, scenarioRequestBody, scenarioUserQuota, 2, 1)

	require.Equal(t, http.StatusBadGateway, w.Code, "响应超时必须返回 502")
	body := w.Body.String()
	require.Contains(t, body, `"code":"task_submit_outcome_unknown"`)
	assert.Contains(t, body, "结果未知", "必须告知客户端结果未知")

	// 请求确实到达上游（证明不是拨号失败），且不得重复 POST
	postCount, _, _, _ := up.snapshot(t)
	require.Equal(t, 1, postCount, "结果未知必须停止自动重试，不得重复 POST")

	// 不得创建本地任务记录（结果未知，无任务行）
	var taskCount int64
	require.NoError(t, model.DB.Model(&model.Task{}).Count(&taskCount).Error)
	assert.Zero(t, taskCount, "结果未知时不得创建本地任务记录")

	// 必须生成 outcome_unknown 恢复记录，且持有并发名额
	var recs []model.TaskSubmitRecovery
	require.NoError(t, model.DB.Find(&recs).Error)
	require.Len(t, recs, 1, "结果未知必须生成恢复记录")
	assert.Equal(t, "outcome_unknown", recs[0].Outcome)
	assert.Equal(t, model.TaskRecoveryStatusUnknown, recs[0].Status)
	assert.True(t, recs[0].ConcurrencyReserved, "结果未知恢复记录必须持有并发名额")
	assert.Equal(t, constant.ChannelTypeDoubaoVideo, recs[0].ChannelType)
	assert.Contains(t, recs[0].ChannelBaseURL, "127.0.0.1", "恢复记录保存渠道地址供后续恢复")
	assert.NotEmpty(t, recs[0].IdempotencyKey, "恢复记录必须保存幂等键")
	assert.NotEmpty(t, recs[0].ClientRequestID, "恢复记录必须保存请求串联 ID")
	assert.Equal(t, "outcome_unknown: timeout", recs[0].Note, "恢复记录备注只写错误分类，不写错误原文")
	assert.NotContains(t, recs[0].Note, "test-key", "恢复记录不得泄露凭据")

	// 用户预扣额度：结果未知同样退款（未创建任务，钱不能滞留）
	require.Eventually(t, func() bool {
		q, err := model.GetUserQuota(scenarioUserID, false)
		return err == nil && q == scenarioUserQuota
	}, 3*time.Second, 20*time.Millisecond, "结果未知后预扣额度必须退回")

	// 并发名额：由恢复记录继续持有（上游可能已创建任务，名额不能随请求释放）
	count, err := model.GetRunningCountForUser(scenarioUserID)
	require.NoError(t, err)
	assert.Equal(t, 1, count, "结果未知时并发名额必须由恢复记录持有")

	// 502 不在默认自动禁用范围，不自动禁用渠道
	ch, err := model.GetChannelById(scenarioChannelID, true)
	require.NoError(t, err)
	assert.Equal(t, common.ChannelStatusEnabled, ch.Status)
}

// 本地用户余额不足：必须在调用上游前拒绝，Mock 收到的 POST 数必须为 0。
func TestRelayTaskLocalBalanceInsufficient(t *testing.T) {
	reached := false
	w, up := runScenario(t, func(w http.ResponseWriter, r *http.Request) {
		reached = true
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{"id":"cgt-should-not-reach"}`)
	}, scenarioRequestBody, 0 /* 用户余额为 0 */, 0, 0)

	require.Equal(t, http.StatusForbidden, w.Code, "本地余额不足必须 403")
	body := w.Body.String()
	require.Contains(t, body, `"code":"insufficient_user_quota"`)
	assert.Contains(t, body, "额度不足")

	assert.False(t, reached, "本地余额不足时不得调用上游")
	postCount, _, _, _ := up.snapshot(t)
	require.Zero(t, postCount, "本地余额不足时上游 POST 数必须为 0")

	var taskCount, recoveryCount int64
	require.NoError(t, model.DB.Model(&model.Task{}).Count(&taskCount).Error)
	require.NoError(t, model.DB.Model(&model.TaskSubmitRecovery{}).Count(&recoveryCount).Error)
	assert.Zero(t, taskCount)
	assert.Zero(t, recoveryCount)

	q, err := model.GetUserQuota(scenarioUserID, false)
	require.NoError(t, err)
	assert.Zero(t, q, "本地余额不足时未发生预扣")

	count, err := model.GetRunningCountForUser(scenarioUserID)
	require.NoError(t, err)
	assert.Zero(t, count, "本地余额不足时并发名额必须释放")

	ch, err := model.GetChannelById(scenarioChannelID, true)
	require.NoError(t, err)
	assert.Equal(t, common.ChannelStatusEnabled, ch.Status, "本地余额不足不涉及渠道健康，不得禁用")
}

// 成功控制组：验证测试链路确实能到达上游并创建任务（证明失败场景的断言非空转）。
func TestRelayTaskUpstreamSuccessControl(t *testing.T) {
	logBuf := captureLogs(t)
	w, up := runScenario(t, func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"cgt-test-0001"}`)
	}, scenarioRequestBody, scenarioUserQuota, 2, 0)

	require.Equal(t, http.StatusOK, w.Code, "成功路径应返回 200")

	postCount, auths, clientReqIDs, idemKeys := up.snapshot(t)
	require.Equal(t, 1, postCount)
	assert.Equal(t, []string{"Bearer test-key"}, auths)
	assert.NotEmpty(t, clientReqIDs[0], "创建请求必须携带 X-Client-Request-Id")
	assert.Empty(t, idemKeys[0], "不得发送 Idempotency-Key（上游未声明支持）")

	var taskCount, recoveryCount int64
	require.NoError(t, model.DB.Model(&model.Task{}).Count(&taskCount).Error)
	require.NoError(t, model.DB.Model(&model.TaskSubmitRecovery{}).Count(&recoveryCount).Error)
	assert.EqualValues(t, 1, taskCount, "成功路径必须创建本地任务记录")
	assert.Zero(t, recoveryCount, "成功路径不得生成恢复记录")

	// 成功路径：预扣额即结算额（delta=0），余额减少一个任务额度
	q, err := model.GetUserQuota(scenarioUserID, false)
	require.NoError(t, err)
	assert.Less(t, q, scenarioUserQuota, "成功路径应扣减额度")

	// 并发名额：由任务持有（任务生命周期释放，而非请求结束释放）
	count, err := model.GetRunningCountForUser(scenarioUserID)
	require.NoError(t, err)
	assert.Equal(t, 1, count, "成功任务持有并发名额")

	// 日志包含状态、重试次数与请求串联信息，且不泄露凭据
	logs := logBuf.String()
	assert.Contains(t, logs, "task submit finished", "汇总日志必须包含任务提交结果")
	assert.Contains(t, logs, "idempotency_key=", "日志必须包含幂等键")
	assert.Contains(t, logs, "client_request_id=", "日志必须包含请求串联 ID")
	assert.Contains(t, logs, "retries=0", "日志必须包含重试次数")
	assert.Contains(t, logs, "status_code=200", "日志必须包含响应状态码")
	assert.NotContains(t, logs, "test-key", "日志不得泄露渠道 API Key")
	assert.NotContains(t, logs, "sk-scenario-token", "日志不得泄露用户 Token")
	assert.NotContains(t, logs, "Bearer test-key", "日志不得泄露 Bearer Token")
}

// 日志断言：失败路径同样包含状态、重试次数与请求串联信息，且不泄露敏感内容。
func TestRelayTaskFailureLogCorrelation(t *testing.T) {
	logBuf := captureLogs(t)
	runScenario(t, func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprint(w, `{"error":{"message":"bad key sk-test-secret ep-20250101-abc123"}}`)
	}, scenarioRequestBody, scenarioUserQuota, 2, 0)

	logs := logBuf.String()
	assert.Contains(t, logs, "task submit finished")
	assert.Contains(t, logs, "idempotency_key=")
	assert.Contains(t, logs, "client_request_id=")
	assert.Contains(t, logs, "retries=0", "401 明确失败不得重试")
	assert.Contains(t, logs, "status_code=401")
	assert.NotContains(t, logs, "sk-test-secret", "日志不得泄露 API Key")
	assert.NotContains(t, logs, "ep-20250101-abc123", "日志不得泄露 Endpoint ID")
	assert.NotContains(t, logs, "Bearer test-key", "日志不得泄露 Bearer Token")
}
