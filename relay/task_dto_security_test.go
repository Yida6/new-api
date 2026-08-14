package relay

import (
	"encoding/json"
	"testing"

	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"

	"github.com/stretchr/testify/require"
)

// newTaskForSecurityTest builds a task whose properties/upstream payload mimic
// an Ark (Volcano Engine) task: the upstream model name is the Endpoint ID and
// the stored data echoes it (as the upstream status response does).
func newTaskForSecurityTest() *model.Task {
	return &model.Task{
		ID:         1,
		TaskID:     "task_abcdef123456",
		Platform:   constant.TaskPlatform("54"),
		UserId:     1,
		Group:      "default",
		ChannelId:  1,
		Quota:      100,
		Action:     "generate",
		Status:     model.TaskStatusSuccess,
		FailReason: "upstream error: model ep-20250101-abc123 does not exist, key sk-abc123def456ghi789",
		Properties: model.Properties{
			Input:             "a video prompt",
			UpstreamModelName: "ep-20250101-abc123",
			OriginModelName:   "doubao-seedance-2-0-260128",
		},
		Data: json.RawMessage(`{"id":"t-1","model":"ep-20250101-abc123","status":"success"}`),
	}
}

// TestTaskModel2DtoHidesSensitiveFields verifies the serialized task DTO never
// exposes the Ark Endpoint ID (properties.upstream_model_name / data / fail
// reason) or API keys, while keeping the business fields intact.
func TestTaskModel2DtoHidesSensitiveFields(t *testing.T) {
	task := newTaskForSecurityTest()
	item := TaskModel2Dto(task)

	raw, err := json.Marshal(item)
	require.NoError(t, err)
	body := string(raw)

	// Sensitive markers must never appear anywhere in the response payload.
	require.NotContains(t, body, "upstream_model_name", "task DTO must not expose properties.upstream_model_name")
	require.NotContains(t, body, "ep-20250101-abc123", "task DTO must not expose the Ark Endpoint ID")
	require.NotContains(t, body, "sk-abc123def456ghi789", "task DTO must not expose API keys")

	// Business fields remain available.
	require.Contains(t, body, `"task_id":"task_abcdef123456"`)
	require.Contains(t, body, `"origin_model_name":"doubao-seedance-2-0-260128"`)
	require.Contains(t, body, `"input":"a video prompt"`)

	// FailReason and Data are redacted, not dropped.
	require.Contains(t, body, `"fail_reason":"upstream error: model ep-*** does not exist, key sk-***"`)
	require.Contains(t, body, `"data":{"id":"t-1","model":"ep-***","status":"success"}`)
}

// TestTaskModel2DtoSeedanceResultURLPointsToProxy verifies that for Seedance
// tasks the response result_url is replaced with this service's authenticated
// proxy entry, never the raw upstream URL, even when the task carries a direct
// upstream result URL in PrivateData.
func TestTaskModel2DtoSeedanceResultURLPointsToProxy(t *testing.T) {
	task := newTaskForSecurityTest()
	task.FailReason = ""
	task.PrivateData.ResultURL = "https://ark.cn-beijing.volces.com/api/v3/contents/generations/tasks/up-1/content?v=secret"
	task.Data = json.RawMessage(`{"id":"up-1","model":"seedance-2.0","status":"succeeded","content":{"video_url":"https://ark.cn-beijing.volces.com/api/v3/contents/generations/tasks/up-1/content?v=secret"},"usage":{"total_tokens":100}}`)

	item := TaskModel2Dto(task)
	raw, err := json.Marshal(item)
	require.NoError(t, err)
	body := string(raw)

	require.NotContains(t, body, "ark.cn-beijing.volces.com", "Seedance DTO must not expose the upstream result URL")
	require.NotContains(t, body, "video_url", "Seedance DTO must strip upstream content.video_url from data")
	require.Contains(t, body, "/v1/videos/task_abcdef123456/content",
		"Seedance result_url must point to the authenticated proxy entry")
}

// TestTaskModel2DtoNonSeedanceKeepsResultURL verifies non-Seedance tasks keep
// their result URL semantics (legacy FailReason fallback is redacted, direct
// URLs are preserved) — the proxy substitution is Seedance-specific.
func TestTaskModel2DtoNonSeedanceKeepsResultURL(t *testing.T) {
	task := newTaskForSecurityTest()
	task.Platform = constant.TaskPlatform("99") // 非 Seedance 平台
	task.PrivateData.ResultURL = ""

	item := TaskModel2Dto(task)
	raw, err := json.Marshal(item)
	require.NoError(t, err)
	body := string(raw)

	// 非 Seedance：仍走旧行为——ResultURL 回退到脱敏后的 FailReason
	require.Contains(t, body, `"result_url":"upstream error: model ep-*** does not exist, key sk-***"`)
}

// TestTaskModel2DtoUnmappedTask verifies a task without model mapping is
// serialized unchanged (no regression for regular channels).
func TestTaskModel2DtoUnmappedTask(t *testing.T) {
	task := &model.Task{
		ID:         2,
		TaskID:     "task_xyz",
		Status:     model.TaskStatusSuccess,
		FailReason: "",
		Properties: model.Properties{
			Input:           "prompt",
			OriginModelName: "gpt-4o-mini",
		},
		Data: json.RawMessage(`{"url":"https://example.com/video.mp4"}`),
	}

	item := TaskModel2Dto(task)
	raw, err := json.Marshal(item)
	require.NoError(t, err)
	body := string(raw)

	require.NotContains(t, body, "upstream_model_name")
	require.Contains(t, body, `"origin_model_name":"gpt-4o-mini"`)
	// URLs inside Data are preserved (only credentials are masked).
	require.Contains(t, body, `https://example.com/video.mp4`)
}

// TestTaskModel2DtoNilDataPreserved verifies nil/empty Data stays nil so the
// response keeps emitting null for empty tasks.
func TestTaskModel2DtoNilDataPreserved(t *testing.T) {
	task := &model.Task{
		ID:     3,
		TaskID: "task_nil",
		Status: model.TaskStatusQueued,
		Properties: model.Properties{
			OriginModelName: "m",
		},
	}
	item := TaskModel2Dto(task)
	require.Nil(t, item.Data)

	raw, err := json.Marshal(item)
	require.NoError(t, err)
	require.Contains(t, string(raw), `"data":null`)
}

// TestTaskDtoPropertiesShapeSanity ensures the sanitized properties object is
// still a JSON object (frontend compatibility), not a string or null.
func TestTaskDtoPropertiesShapeSanity(t *testing.T) {
	task := newTaskForSecurityTest()
	item := TaskModel2Dto(task)

	propsJSON, err := json.Marshal(item.Properties)
	require.NoError(t, err)
	require.NotNil(t, propsJSON)
	require.True(t, len(propsJSON) > 2 && propsJSON[0] == '{' && propsJSON[len(propsJSON)-1] == '}')

	var m map[string]interface{}
	require.NoError(t, json.Unmarshal(propsJSON, &m))
	require.Equal(t, "a video prompt", m["input"])
	require.Equal(t, "doubao-seedance-2-0-260128", m["origin_model_name"])
	_, hasUpstream := m["upstream_model_name"]
	require.False(t, hasUpstream)
}

// TestTaskModel2DtoSeedanceFailReasonHidesUpstreamURL 验证 Seedance 任务的
// fail_reason 即使包含上游 URL（RedactCredentials 只掩码凭证、保留 URL），
// 对外响应也不得泄露可访问的上游地址。
func TestTaskModel2DtoSeedanceFailReasonHidesUpstreamURL(t *testing.T) {
	task := newTaskForSecurityTest()
	task.PrivateData.ResultURL = ""
	task.FailReason = "generation failed: https://ark.cn-beijing.volces.com/api/v3/contents/generations/tasks/up-1/content?v=secret"

	item := TaskModel2Dto(task)
	raw, err := json.Marshal(item)
	require.NoError(t, err)
	body := string(raw)

	require.NotContains(t, body, "ark.cn-beijing.volces.com",
		"Seedance fail_reason 不得泄露上游 URL，body=%s", body)
	require.NotContains(t, body, "contents/generations",
		"Seedance fail_reason 不得泄露上游路径细节，body=%s", body)
	// 掩码后仍保留受控错误文本（而非整段删除）
	require.Contains(t, body, "generation failed")
}

// TestTaskModel2DtoSeedanceDataNestedURLFieldsStripped 验证 Seedance data 的
// fail-closed 清洗：嵌套对象、数组以及不同 URL 字段名（url/video_url/
// result_url/download_url/play_url/cover_url）下均不泄露上游直链。
func TestTaskModel2DtoSeedanceDataNestedURLFieldsStripped(t *testing.T) {
	task := newTaskForSecurityTest()
	task.PrivateData.ResultURL = ""
	task.Data = json.RawMessage(`{
		"id": "up-1",
		"model": "seedance-2.0",
		"status": "succeeded",
		"content": [
			{"type": "video_url", "video_url": {"url": "https://ark.cn-beijing.volces.com/api/v3/contents/generations/tasks/up-1/content?v=1"}},
			{"type": "text", "text": "keep me"}
		],
		"result_url": "https://cdn.volces.example.net/result.mp4",
		"download_url": "https://cdn.volces.example.net/dl.mp4",
		"play_url": "https://cdn.volces.example.net/play.mp4",
		"cover_url": "https://cdn.volces.example.net/cover.jpg",
		"nested": {"thumbnail_url": "https://cdn.volces.example.net/thumb.jpg", "deeper": {"url": "https://cdn.volces.example.net/deep.mp4"}},
		"usage": {"total_tokens": 100}
	}`)

	item := TaskModel2Dto(task)
	raw, err := json.Marshal(item)
	require.NoError(t, err)
	body := string(raw)

	// 提取 data 子对象做字段级校验（DTO 顶层 result_url 指向代理入口是合法的）
	var resp map[string]any
	require.NoError(t, json.Unmarshal(raw, &resp))
	dataRaw, err := json.Marshal(resp["data"])
	require.NoError(t, err)
	dataBody := string(dataRaw)

	require.NotContains(t, body, "ark.cn-beijing.volces.com", "data 不得泄露上游视频直链，body=%s", body)
	require.NotContains(t, dataBody, "cdn.volces.example.net", "data 不得泄露任何资源地址，body=%s", dataBody)
	for _, field := range []string{"result_url", "download_url", "play_url", "cover_url", "thumbnail_url"} {
		require.NotContains(t, dataBody, field, "data 必须删除可能携带资源地址的字段 %s，body=%s", field, dataBody)
	}
	// 非 URL 业务字段保留（fail-open 的字段不误删）
	require.Contains(t, dataBody, `"text":"keep me"`)
	require.Contains(t, dataBody, `"usage":{"total_tokens":100}`)
	require.Contains(t, dataBody, `"status":"succeeded"`)
}

// TestTaskModel2DtoSeedanceDataInvalidFailsClosed 验证 Seedance data 无法解析
// 或无法清洗时返回 null/最小安全结构，绝不回退原始数据（可能含上游地址）。
func TestTaskModel2DtoSeedanceDataInvalidFailsClosed(t *testing.T) {
	for name, invalid := range map[string]string{
		"not-json":       `not-json-at-all`,
		"scalar":         `"just-a-string"`,
		"url-array":      `["https://ark.cn-beijing.volces.com/x.mp4"]`,
		"all-url-object": `{"url":"https://ark.cn-beijing.volces.com/x.mp4"}`,
	} {
		t.Run(name, func(t *testing.T) {
			task := newTaskForSecurityTest()
			task.PrivateData.ResultURL = ""
			task.Data = json.RawMessage(invalid)

			item := TaskModel2Dto(task)
			raw, err := json.Marshal(item)
			require.NoError(t, err)
			body := string(raw)

			require.NotContains(t, body, "ark.cn-beijing.volces.com",
				"无效/不可清洗的 data 不得回退原始数据，body=%s", body)
			require.NotContains(t, body, invalid,
				"data 必须 fail-closed（null/空对象），不得回显原文，body=%s", body)
			require.Contains(t, body, `"data":null`,
				"无法清洗的 data 必须返回 null 最小安全结构，body=%s", body)
		})
	}
}
