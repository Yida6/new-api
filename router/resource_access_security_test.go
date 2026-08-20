package router

// 资源访问安全回归测试：
//   1. /mj/image/:id（含 /:mode/mj 变体）禁止匿名访问，必须返回 401；
//   2. 已登录但非资源所有者访问必须被拒绝（统一 404）；
//   3. 所有者正常访问成功；
//   4. Seedance 任务 fetch 响应（通用格式与 OpenAI Video 格式）不得包含
//      可绕过本地鉴权的上游直链，result_url / metadata.url 必须指向
//      本服务鉴权代理入口；
//   5. /v1/videos/:task_id/content 代理响应头不得泄露上游地址/重定向头。

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting/system_setting"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	upstreamSeedanceHost = "https://ark.cn-beijing.volces.com"
	upstreamSeedanceURL  = upstreamSeedanceHost + "/api/v3/contents/generations/tasks/up-1/content?v=secret"
	seedanceTaskDataJSON = `{"id":"up-1","model":"seedance-2.0","status":"succeeded","content":{"video_url":"` +
		upstreamSeedanceURL + `"},"usage":{"total_tokens":100}}`
)

// setupResourceSecurityTestEnv 建立路由级安全测试环境：
//   - 用户 A（资源所有者）与用户 B（无关用户）及各自 API 令牌；
//   - A 拥有的 Midjourney 任务与 Seedance 视频任务（含上游直链 result_url/data）；
//   - 装配 relay + video 路由。
func setupResourceSecurityTestEnv(t *testing.T) (engine *gin.Engine, mjID string, videoTaskID string) {
	t.Helper()
	setupRelayRouterTestDB(t)
	require.NoError(t, model.DB.AutoMigrate(&model.Midjourney{}, &model.Task{}, &model.Channel{}))

	userA := model.User{Username: "sec-owner", Status: common.UserStatusEnabled, Group: "default", Quota: 100, AffCode: "affowner"}
	require.NoError(t, model.DB.Create(&userA).Error)
	userB := model.User{Username: "sec-other", Status: common.UserStatusEnabled, Group: "default", Quota: 100, AffCode: "affother"}
	require.NoError(t, model.DB.Create(&userB).Error)

	require.NoError(t, model.DB.Create(&model.Token{UserId: userA.Id, Key: "resownerkey", Status: common.TokenStatusEnabled, ExpiredTime: -1, UnlimitedQuota: true}).Error)
	require.NoError(t, model.DB.Create(&model.Token{UserId: userB.Id, Key: "resotherkey", Status: common.TokenStatusEnabled, ExpiredTime: -1, UnlimitedQuota: true}).Error)

	channel := model.Channel{Type: constant.ChannelTypeDoubaoVideo, Key: "upstream-key", Status: common.ChannelStatusEnabled, Name: "test-seedance", Group: "default"}
	require.NoError(t, model.DB.Create(&channel).Error)

	mjTask := model.Midjourney{UserId: userA.Id, MjId: "mj-sec-owner-0001", Action: "IMAGINE", Status: "SUCCESS", Progress: "100%", ImageUrl: "https://upstream.example.com/img/mj-sec-owner-0001.png", ChannelId: channel.Id}
	require.NoError(t, model.DB.Create(&mjTask).Error)

	videoTask := model.Task{
		TaskID:    "task_seedance_owner_1",
		Platform:  constant.TaskPlatform(strconv.Itoa(constant.ChannelTypeDoubaoVideo)),
		UserId:    userA.Id,
		Group:     "default",
		ChannelId: channel.Id,
		Quota:     100,
		Action:    "generate",
		Status:    model.TaskStatusSuccess,
		Progress:  "100%",
		Properties: model.Properties{
			OriginModelName: "doubao-seedance-2-0-260128",
		},
		PrivateData: model.TaskPrivateData{
			UpstreamTaskID: "up-1",
			ResultURL:      upstreamSeedanceURL,
		},
		Data: json.RawMessage(seedanceTaskDataJSON),
	}
	require.NoError(t, model.DB.Create(&videoTask).Error)

	gin.SetMode(gin.TestMode)
	engine = gin.New()
	SetRelayRouter(engine)
	SetVideoRouter(engine)
	return engine, mjTask.MjId, videoTask.TaskID
}

// allowLoopbackFetchForTest 放行本机回环地址抓取（仅测试内生效，结束后恢复）。
// 默认 SSRF 防护拒绝私有 IP，本地 httptest 上游无法访问。
func allowLoopbackFetchForTest(t *testing.T) {
	t.Helper()
	fetchSetting := system_setting.GetFetchSetting()
	prevSSRF := fetchSetting.EnableSSRFProtection
	fetchSetting.EnableSSRFProtection = false
	t.Cleanup(func() { fetchSetting.EnableSSRFProtection = prevSSRF })
}

// ── 1. /mj/image/:id 匿名访问必须被拒绝 ──────────────────────────────────

func TestMjImageRouteRejectsAnonymousAccess(t *testing.T) {
	engine, mjID, _ := setupResourceSecurityTestEnv(t)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/mj/image/"+mjID, nil)
	engine.ServeHTTP(rec, req)
	require.Equal(t, http.StatusUnauthorized, rec.Code,
		"匿名访问 /mj/image/:id 必须返回 401，实际=%d body=%s", rec.Code, rec.Body.String())

	// /:mode/mj 变体（channel 模式前缀）同样受保护
	rec2 := httptest.NewRecorder()
	req2 := httptest.NewRequest(http.MethodGet, "/mj-proxy/mj/image/"+mjID, nil)
	engine.ServeHTTP(rec2, req2)
	require.Equal(t, http.StatusUnauthorized, rec2.Code,
		"匿名访问 /:mode/mj/image/:id 必须返回 401，实际=%d body=%s", rec2.Code, rec2.Body.String())
}

// ── 2. /mj/image/:id 越权访问必须被拒绝（统一 404）───────────────────────

func TestMjImageRouteRejectsCrossUserAccess(t *testing.T) {
	engine, mjID, _ := setupResourceSecurityTestEnv(t)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/mj/image/"+mjID, nil)
	req.Header.Set("Authorization", "Bearer resotherkey")
	engine.ServeHTTP(rec, req)
	require.Equal(t, http.StatusNotFound, rec.Code,
		"用户 B 访问用户 A 的 MJ 图片必须被拒绝（统一 404），实际=%d body=%s", rec.Code, rec.Body.String())
}

// ── 3. 所有者正常访问 /mj/image/:id ──────────────────────────────────────

func TestMjImageRouteOwnerCanFetch(t *testing.T) {
	engine, mjID, _ := setupResourceSecurityTestEnv(t)
	service.InitHttpClient()
	allowLoopbackFetchForTest(t)

	imgBytes := []byte("fake-mj-image-bytes")
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(imgBytes)
	}))
	defer upstream.Close()

	require.NoError(t, model.DB.Model(&model.Midjourney{}).
		Where("mj_id = ?", mjID).Update("image_url", upstream.URL+"/img.png").Error)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/mj/image/"+mjID, nil)
	req.Header.Set("Authorization", "Bearer resownerkey")
	engine.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code,
		"所有者访问自己的 MJ 图片应成功，实际=%d body=%s", rec.Code, rec.Body.String())
	assert.Equal(t, "image/png", rec.Header().Get("Content-Type"))
	assert.Equal(t, string(imgBytes), rec.Body.String())
}

// ── 4. Seedance 任务 fetch：鉴权 + 所有权 + 无上游直链 ───────────────────

func TestSeedanceTaskFetchAuthzAndNoUpstreamLeak(t *testing.T) {
	engine, _, videoTaskID := setupResourceSecurityTestEnv(t)

	// 匿名 → 401
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/video/generations/"+videoTaskID, nil)
	engine.ServeHTTP(rec, req)
	require.Equal(t, http.StatusUnauthorized, rec.Code,
		"匿名获取 Seedance 任务必须返回 401，实际=%d body=%s", rec.Code, rec.Body.String())

	// 用户 B（非所有者）→ 400 task_not_exist（统一错误，不泄露资源存在性）
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/v1/video/generations/"+videoTaskID, nil)
	req.Header.Set("Authorization", "Bearer resotherkey")
	engine.ServeHTTP(rec, req)
	require.Equal(t, http.StatusBadRequest, rec.Code,
		"越权获取任务必须被拒绝，实际=%d body=%s", rec.Code, rec.Body.String())
	require.NotContains(t, rec.Body.String(), videoTaskID, "越权响应不得泄露任务 ID/状态")

	// 所有者 → 200，响应不含上游直链，result_url 指向本服务鉴权代理入口
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/v1/video/generations/"+videoTaskID, nil)
	req.Header.Set("Authorization", "Bearer resownerkey")
	engine.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code,
		"所有者获取任务应成功，实际=%d body=%s", rec.Code, rec.Body.String())
	body := rec.Body.String()
	require.NotContains(t, body, upstreamSeedanceHost, "响应不得包含上游资源直链（result_url/data）")
	require.NotContains(t, body, "video_url", "响应 data 不得包含上游 content.video_url")
	require.Contains(t, body, "/v1/videos/"+videoTaskID+"/content",
		"result_url 必须指向本服务鉴权代理入口，body=%s", body)
}

func TestSeedanceOpenAIVideoFetchNoUpstreamLeak(t *testing.T) {
	engine, _, videoTaskID := setupResourceSecurityTestEnv(t)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/videos/"+videoTaskID, nil)
	req.Header.Set("Authorization", "Bearer resownerkey")
	engine.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code,
		"所有者通过 OpenAI Video API 获取任务应成功，实际=%d body=%s", rec.Code, rec.Body.String())
	body := rec.Body.String()
	require.NotContains(t, body, upstreamSeedanceHost, "OpenAI Video 响应不得包含上游资源直链")
	require.Contains(t, body, "/v1/videos/"+videoTaskID+"/content",
		"metadata.url 必须指向本服务鉴权代理入口，body=%s", body)
}

// ── 5. /v1/videos/:task_id/content 代理：鉴权 + 所有权 + 响应头脱敏 ───────

func TestVideoContentProxyAuthzOwnershipAndHeaderSanitization(t *testing.T) {
	engine, _, videoTaskID := setupResourceSecurityTestEnv(t)
	service.InitHttpClient()
	allowLoopbackFetchForTest(t)

	videoBytes := []byte("fake-video-bytes")
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "video/mp4")
		// 模拟上游可能返回的重定向头/自定义头（不得透传给客户端）
		w.Header().Set("Location", upstreamSeedanceHost+"/api/v3/contents/generations/tasks/up-1/content?v=secret")
		w.Header().Set("X-Upstream-URL", upstreamSeedanceHost+"/secret.mp4")
		_, _ = w.Write(videoBytes)
	}))
	defer upstream.Close()

	// 将任务的 result_url 指向本地上游，验证代理成功路径与响应头脱敏
	var task model.Task
	require.NoError(t, model.DB.Where("task_id = ?", videoTaskID).First(&task).Error)
	task.PrivateData.ResultURL = upstream.URL + "/video.mp4"
	require.NoError(t, model.DB.Save(&task).Error)

	// 匿名 → 401
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/videos/"+videoTaskID+"/content", nil)
	engine.ServeHTTP(rec, req)
	require.Equal(t, http.StatusUnauthorized, rec.Code,
		"匿名访问视频内容代理必须返回 401，实际=%d body=%s", rec.Code, rec.Body.String())

	// 用户 B（非所有者）→ 404（统一不泄露资源存在性）
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/v1/videos/"+videoTaskID+"/content", nil)
	req.Header.Set("Authorization", "Bearer resotherkey")
	engine.ServeHTTP(rec, req)
	require.Equal(t, http.StatusNotFound, rec.Code,
		"越权访问视频内容代理必须被拒绝，实际=%d body=%s", rec.Code, rec.Body.String())

	// 所有者 → 200，内容与 Content-Type 正确，缓存策略合理，且不泄露上游地址
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/v1/videos/"+videoTaskID+"/content", nil)
	req.Header.Set("Authorization", "Bearer resownerkey")
	engine.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code,
		"所有者访问视频内容应成功，实际=%d body=%s", rec.Code, rec.Body.String())
	assert.Equal(t, string(videoBytes), rec.Body.String())
	assert.Equal(t, "video/mp4", rec.Header().Get("Content-Type"))
	// 私有用户资源禁止一切缓存：private 只排除共享缓存，同一浏览器内的用户
	// 私有缓存无法按登录账号隔离（A 退出后 B 复用 URL 可能直接命中缓存、
	// 绕过服务端所有权校验），必须 no-store 保证每次访问都执行鉴权与校验。
	cacheControl := rec.Header().Get("Cache-Control")
	assert.Contains(t, cacheControl, "private", "私有用户资源必须为 private，实际=%q", cacheControl)
	assert.Contains(t, cacheControl, "no-store", "私有用户资源必须禁止一切缓存(no-store)，实际=%q", cacheControl)
	assert.NotContains(t, cacheControl, "public", "私有用户资源禁止 public 缓存")
	assert.NotContains(t, cacheControl, "max-age", "禁止 max-age：任何时长缓存都可能绕过所有权校验")
	assert.Empty(t, rec.Header().Get("Location"), "不得透传上游 Location（重定向地址）")
	assert.Empty(t, rec.Header().Get("X-Upstream-URL"), "不得透传上游自定义头")
	for key, values := range rec.Header() {
		for _, v := range values {
			require.NotContains(t, v, upstreamSeedanceHost,
				"响应头 %s 泄露上游地址: %s", key, v)
		}
	}
}

func TestVideoPlaybackURLStreamsAuthenticatedSingleRange(t *testing.T) {
	engine, _, videoTaskID := setupResourceSecurityTestEnv(t)
	service.InitHttpClient()
	allowLoopbackFetchForTest(t)

	previousSecret := common.SessionSecret
	common.SessionSecret = "video-playback-test-secret-with-sufficient-entropy"
	t.Cleanup(func() { common.SessionSecret = previousSecret })

	videoBytes := []byte("0123456789")
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		require.Equal(t, "bytes=2-5", req.Header.Get("Range"))
		w.Header().Set("Content-Type", "video/mp4")
		w.Header().Set("Accept-Ranges", "bytes")
		w.Header().Set("Content-Range", "bytes 2-5/10")
		w.Header().Set("Content-Length", "4")
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(videoBytes[2:6])
	}))
	defer upstream.Close()

	var task model.Task
	require.NoError(t, model.DB.Where("task_id = ?", videoTaskID).First(&task).Error)
	task.PrivateData.ResultURL = upstream.URL + "/video.mp4"
	require.NoError(t, model.DB.Save(&task).Error)

	issueRec := httptest.NewRecorder()
	issueReq := httptest.NewRequest(http.MethodGet, "/v1/videos/"+videoTaskID+"/playback-url", nil)
	issueReq.Header.Set("Authorization", "Bearer resownerkey")
	engine.ServeHTTP(issueRec, issueReq)
	require.Equal(t, http.StatusOK, issueRec.Code, issueRec.Body.String())
	var issued struct {
		URL string `json:"url"`
	}
	require.NoError(t, json.Unmarshal(issueRec.Body.Bytes(), &issued))
	require.NotEmpty(t, issued.URL)

	playRec := httptest.NewRecorder()
	playReq := httptest.NewRequest(http.MethodGet, issued.URL, nil)
	playReq.Header.Set("Range", "bytes=2-5")
	engine.ServeHTTP(playRec, playReq)
	require.Equal(t, http.StatusPartialContent, playRec.Code, playRec.Body.String())
	assert.Equal(t, "2345", playRec.Body.String())
	assert.Equal(t, "bytes 2-5/10", playRec.Header().Get("Content-Range"))
	assert.Equal(t, "bytes", playRec.Header().Get("Accept-Ranges"))
	assert.Contains(t, playRec.Header().Get("Cache-Control"), "no-store")

	parsedURL, err := url.Parse(issued.URL)
	require.NoError(t, err)
	parsedURL.Path = "/v1/videos/task_other/playback"
	wrongTaskRec := httptest.NewRecorder()
	engine.ServeHTTP(wrongTaskRec, httptest.NewRequest(http.MethodGet, parsedURL.String(), nil))
	assert.Equal(t, http.StatusUnauthorized, wrongTaskRec.Code)
}

func TestVideoPlaybackRejectsMultipleRangesBeforeUpstream(t *testing.T) {
	engine, _, videoTaskID := setupResourceSecurityTestEnv(t)
	service.InitHttpClient()
	allowLoopbackFetchForTest(t)

	previousSecret := common.SessionSecret
	common.SessionSecret = "video-playback-range-test-secret"
	t.Cleanup(func() { common.SessionSecret = previousSecret })

	upstreamCalled := false
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		upstreamCalled = true
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()
	var task model.Task
	require.NoError(t, model.DB.Where("task_id = ?", videoTaskID).First(&task).Error)
	task.PrivateData.ResultURL = upstream.URL + "/video.mp4"
	require.NoError(t, model.DB.Save(&task).Error)

	issueRec := httptest.NewRecorder()
	issueReq := httptest.NewRequest(http.MethodGet, "/v1/videos/"+videoTaskID+"/playback-url", nil)
	issueReq.Header.Set("Authorization", "Bearer resownerkey")
	engine.ServeHTTP(issueRec, issueReq)
	require.Equal(t, http.StatusOK, issueRec.Code)
	var issued struct {
		URL string `json:"url"`
	}
	require.NoError(t, json.Unmarshal(issueRec.Body.Bytes(), &issued))

	playRec := httptest.NewRecorder()
	playReq := httptest.NewRequest(http.MethodGet, issued.URL, nil)
	playReq.Header.Set("Range", "bytes=0-1,4-5")
	engine.ServeHTTP(playRec, playReq)
	assert.Equal(t, http.StatusRequestedRangeNotSatisfiable, playRec.Code)
	assert.False(t, upstreamCalled)
}

// TestVideoContentProxyDataURLSuccessNotPublic 覆盖视频代理的 data URL 成功路径：
// 同样不得返回共享缓存（public），且匿名 401 / 跨用户 404 保持不变。
func TestVideoContentProxyDataURLSuccessNotPublic(t *testing.T) {
	engine, _, videoTaskID := setupResourceSecurityTestEnv(t)

	// 将任务结果改为 data URL（第二条成功响应路径）
	videoBytes := []byte("fake-video-data-url-bytes")
	dataURL := "data:video/mp4;base64," + base64.StdEncoding.EncodeToString(videoBytes)
	var task model.Task
	require.NoError(t, model.DB.Where("task_id = ?", videoTaskID).First(&task).Error)
	task.PrivateData.ResultURL = dataURL
	require.NoError(t, model.DB.Save(&task).Error)

	// 匿名 → 401
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/videos/"+videoTaskID+"/content", nil)
	engine.ServeHTTP(rec, req)
	require.Equal(t, http.StatusUnauthorized, rec.Code,
		"匿名访问 data URL 视频内容代理必须返回 401，实际=%d body=%s", rec.Code, rec.Body.String())

	// 用户 B（非所有者）→ 404
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/v1/videos/"+videoTaskID+"/content", nil)
	req.Header.Set("Authorization", "Bearer resotherkey")
	engine.ServeHTTP(rec, req)
	require.Equal(t, http.StatusNotFound, rec.Code,
		"越权访问 data URL 视频内容代理必须被拒绝，实际=%d body=%s", rec.Code, rec.Body.String())

	// 所有者 → 200，内容正确且缓存策略为 private（非 public）
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/v1/videos/"+videoTaskID+"/content", nil)
	req.Header.Set("Authorization", "Bearer resownerkey")
	engine.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code,
		"所有者访问 data URL 视频内容应成功，实际=%d body=%s", rec.Code, rec.Body.String())
	assert.Equal(t, string(videoBytes), rec.Body.String())
	assert.Equal(t, "video/mp4", rec.Header().Get("Content-Type"))
	// data URL 成功路径同样禁止一切缓存（与上游代理路径一致）
	cacheControl := rec.Header().Get("Cache-Control")
	assert.Contains(t, cacheControl, "private", "data URL 成功路径必须为 private，实际=%q", cacheControl)
	assert.Contains(t, cacheControl, "no-store", "data URL 成功路径必须禁止一切缓存(no-store)，实际=%q", cacheControl)
	assert.NotContains(t, cacheControl, "public", "data URL 成功路径禁止 public 缓存")
	assert.NotContains(t, cacheControl, "max-age", "data URL 成功路径禁止 max-age 缓存")
}
