package router

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/i18n"
	"github.com/QuantumNous/new-api/middleware"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/setting"
	"github.com/alicebob/miniredis/v2"
	"github.com/gin-gonic/gin"
	"github.com/go-redis/redis/v8"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Seedance 创建请求的最小合法 body：Distribute 需要从其中解析 model 才能走到
// 渠道分配阶段（无可用渠道时以 503 结束），从而证明限流放行后的行为。
const videoRouterGenerationBody = `{"model":"doubao-seedance-2.0"}`

var (
	videoRouterMiniRedisServer *miniredis.Miniredis
	videoRouterMiniRedisClient *redis.Client
)

// TestMain 为视频路由限流测试提供共享的 Redis 实例。
//
// middleware.ModelRequestRateLimit() 的 Redis 分支通过 common/limiter.New 的
// sync.Once 单例（绑定首个 client）执行令牌桶检查，因此全部测试必须复用同一个
// client，否则后续测试会命中已关闭的连接。限流 key 均按用户 ID 派生，各测试
// 使用唯一用户，天然隔离，不依赖测试执行顺序。
func TestMain(m *testing.M) {
	server, err := miniredis.Run()
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to start miniredis: %v\n", err)
		os.Exit(1)
	}
	// 生产环境由 main 初始化；测试进程需显式加载翻译文件，
	// 否则 Distribute 无可用渠道时 abortWithOpenAiMessage 的 i18n 调用会 panic。
	if err := i18n.Init(); err != nil {
		fmt.Fprintf(os.Stderr, "failed to init i18n: %v\n", err)
		os.Exit(1)
	}
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	if err := client.Ping(context.Background()).Err(); err != nil {
		fmt.Fprintf(os.Stderr, "failed to ping miniredis: %v\n", err)
		os.Exit(1)
	}
	videoRouterMiniRedisServer = server
	videoRouterMiniRedisClient = client

	code := m.Run()

	_ = client.Close()
	server.Close()
	os.Exit(code)
}

// enableVideoRouterRedis 显式切到 Redis 限流模式并在 Cleanup 中恢复缓存状态。
// 各测试使用独立内存 SQLite，用户 ID 都从 1 递增，共享 Redis 中按 ID 派生的
// key（rateLimit:<id>、user:<id>、token 缓存）可能跨测试重复；Cleanup 时
// FlushDB 保证下一测试从干净状态开始，不依赖测试执行顺序。
func enableVideoRouterRedis(t *testing.T) {
	t.Helper()
	previousRedisEnabled := common.RedisEnabled
	previousRDB := common.RDB
	common.RedisEnabled = true
	common.RDB = videoRouterMiniRedisClient
	t.Cleanup(func() {
		_ = videoRouterMiniRedisClient.FlushDB(context.Background()).Err()
		common.RedisEnabled = previousRedisEnabled
		common.RDB = previousRDB
	})
}

// setModelRequestRateLimitSettings 覆盖模型请求限流的全局配置并在 Cleanup 中恢复。
func setModelRequestRateLimitSettings(t *testing.T, enabled bool, count, successCount, durationMinutes int, groupLimits map[string][2]int) {
	t.Helper()
	previousEnabled := setting.ModelRequestRateLimitEnabled
	previousCount := setting.ModelRequestRateLimitCount
	previousSuccessCount := setting.ModelRequestRateLimitSuccessCount
	previousDurationMinutes := setting.ModelRequestRateLimitDurationMinutes
	previousGroupLimits := setting.ModelRequestRateLimitGroup

	setting.ModelRequestRateLimitEnabled = enabled
	setting.ModelRequestRateLimitCount = count
	setting.ModelRequestRateLimitSuccessCount = successCount
	setting.ModelRequestRateLimitDurationMinutes = durationMinutes
	setting.ModelRequestRateLimitGroup = groupLimits

	t.Cleanup(func() {
		setting.ModelRequestRateLimitEnabled = previousEnabled
		setting.ModelRequestRateLimitCount = previousCount
		setting.ModelRequestRateLimitSuccessCount = previousSuccessCount
		setting.ModelRequestRateLimitDurationMinutes = previousDurationMinutes
		setting.ModelRequestRateLimitGroup = previousGroupLimits
	})
}

type videoRouterTestIdentity struct {
	User  model.User
	Token model.Token
}

// createVideoRouterTestIdentity 创建唯一用户与令牌（每个测试独立内存 DB）。
// 注意：不同测试的 SQLite 用户 ID 都从 1 递增，因此 token key 直接采用
// 不含 "-" 的 username 保证全局唯一（TokenAuth 会把 key 按 "-" 切分取第一段，
// 且 "videorlkey<userId>" 这类按 ID 派生的 key 会在跨测试时重复）。
func createVideoRouterTestIdentity(t *testing.T, username, userGroup, tokenGroup string) videoRouterTestIdentity {
	t.Helper()
	user := model.User{
		Username: username,
		Status:   common.UserStatusEnabled,
		Group:    userGroup,
		Quota:    100,
		// aff_code 带 uniqueIndex，SQLite 中多个空串会互相冲突，必须逐用户唯一。
		AffCode: "videorlaff" + username,
	}
	require.NoError(t, model.DB.Create(&user).Error)
	token := model.Token{
		UserId:         user.Id,
		Key:            username,
		Status:         common.TokenStatusEnabled,
		ExpiredTime:    -1,
		UnlimitedQuota: true,
		Group:          tokenGroup,
	}
	require.NoError(t, model.DB.Create(&token).Error)
	return videoRouterTestIdentity{User: user, Token: token}
}

func performVideoRouterRequest(t *testing.T, engine *gin.Engine, method, path, tokenKey, body string) *httptest.ResponseRecorder {
	t.Helper()
	var request *http.Request
	if body != "" {
		request = httptest.NewRequest(method, path, strings.NewReader(body))
		request.Header.Set("Content-Type", "application/json")
	} else {
		request = httptest.NewRequest(method, path, nil)
	}
	if tokenKey != "" {
		request.Header.Set("Authorization", "Bearer "+tokenKey)
	}
	recorder := httptest.NewRecorder()
	engine.ServeHTTP(recorder, request)
	return recorder
}

// TestVideoGenerationsPOSTIsModelRateLimitedPerUser 验证按用户计数：
// 同一用户达到总请求阈值后返回 429，且其它用户不受影响。
func TestVideoGenerationsPOSTIsModelRateLimitedPerUser(t *testing.T) {
	setupRelayRouterTestDB(t)
	enableVideoRouterRedis(t)
	// 窗口内总请求数 = 1（分组无覆盖）。
	setModelRequestRateLimitSettings(t, true, 1, 1000, 1, nil)

	userA := createVideoRouterTestIdentity(t, "videorlperusera", "default", "")
	userB := createVideoRouterTestIdentity(t, "videorlperuserb", "default", "")

	engine := gin.New()
	SetVideoRouter(engine)

	// 未认证请求在限流之前被 TokenAuth 拒绝（401），证明顺序为
	// TokenAuth → ModelRequestRateLimit。
	unauthenticated := performVideoRouterRequest(t, engine, http.MethodPost, "/v1/video/generations", "", videoRouterGenerationBody)
	assert.Equal(t, http.StatusUnauthorized, unauthenticated.Code)

	// 用户 A 第一笔提交通过限流（无渠道时由 Distribute 以 503 结束，而非 429）。
	first := performVideoRouterRequest(t, engine, http.MethodPost, "/v1/video/generations", userA.Token.Key, videoRouterGenerationBody)
	assert.NotEqual(t, http.StatusTooManyRequests, first.Code, "第一笔提交不应被限流")
	// 第二笔在渠道分配之前被限流拦截（429 而非 503），证明限流位于 Distribute 之前。
	second := performVideoRouterRequest(t, engine, http.MethodPost, "/v1/video/generations", userA.Token.Key, videoRouterGenerationBody)
	assert.Equal(t, http.StatusTooManyRequests, second.Code, "同一用户达到阈值后应返回 429")

	// 用户 B 不受用户 A 计数影响，仍可提交。
	userBFirst := performVideoRouterRequest(t, engine, http.MethodPost, "/v1/video/generations", userB.Token.Key, videoRouterGenerationBody)
	assert.NotEqual(t, http.StatusTooManyRequests, userBFirst.Code, "限流应按用户 ID 隔离计数")
}

// TestVideoGenerationsPOSTGroupLimitOverridesGlobal 验证分组覆盖：
// vip 分组的覆盖配置（总请求数=1）优先于全局默认（总请求数=100），
// 且限流读取 Token 所属分组（ContextKeyTokenGroup）而非回退到用户分组。
func TestVideoGenerationsPOSTGroupLimitOverridesGlobal(t *testing.T) {
	setupRelayRouterTestDB(t)
	enableVideoRouterRedis(t)
	// 全局默认总请求数很高（100），vip 分组通过覆盖配置收紧到 1。
	setModelRequestRateLimitSettings(t, true, 100, 1000, 1, map[string][2]int{"vip": {1, 1000}})

	// 用户分组均为 default：defaultToken 不设分组（ContextKeyTokenGroup 为空，
	// 回退用户分组 "default"），无覆盖配置走全局默认；vipToken 命中分组覆盖，
	// 以证明生效的是 Token 分组而非回退到的用户分组。
	defaultIdentity := createVideoRouterTestIdentity(t, "videorlgroupdefault", "default", "")
	vipIdentity := createVideoRouterTestIdentity(t, "videorlgroupvip", "default", "vip")

	engine := gin.New()
	SetVideoRouter(engine)

	// default 分组无覆盖配置，连续提交都应使用全局默认阈值（不 429）。
	for i := 0; i < 3; i++ {
		resp := performVideoRouterRequest(t, engine, http.MethodPost, "/v1/video/generations", defaultIdentity.Token.Key, videoRouterGenerationBody)
		assert.NotEqual(t, http.StatusTooManyRequests, resp.Code, "无分组覆盖时应使用全局默认阈值")
	}

	// vip 分组第一笔通过，第二笔被覆盖配置限流。
	firstVIP := performVideoRouterRequest(t, engine, http.MethodPost, "/v1/video/generations", vipIdentity.Token.Key, videoRouterGenerationBody)
	assert.NotEqual(t, http.StatusTooManyRequests, firstVIP.Code, "分组覆盖第一笔不应被限流")
	secondVIP := performVideoRouterRequest(t, engine, http.MethodPost, "/v1/video/generations", vipIdentity.Token.Key, videoRouterGenerationBody)
	assert.Equal(t, http.StatusTooManyRequests, secondVIP.Code, "Token 所属分组的覆盖配置应优先于全局默认阈值")
}

// TestVideoGenerationsGETPollingDoesNotConsumeQuota 验证轮询不计数：
// GET /v1/video/generations/:task_id 多次轮询不消耗也不触发生成请求限流。
func TestVideoGenerationsGETPollingDoesNotConsumeQuota(t *testing.T) {
	setupRelayRouterTestDB(t)
	enableVideoRouterRedis(t)
	setModelRequestRateLimitSettings(t, true, 1, 1000, 1, nil)

	user := createVideoRouterTestIdentity(t, "videorlpoll", "default", "")

	engine := gin.New()
	SetVideoRouter(engine)

	// 高频轮询：连续 GET 均不应被限流拦截（非 429）。
	for i := 0; i < 5; i++ {
		resp := performVideoRouterRequest(t, engine, http.MethodGet, "/v1/video/generations/task_abc", user.Token.Key, "")
		assert.NotEqual(t, http.StatusTooManyRequests, resp.Code, "GET 轮询不应触发生成请求限流")
	}

	// 轮询之后，第一笔 POST 仍应通过限流、第二笔才被限流，
	// 证明 GET 没有消耗 POST 的生成请求额度。
	first := performVideoRouterRequest(t, engine, http.MethodPost, "/v1/video/generations", user.Token.Key, videoRouterGenerationBody)
	assert.NotEqual(t, http.StatusTooManyRequests, first.Code, "轮询不应消耗生成请求额度")
	second := performVideoRouterRequest(t, engine, http.MethodPost, "/v1/video/generations", user.Token.Key, videoRouterGenerationBody)
	assert.Equal(t, http.StatusTooManyRequests, second.Code)
}

// TestVideoGenerationsPOSTUnchangedWhenRateLimitDisabled 验证开关关闭后行为不变：
// 即使把总/成功阈值都压到 1，只要 ModelRequestRateLimitEnabled=false，
// 连续提交都不会被 429 拦截，响应与未挂载限流时保持一致。
func TestVideoGenerationsPOSTUnchangedWhenRateLimitDisabled(t *testing.T) {
	setupRelayRouterTestDB(t)
	enableVideoRouterRedis(t)
	setModelRequestRateLimitSettings(t, false, 1, 1, 1, nil)

	user := createVideoRouterTestIdentity(t, "videorldisabled", "default", "")

	engine := gin.New()
	SetVideoRouter(engine)

	var previousBody string
	for i := 0; i < 3; i++ {
		resp := performVideoRouterRequest(t, engine, http.MethodPost, "/v1/video/generations", user.Token.Key, videoRouterGenerationBody)
		assert.NotEqual(t, http.StatusTooManyRequests, resp.Code, "关闭限流后不应返回 429")
		if i == 0 {
			previousBody = resp.Body.String()
		} else {
			assert.Equal(t, previousBody, resp.Body.String(), "关闭限流后多次提交的响应应保持一致")
		}
	}
}

// videoRouterMemoryUserCounter 提供进程级递增的用户 ID：inMemoryRateLimiter 是
// middleware 包内全局共享状态且无法从本包重置，递增 ID 保证内存限流 key
// （MRRL<userId> / MRRLS<userId>）跨测试、跨轮次（-count=N 重跑）均唯一，
// 不依赖测试执行顺序。
var videoRouterMemoryUserCounter int

// TestVideoGenerationsPOSTIsModelRateLimitedInMemoryMode 覆盖内存限流模式：
// ModelRequestRateLimit 在 Redis 关闭时走内存限流器，行为与 Redis 模式一致
// （阈值=1 时第二笔 429）。路由装配（TokenAuth → ModelRequestRateLimit →
// Distribute → RelayTask）已由 Redis 模式测试通过真实 SetVideoRouter 验证，
// 本测试用等价中间件链聚焦内存限流路径本身。
func TestVideoGenerationsPOSTIsModelRateLimitedInMemoryMode(t *testing.T) {
	previousRedisEnabled := common.RedisEnabled
	common.RedisEnabled = false
	t.Cleanup(func() { common.RedisEnabled = previousRedisEnabled })

	setModelRequestRateLimitSettings(t, true, 1, 1000, 1, nil)

	videoRouterMemoryUserCounter++
	userID := videoRouterMemoryUserCounter

	engine := gin.New()
	engine.POST("/v1/video/generations",
		func(c *gin.Context) { c.Set("id", userID) }, // 等价 TokenAuth 设置用户 ID
		middleware.ModelRequestRateLimit(),
		func(c *gin.Context) { c.Status(http.StatusNoContent) }, // 等价 RelayTask 完成
	)

	first := performVideoRouterRequest(t, engine, http.MethodPost, "/v1/video/generations", "", "")
	assert.Equal(t, http.StatusNoContent, first.Code, "内存模式第一笔提交不应被限流")
	second := performVideoRouterRequest(t, engine, http.MethodPost, "/v1/video/generations", "", "")
	assert.Equal(t, http.StatusTooManyRequests, second.Code, "内存模式达到阈值后应返回 429")
}
