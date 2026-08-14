package doubao

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/model"
	relaycommon "github.com/QuantumNous/new-api/relay/common"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// 首次创建请求必须携带 X-Client-Request-Id；同一次请求重试（再次调用
// BuildRequestHeader）必须复用相同值。绝不发送 Idempotency-Key（服务端未声明支持）。
func TestBuildRequestHeaderSetsAndReusesClientRequestID(t *testing.T) {
	a := &TaskAdaptor{apiKey: "test-key", baseURL: "https://ark.cn-beijing.volces.com"}
	info := &relaycommon.RelayInfo{TaskRelayInfo: &relaycommon.TaskRelayInfo{}}

	req, err := http.NewRequest(http.MethodPost, "https://ark.cn-beijing.volces.com/api/v3/contents/generations/tasks", nil)
	require.NoError(t, err)

	require.NoError(t, a.BuildRequestHeader(nil, req, info))
	first := req.Header.Get("X-Client-Request-Id")
	require.NotEmpty(t, first, "首次创建请求必须携带 X-Client-Request-Id 请求头")
	parsed, err := uuid.Parse(first)
	require.NoError(t, err, "X-Client-Request-Id 必须是合法 UUID: %q", first)
	assert.Equal(t, uuid.Version(4), parsed.Version())

	// 必须不发送未声明支持的 Idempotency-Key
	assert.Empty(t, req.Header.Get("Idempotency-Key"), "不得发送服务端未声明支持的 Idempotency-Key")

	// 模拟超时后的重试：再次构建请求头，X-Client-Request-Id 必须保持不变（日志串联）
	req2, err := http.NewRequest(http.MethodPost, "https://ark.cn-beijing.volces.com/api/v3/contents/generations/tasks", nil)
	require.NoError(t, err)
	require.NoError(t, a.BuildRequestHeader(nil, req2, info))
	assert.Equal(t, first, req2.Header.Get("X-Client-Request-Id"), "重试必须复用相同的 X-Client-Request-Id")
}

// 无 TaskRelayInfo（如任务查询路径）时不携带 X-Client-Request-Id，且不 panic。
func TestBuildRequestHeaderNoKeyWhenNoTaskRelayInfo(t *testing.T) {
	a := &TaskAdaptor{apiKey: "test-key", baseURL: "https://ark.cn-beijing.volces.com"}
	info := &relaycommon.RelayInfo{}

	req, err := http.NewRequest(http.MethodPost, "https://ark.cn-beijing.volces.com/api/v3/contents/generations/tasks", nil)
	require.NoError(t, err)
	require.NoError(t, a.BuildRequestHeader(nil, req, info))
	assert.Empty(t, req.Header.Get("X-Client-Request-Id"))
	assert.Empty(t, req.Header.Get("Idempotency-Key"))
}

// 请求成功后正确返回 Seedance 上游任务 ID。
func TestDoResponseReturnsUpstreamTaskID(t *testing.T) {
	gin.SetMode(gin.TestMode)
	a := &TaskAdaptor{}
	info := &relaycommon.RelayInfo{
		OriginModelName: "doubao-seedance-2-0-260128",
		TaskRelayInfo: &relaycommon.TaskRelayInfo{
			PublicTaskID: "task_public123",
		},
	}

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/videos", nil)

	resp := &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader(`{"id":"cgt-seedance-0001"}`)),
	}

	taskID, taskData, taskErr := a.DoResponse(c, resp, info)
	require.Nil(t, taskErr)
	assert.Equal(t, "cgt-seedance-0001", taskID, "必须返回 Seedance 上游任务 ID")
	require.NotEmpty(t, taskData)
	assert.Contains(t, string(taskData), "cgt-seedance-0001")

	// 响应体中包含公开任务 ID
	assert.Contains(t, w.Body.String(), "task_public123")
	assert.Equal(t, http.StatusOK, w.Code)
}

// 查询列表接口与内容指纹（候选发现的模糊匹配基础）。
func TestParseTaskListAndContentFingerprint(t *testing.T) {
	body := `{
		"items": [
			{
				"id": "cgt-a",
				"model": "doubao-seedance-2-0-260128",
				"status": "succeeded",
				"created_at": 1718049470,
				"content": [
					{"type": "image_url", "image_url": {"url": "https://img.example/a.png"}},
					{"type": "text", "text": "一只柯基在草地上奔跑"}
				]
			},
			{
				"id": "cgt-b",
				"model": "doubao-seedance-2-0-260128",
				"status": "queued",
				"created_at": "1718049471",
				"content": [{"type": "text", "text": "完全不同的提示词"}]
			}
		],
		"total": 2
	}`
	items, err := ParseTaskList([]byte(body))
	require.NoError(t, err)
	require.Len(t, items, 2)

	// created_at 兼容整数与字符串
	assert.Equal(t, int64(1718049470), int64(items[0].CreatedAt))
	assert.Equal(t, int64(1718049471), int64(items[1].CreatedAt))

	// 与客户端内容指纹同构：model + 文本 + 图片数量
	clientFP := relaycommon.SubmitContentFingerprint(relaycommon.TaskSubmitReq{
		Model:  "doubao-seedance-2-0-260128",
		Prompt: "一只柯基在草地上奔跑",
		Images: []string{"data:image/png;base64,AAAA"},
	})
	assert.Equal(t, clientFP, items[0].ContentFingerprint(), "同内容候选应与客户端指纹一致")
	assert.NotEqual(t, clientFP, items[1].ContentFingerprint(), "不同内容候选指纹应不同")
}

// ===========================================================================
// 保守预扣估算（EstimateSeedanceRatios / ResolveSeedanceDuration）
// 契约说明见 constants.go 文件头注释；测试同时充当公式依据文档。
// ===========================================================================

// 旧版估算测试已迁移至 billing_estimate_test.go：
// 定价倍率（PricingRatios）与仅预扣缓冲（PreConsumeMultiplier）分离、
// 模型别名归一化（ResolveSeedancePriceModel）、时长三态解析与 {5,10} 支持
// 矩阵校验、分辨率/价格表缺失组合 400、metadata 大小写归一化与浮点拒绝、
// 多字节原因截断安全。见 billing_estimate_test.go。

// ===========================================================================
// OpenAI Video API 响应安全：metadata.url 必须指向本服务鉴权代理入口，
// 禁止返回上游 video_url 直链（客户端可绕过本地鉴权直接访问上游资源）。
// ===========================================================================

func TestConvertToOpenAIVideoUsesProxyURLNotUpstream(t *testing.T) {
	a := &TaskAdaptor{}
	task := &model.Task{
		TaskID: "task_seedance_proxy_test",
		Status: model.TaskStatusSuccess,
		Properties: model.Properties{
			OriginModelName: "doubao-seedance-2-0-260128",
		},
		Data: json.RawMessage(`{"id":"up-1","model":"seedance-2.0","status":"succeeded","content":{"video_url":"https://ark.cn-beijing.volces.com/api/v3/contents/generations/tasks/up-1/content?v=secret"},"usage":{"total_tokens":100}}`),
	}

	body, err := a.ConvertToOpenAIVideo(task)
	require.NoError(t, err)
	s := string(body)

	require.NotContains(t, s, "ark.cn-beijing.volces.com", "OpenAI Video 响应不得包含上游视频直链")
	require.NotContains(t, s, "video_url", "OpenAI Video 响应不得包含上游 video_url 字段")
	require.Contains(t, s, "/v1/videos/task_seedance_proxy_test/content",
		"metadata.url 必须指向本服务鉴权代理入口，body=%s", s)
}

// 失败任务的 error.message 即使携带上游 URL（RedactCredentials 保留 URL），
// OpenAI Video 响应也不得泄露可直接访问的上游地址。
func TestConvertToOpenAIVideoErrorHidesUpstreamURL(t *testing.T) {
	a := &TaskAdaptor{}
	task := &model.Task{
		TaskID: "task_seedance_err_test",
		Status: model.TaskStatusFailure,
		Properties: model.Properties{
			OriginModelName: "doubao-seedance-2-0-260128",
		},
		Data: json.RawMessage(`{"id":"up-1","model":"seedance-2.0","status":"failed","error":{"code":"1201","message":"generation failed: https://ark.cn-beijing.volces.com/api/v3/contents/generations/tasks/up-1/content?v=secret"}}`),
	}

	body, err := a.ConvertToOpenAIVideo(task)
	require.NoError(t, err)
	s := string(body)

	require.NotContains(t, s, "ark.cn-beijing.volces.com",
		"OpenAI Video error.message 不得泄露上游 URL，body=%s", s)
	require.NotContains(t, s, "contents/generations",
		"OpenAI Video error.message 不得泄露上游路径细节，body=%s", s)
	require.Contains(t, s, "generation failed",
		"掩码后应保留受控错误文本，body=%s", s)
	require.Contains(t, s, "/v1/videos/task_seedance_err_test/content",
		"metadata.url 必须指向本服务鉴权代理入口，body=%s", s)
}
