package relay

import (
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	relaycommon "github.com/QuantumNous/new-api/relay/common"

	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// 在测试文件内初始化内存 SQLite（仅本文件相关测试使用，结束后恢复原 DB）。
func initRecoveryTestDB(t *testing.T) {
	t.Helper()
	origDB := model.DB
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	model.DB = db
	t.Cleanup(func() { model.DB = origDB })
	require.NoError(t, db.AutoMigrate(&model.TaskSubmitRecovery{}, &model.Task{}, &model.Channel{}))
}

func newRecoveryCtx(t *testing.T, userID int, body string) *gin.Context {
	t.Helper()
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/videos", strings.NewReader(body))
	c.Set(string(constant.ContextKeyUserId), userID)
	c.Set("channel_type", 54)
	return c
}

func newRecoveryInfo(userID int, baseURL string) *relaycommon.RelayInfo {
	return &relaycommon.RelayInfo{
		UserId:          userID,
		OriginModelName: "doubao-seedance-2-0-260128",
		ChannelMeta: &relaycommon.ChannelMeta{
			ChannelId:      1,
			ChannelType:    54,
			ChannelBaseUrl: baseURL,
			ApiKey:         "test-key",
		},
		TaskRelayInfo: &relaycommon.TaskRelayInfo{
			PublicTaskID:    "task_public123",
			IdempotencyKey:  "11111111-1111-4111-8111-111111111111",
			ClientRequestID: "22222222-2222-4222-8222-222222222222",
		},
	}
}

// ---------------------------------------------------------------------------
// 候选发现：唯一候选 → inferred（推测关联，不自动关联）；
// 多个候选 → ambiguous（不自动选择）；0 个候选 → unknown。
// ---------------------------------------------------------------------------

func TestDiscoverRecoveryCandidates(t *testing.T) {
	initRecoveryTestDB(t)

	// 模拟 Seedance 查询列表接口：按 filter.model 返回任务项
	var mu sync.Mutex
	var receivedModels []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v3/contents/generations/tasks" {
			http.NotFound(w, r)
			return
		}
		mu.Lock()
		receivedModels = append(receivedModels, r.URL.Query().Get("filter.model"))
		mu.Unlock()
		model := r.URL.Query().Get("filter.model")
		w.Header().Set("Content-Type", "application/json")
		switch model {
		case "m-unique":
			fmt.Fprint(w, `{"items":[{"id":"cgt-only","model":"m-unique","status":"succeeded","created_at":1718049470,
				"content":[{"type":"image_url","image_url":{"url":"https://x/a.png"}},{"type":"text","text":"一只柯基在草地上奔跑"}]}],"total":1}`)
		case "m-multi":
			fmt.Fprint(w, `{"items":[
				{"id":"cgt-1","model":"m-multi","status":"succeeded","created_at":1718049470,
				 "content":[{"type":"text","text":"重复的提示词"}]},
				{"id":"cgt-2","model":"m-multi","status":"queued","created_at":1718049471,
				 "content":[{"type":"text","text":"重复的提示词"}]}],"total":2}`)
		default:
			fmt.Fprint(w, `{"items":[],"total":0}`)
		}
	}))
	defer srv.Close()

	baseURL := strings.TrimSuffix(srv.URL, "/")
	ch := &model.Channel{Type: 54, Key: "k", Status: 1, Name: "test", BaseURL: &baseURL}
	require.NoError(t, model.DB.Create(ch).Error)

	c := newRecoveryCtx(t, 1, `{}`)

	t.Run("unique candidate infers but does not associate", func(t *testing.T) {
		rec := &model.TaskSubmitRecovery{
			UserId:             1,
			Platform:           "54",
			Model:              "m-unique",
			ChannelId:          ch.Id,
			ChannelType:        54,
			ContentFingerprint: relaycommon.SubmitContentFingerprint(relaycommon.TaskSubmitReq{Model: "m-unique", Prompt: "一只柯基在草地上奔跑", Images: []string{"x"}}),
			FirstSubmitTime:    1718049400,
			Outcome:            "outcome_unknown",
			Status:             model.TaskRecoveryStatusUnknown,
		}
		require.NoError(t, rec.Insert())

		updated, err := DiscoverRecoveryCandidates(c, rec)
		require.NoError(t, err)
		assert.Equal(t, model.TaskRecoveryStatusInferred, updated.Status, "唯一候选应标记为推测关联")
		assert.Empty(t, updated.UpstreamTaskID, "推测关联不得自动填写上游 task_id")
		assert.Contains(t, updated.Candidates, "cgt-only")
	})

	t.Run("multiple candidates never auto-selected", func(t *testing.T) {
		rec := &model.TaskSubmitRecovery{
			UserId:             1,
			Platform:           "54",
			Model:              "m-multi",
			ChannelId:          ch.Id,
			ChannelType:        54,
			ContentFingerprint: relaycommon.SubmitContentFingerprint(relaycommon.TaskSubmitReq{Model: "m-multi", Prompt: "重复的提示词"}),
			FirstSubmitTime:    1718049400,
			Outcome:            "outcome_unknown",
			Status:             model.TaskRecoveryStatusUnknown,
		}
		require.NoError(t, rec.Insert())

		updated, err := DiscoverRecoveryCandidates(c, rec)
		require.NoError(t, err)
		assert.Equal(t, model.TaskRecoveryStatusAmbiguous, updated.Status, "多个候选不得自动选择")
		assert.Empty(t, updated.UpstreamTaskID)
		assert.Contains(t, updated.Candidates, "cgt-1")
		assert.Contains(t, updated.Candidates, "cgt-2")
	})

	t.Run("zero candidates stays unknown", func(t *testing.T) {
		rec := &model.TaskSubmitRecovery{
			UserId:             1,
			Platform:           "54",
			Model:              "m-none",
			ChannelId:          ch.Id,
			ChannelType:        54,
			ContentFingerprint: "deadbeef",
			FirstSubmitTime:    1718049400,
			Outcome:            "outcome_unknown",
			Status:             model.TaskRecoveryStatusUnknown,
		}
		require.NoError(t, rec.Insert())

		updated, err := DiscoverRecoveryCandidates(c, rec)
		require.NoError(t, err)
		assert.Equal(t, model.TaskRecoveryStatusUnknown, updated.Status)
	})

	// 候选发现按 model 查询上游
	mu.Lock()
	assert.Contains(t, receivedModels, "m-unique")
	assert.Contains(t, receivedModels, "m-multi")
	mu.Unlock()
}

// 终态（associated/recreated）不允许被 discover 重新打开。
func TestDiscoverRecoveryCandidatesRejectedOnTerminalStates(t *testing.T) {
	initRecoveryTestDB(t)
	c := newRecoveryCtx(t, 1, `{}`)

	for _, status := range []string{model.TaskRecoveryStatusAssociated, model.TaskRecoveryStatusRecreated} {
		rec := &model.TaskSubmitRecovery{
			UserId:     1,
			Model:      "m",
			Outcome:    "outcome_unknown",
			Status:     status,
			UpstreamTaskID: "cgt-x",
		}
		require.NoError(t, rec.Insert())
		_, err := DiscoverRecoveryCandidates(c, rec)
		assert.Error(t, err, "终态 %s 不允许被 discover 重新打开", status)
		assert.Equal(t, status, rec.Status, "discover 失败后不得改变状态")
	}
}

// 模型映射场景：恢复记录存有上游模型名，候选发现用上游模型过滤并可匹配内容指纹。
func TestDiscoverRecoveryCandidatesWithModelMapping(t *testing.T) {
	initRecoveryTestDB(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("filter.model") != "seedance-2.0-mapped" {
			fmt.Fprint(w, `{"items":[],"total":0}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"items":[{"id":"cgt-mapped","model":"seedance-2.0-mapped","status":"queued","created_at":1718049470,
			"content":[{"type":"text","text":"映射后的提示词"}]}],"total":1}`)
	}))
	defer srv.Close()

	baseURL := strings.TrimSuffix(srv.URL, "/")
	ch := &model.Channel{Type: 54, Key: "k", Status: 1, Name: "test", BaseURL: &baseURL}
	require.NoError(t, model.DB.Create(ch).Error)

	c := newRecoveryCtx(t, 1, `{}`)
	// 用户侧模型 = 映射前；上游模型 = 映射后；内容指纹必须用上游模型计算
	rec := &model.TaskSubmitRecovery{
		UserId:             1,
		Platform:           "54",
		Model:              "doubao-seedance-2-0-260128",
		UpstreamModelName:  "seedance-2.0-mapped",
		ChannelId:          ch.Id,
		ChannelType:        54,
		ContentFingerprint: relaycommon.SubmitContentFingerprint(relaycommon.TaskSubmitReq{Model: "seedance-2.0-mapped", Prompt: "映射后的提示词"}),
		FirstSubmitTime:    1718049400,
		Outcome:            "outcome_unknown",
		Status:             model.TaskRecoveryStatusUnknown,
	}
	require.NoError(t, rec.Insert())

	updated, err := DiscoverRecoveryCandidates(c, rec)
	require.NoError(t, err)
	assert.Equal(t, model.TaskRecoveryStatusInferred, updated.Status, "模型映射下应能发现唯一候选")
	assert.Contains(t, updated.Candidates, "cgt-mapped")
}

// 状态机并发不变量：discover 执行上游查询期间被并发 recreate 原子占位，
// discover 的条件更新必须失败并丢弃结果，绝不覆盖 recreated。
func TestDiscoverRecoveryCandidatesDoesNotOverwriteRecreated(t *testing.T) {
	initRecoveryTestDB(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"items":[{"id":"cgt-race","model":"m-race","status":"queued","created_at":1718049470,
			"content":[{"type":"text","text":"并发竞态提示词"}]}],"total":1}`)
	}))
	defer srv.Close()

	baseURL := strings.TrimSuffix(srv.URL, "/")
	ch := &model.Channel{Type: 54, Key: "k", Status: 1, Name: "test", BaseURL: &baseURL}
	require.NoError(t, model.DB.Create(ch).Error)

	c := newRecoveryCtx(t, 1, `{}`)
	rec := &model.TaskSubmitRecovery{
		UserId:             1,
		Platform:           "54",
		Model:              "m-race",
		ChannelId:          ch.Id,
		ChannelType:        54,
		ContentFingerprint: relaycommon.SubmitContentFingerprint(relaycommon.TaskSubmitReq{Model: "m-race", Prompt: "并发竞态提示词"}),
		FirstSubmitTime:    1718049400,
		Outcome:            "outcome_unknown",
		Status:             model.TaskRecoveryStatusUnknown,
	}
	require.NoError(t, rec.Insert())

	// 模拟 discover 已加载旧快照（内存里仍是 unknown）后，并发 recreate 原子占位
	claimed, err := model.MarkRecoveryRecreated(rec.ID, 1, "concurrent recreate claimed")
	require.NoError(t, err)
	require.True(t, claimed)

	// discover 用旧快照继续执行（先查询上游，再条件更新）→ 必须失败且不改状态
	_, err = DiscoverRecoveryCandidates(c, rec)
	assert.Error(t, err, "状态已被并发变更，discover 结果必须丢弃")

	got, err := model.GetTaskSubmitRecoveryByID(rec.ID, 1)
	require.NoError(t, err)
	assert.Equal(t, model.TaskRecoveryStatusRecreated, got.Status, "discover 不得覆盖并发 recreate 写下的状态")
	assert.NotContains(t, got.Candidates, "cgt-race", "被丢弃的发现结果不得写入")
}

// ---------------------------------------------------------------------------
// 已取得 task_id 但本地落库失败 → 用 GET 恢复，绝不再次 POST
// ---------------------------------------------------------------------------

func TestRecoverTaskAfterInsertFailureUsesGETNotPOST(t *testing.T) {
	initRecoveryTestDB(t)

	var mu sync.Mutex
	method := ""
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		method = r.Method + " " + r.URL.Path
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"cgt-seedance-0001","status":"queued","content":{"video_url":""}}`)
	}))
	defer srv.Close()

	c := newRecoveryCtx(t, 1, `{"model":"m","prompt":"p"}`)
	info := newRecoveryInfo(1, srv.URL)
	task := model.InitTask(constant.TaskPlatform("54"), info)
	task.TaskID = "task_public123"

	// 预置同主键行，使任务 Insert 重试必然失败（模拟本地落库失败）
	require.NoError(t, model.DB.Create(&model.Task{ID: 999999, TaskID: "occupied"}).Error)
	task.ID = 999999

	createdReserved := RecoverTaskAfterInsertFailure(c, info, task, "cgt-seedance-0001", errors.New("simulated insert failure"), true)

	// 恢复记录：关联上游 task_id，绝不再次 POST；且持有并发名额（reservedSlot=true）
	require.True(t, createdReserved, "二次落库仍失败且已预留名额时，恢复记录必须持有名额")
	rec, err := model.GetTaskSubmitRecoveryByID(1, 1)
	require.NoError(t, err)
	assert.Equal(t, model.TaskRecoveryStatusAssociated, rec.Status)
	assert.Equal(t, "cgt-seedance-0001", rec.UpstreamTaskID)
	assert.Equal(t, "confirmed_success", rec.Outcome)
	assert.True(t, rec.ConcurrencyReserved, "已取得上游 task_id 的恢复记录必须占用并发名额")
	assert.NotEmpty(t, rec.IdempotencyKey)
	assert.NotEmpty(t, rec.ClientRequestID)

	mu.Lock()
	defer mu.Unlock()
	assert.Contains(t, method, "GET", "恢复必须使用查询接口（GET），绝不能再次 POST")
	assert.NotContains(t, method, "POST", "恢复过程中不得出现任何 POST 创建请求")
}

// 首次 Insert 失败、恢复重试 Insert 成功：任务行实际已落库，名额归任务生命周期，
// 不得创建占名额的恢复记录（返回 false）。
func TestRecoverTaskAfterInsertFailureRetrySucceeds(t *testing.T) {
	initRecoveryTestDB(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"cgt-seedance-0002","status":"queued","content":{"video_url":""}}`)
	}))
	defer srv.Close()

	c := newRecoveryCtx(t, 1, `{"model":"m","prompt":"p"}`)
	info := newRecoveryInfo(1, srv.URL)
	task := model.InitTask(constant.TaskPlatform("54"), info)
	task.TaskID = "task_public123"

	// controller 侧首次 Insert 已失败（模拟），此时任务行不存在；
	// RecoverTaskAfterInsertFailure 内部重试 Insert 应成功（瞬时 DB 错误自愈）
	createdReserved := RecoverTaskAfterInsertFailure(c, info, task, "cgt-seedance-0002", errors.New("simulated transient insert failure"), true)

	assert.False(t, createdReserved, "重试 Insert 成功后任务行已存在，名额归任务，不得创建占名额恢复记录")

	// 任务行确实落库
	_, exist, err := model.GetByTaskId(1, "task_public123")
	require.NoError(t, err)
	assert.True(t, exist, "恢复重试成功必须留下任务行")

	// 不得产生恢复记录
	_, err = model.GetTaskSubmitRecoveryByID(1, 1)
	assert.Error(t, err, "重试成功路径不得创建恢复记录")
}

// ---------------------------------------------------------------------------
// outcome_unknown 持久化：不保存敏感请求内容；人工重试生成新的逻辑尝试记录
// ---------------------------------------------------------------------------

func TestPersistOutcomeUnknownNoSensitiveContent(t *testing.T) {
	initRecoveryTestDB(t)
	c := newRecoveryCtx(t, 1, `{"prompt":"secret-prompt-xyz","model":"m"}`)
	info := newRecoveryInfo(1, "https://ark.example.com")

	fp := "abc:def:" + strings.Repeat("0", 64)
	cfp := relaycommon.SubmitContentFingerprint(relaycommon.TaskSubmitReq{Model: "m", Prompt: "secret-prompt-xyz"})
	rec, err := PersistOutcomeUnknown(c, info, fp, cfp, errors.New("read tcp 1.2.3.4:443: connection reset"), true)
	require.NoError(t, err)

	assert.Equal(t, "outcome_unknown", rec.Outcome)
	assert.Equal(t, model.TaskRecoveryStatusUnknown, rec.Status)
	assert.True(t, rec.ConcurrencyReserved, "outcome_unknown 恢复记录必须持有并发名额（上游可能已创建任务）")
	assert.Equal(t, "11111111-1111-4111-8111-111111111111", rec.IdempotencyKey)
	assert.Equal(t, "22222222-2222-4222-8222-222222222222", rec.ClientRequestID)
	assert.Equal(t, int64(54), int64(rec.ChannelType))
	assert.NotZero(t, rec.FirstSubmitTime)

	// 只落错误分类标识，绝不落错误原文（原文可能回显请求内容）
	assert.Equal(t, "outcome_unknown: conn_reset", rec.Note, "Note 必须为确定性的非敏感分类")
	assert.NotContains(t, rec.Note, "1.2.3.4")
	assert.NotContains(t, rec.Note, "secret-prompt-xyz")
	assert.NotContains(t, rec.Note, "Authorization")
	assert.NotContains(t, rec.Note, "Bearer test-key")
	// 指纹为哈希而非原文（64 位 hex）
	_, err = hex.DecodeString(cfp)
	require.NoError(t, err)
}

func TestPersistOutcomeUnknownChildAttempt(t *testing.T) {
	initRecoveryTestDB(t)
	c := newRecoveryCtx(t, 1, `{"model":"m","prompt":"p"}`)
	info := newRecoveryInfo(1, "https://ark.example.com")

	parent := &model.TaskSubmitRecovery{
		UserId:   1,
		Platform: "54",
		Model:    "m",
		Outcome:  "outcome_unknown",
		Status:   model.TaskRecoveryStatusUnknown,
		Attempt:  1,
	}
	require.NoError(t, parent.Insert())

	// 模拟恢复入口注入：用户在父记录上确认后的人工重试
	info.TaskRelayInfo.RecoveryParentID = parent.ID
	child, err := PersistOutcomeUnknown(c, info, "fp", "cfp", errors.New("timeout"), false)
	require.NoError(t, err)
	assert.Equal(t, parent.ID, child.ParentID, "人工重试必须建立父子关联")
	assert.Equal(t, parent.Attempt+1, child.Attempt, "人工重试必须生成新的逻辑尝试记录（Attempt 递增）")

	// 父记录状态在控制器层标记为 recreated（此处验证查询可用）
	got, err := model.GetTaskSubmitRecoveryByID(parent.ID, 1)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, model.TaskRecoveryStatusUnknown, got.Status)
}

// 日志/存储审计：恢复记录 JSON 序列化不含请求体字段。
func TestRecoveryRecordHasNoRequestBodyField(t *testing.T) {
	initRecoveryTestDB(t)
	rec := &model.TaskSubmitRecovery{
		UserId:        1,
		IdempotencyKey: "k",
		Fingerprint:   "fp",
	}
	b, err := common.Marshal(rec)
	require.NoError(t, err)
	assert.NotContains(t, string(b), "prompt")
	assert.NotContains(t, string(b), "api_key")
	assert.NotContains(t, string(b), "Authorization")
}
