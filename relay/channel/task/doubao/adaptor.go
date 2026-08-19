package doubao

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"

	"github.com/QuantumNous/new-api/constant"
	taskdto "github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/relay/channel"
	"github.com/QuantumNous/new-api/relay/channel/task/taskcommon"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/service"

	"github.com/gin-gonic/gin"
	"github.com/pkg/errors"
	"github.com/samber/lo"
)

// ============================
// Request / Response structures
// ============================

type ContentItem struct {
	Type     string    `json:"type,omitempty"`
	Text     string    `json:"text,omitempty"`
	ImageURL *MediaURL `json:"image_url,omitempty"`
	VideoURL *MediaURL `json:"video_url,omitempty"`
	AudioURL *MediaURL `json:"audio_url,omitempty"`
	Role     string    `json:"role,omitempty"`
}

type MediaURL struct {
	URL string `json:"url,omitempty"`
}

type requestPayload struct {
	Model                 string         `json:"model"`
	Content               []ContentItem  `json:"content,omitempty"`
	CallbackURL           string         `json:"callback_url,omitempty"`
	ReturnLastFrame       *dto.BoolValue `json:"return_last_frame,omitempty"`
	ServiceTier           string         `json:"service_tier,omitempty"`
	ExecutionExpiresAfter *dto.IntValue  `json:"execution_expires_after,omitempty"`
	GenerateAudio         *dto.BoolValue `json:"generate_audio,omitempty"`
	Draft                 *dto.BoolValue `json:"draft,omitempty"`
	Tools                 []struct {
		Type string `json:"type,omitempty"`
	} `json:"tools,omitempty"`
	SafetyIdentifier string         `json:"safety_identifier,omitempty"`
	Priority         *dto.IntValue  `json:"priority,omitempty"`
	Resolution       string         `json:"resolution,omitempty"`
	Ratio            string         `json:"ratio,omitempty"`
	Duration         *dto.IntValue  `json:"duration,omitempty"`
	Frames           *dto.IntValue  `json:"frames,omitempty"`
	Seed             *dto.IntValue  `json:"seed,omitempty"`
	CameraFixed      *dto.BoolValue `json:"camera_fixed,omitempty"`
	Watermark        *dto.BoolValue `json:"watermark,omitempty"`
}

type responsePayload struct {
	ID string `json:"id"` // task_id
}

type responseTask struct {
	ID      string `json:"id"`
	Model   string `json:"model"`
	Status  string `json:"status"`
	Content struct {
		VideoURL string `json:"video_url"`
	} `json:"content"`
	Seed            int    `json:"seed"`
	Resolution      string `json:"resolution"`
	Duration        int    `json:"duration"`
	Ratio           string `json:"ratio"`
	FramesPerSecond int    `json:"framespersecond"`
	ServiceTier     string `json:"service_tier"`
	Tools           []struct {
		Type string `json:"type"`
	} `json:"tools"`
	Usage struct {
		CompletionTokens int `json:"completion_tokens"`
		TotalTokens      int `json:"total_tokens"`
		ToolUsage        struct {
			WebSearch int `json:"web_search"`
		} `json:"tool_usage"`
	} `json:"usage"`
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
	CreatedAt int64 `json:"created_at"`
	UpdatedAt int64 `json:"updated_at"`
}

// ============================
// Adaptor implementation
// ============================

type TaskAdaptor struct {
	taskcommon.BaseBilling
	ChannelType int
	apiKey      string
	baseURL     string
}

func (a *TaskAdaptor) Init(info *relaycommon.RelayInfo) {
	a.ChannelType = info.ChannelType
	a.baseURL = info.ChannelBaseUrl
	a.apiKey = info.ApiKey
}

// ValidateRequestAndSetAction parses body, validates fields and sets default action.
// Seedance 参数支持矩阵（与 constants.go 注释/测试同一契约）：
//   - 时长：按模型系列允许（SeedanceSupportedDurationsForModel，2.5: 4–30s、
//     2.0: 4–15s、未知模型: 5–10s）；提供了但无法可靠解析（非数字、浮点小数、
//     <=0）→ 400；未提供 → 合法（上游默认）。
//   - 分辨率：未知格式 → 400；有价格表模型 + 显式档（1080p/4k）在表内缺失 → 400。
//   - 上述校验全部在发送上游之前完成，绝不"以低价预扣后继续提交"。
func (a *TaskAdaptor) ValidateRequestAndSetAction(c *gin.Context, info *relaycommon.RelayInfo) (taskErr *taskdto.TaskError) {
	// Accept only POST /v1/video/generations as "generate" action.
	if taskErr := relaycommon.ValidateBasicTaskRequest(c, info, constant.TaskActionGenerate); taskErr != nil {
		return taskErr
	}
	req, err := relaycommon.GetTaskRequest(c)
	if err != nil {
		return nil
	}

	// 时长支持矩阵校验（三态）
	duration, outcome := ResolveSeedanceDurationEx(&req)
	switch outcome {
	case DurationParseUnparsable:
		return service.TaskErrorWrapperLocal(
			fmt.Errorf("seedance duration must be a positive integer within the supported range"),
			"invalid_seedance_duration", http.StatusBadRequest)
	case DurationParseOK:
		if !SeedanceSupportedDurationsForModel(info.OriginModelName)[duration] {
			return service.TaskErrorWrapperLocal(
				fmt.Errorf("seedance duration must be one of %v seconds (got %d)",
					SeedanceSupportedDurationListForModel(info.OriginModelName), duration),
				"invalid_seedance_duration", http.StatusBadRequest)
		}
	}

	// 分辨率支持矩阵校验（未知格式 400；有表模型显式档缺失 400；
	// 完整 (分辨率档, hasVideo) 组合校验，与结算倍率取用同一组合）
	modelVersion := ResolveSeedancePriceModel(info.OriginModelName, info.GetUpstreamModelName())
	resolution := metadataStringValue(req.Metadata, "resolution")
	hasVideo := hasVideoInMetadata(req.Metadata)
	if msg := ValidateSeedanceResolutionForModel(modelVersion, resolution, hasVideo); msg != "" {
		return service.TaskErrorWrapperLocal(errors.New(msg), "invalid_seedance_resolution", http.StatusBadRequest)
	}
	return nil
}

// BuildRequestURL constructs the upstream URL.
func (a *TaskAdaptor) BuildRequestURL(_ *relaycommon.RelayInfo) (string, error) {
	return fmt.Sprintf("%s/api/v3/contents/generations/tasks", a.baseURL), nil
}

// BuildRequestHeader sets required headers.
func (a *TaskAdaptor) BuildRequestHeader(_ *gin.Context, req *http.Request, info *relaycommon.RelayInfo) error {
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+a.apiKey)
	// X-Client-Request-Id：火山方舟文档声明的自定义请求头，仅用于串联客户端/
	// 服务端日志与问题排查，不具备幂等语义（不得将其描述为幂等键）。
	// 同一逻辑任务的每次重试复用同一值，全新任务生成新值。
	if clientRequestID := info.EnsureTaskClientRequestID(); clientRequestID != "" {
		req.Header.Set("X-Client-Request-Id", clientRequestID)
	}
	// 注意：不再发送 Idempotency-Key —— 方舟创建视频任务接口未声明支持该请求头，
	// 服务端不会据此去重；重复 POST 仍可能创建多个任务。结果未知时由客户端
	// 停止自动重试并进入恢复流程（outcome_unknown，见 controller/relay.go）。
	return nil
}

// EstimateBilling 根据请求 metadata 中的输出分辨率与是否包含视频输入，返回
// Seedance 任务的**真实结算倍率** OtherRatio（"size" = 请求实际组合单价 /
// 基准组合单价，允许 < 1，如 28/46、31/46、26/46、16/46），驱动预扣与真实
// Token 结算。时长预扣缓冲**不**在此返回（由 PreConsumeMultiplier 单独承载，
// 绝不进入 totalTokens 结算）。
//
// 契约见 constants.go 文件头注释：参数校验已在 ValidateRequestAndSetAction
// 先行完成（未知分辨率/非法时长/价格表缺失组合均 400），此处为防御性兜底。
func (a *TaskAdaptor) EstimateBilling(c *gin.Context, info *relaycommon.RelayInfo) map[string]float64 {
	req, err := relaycommon.GetTaskRequest(c)
	if err != nil {
		return nil
	}
	hasVideo := hasVideoInMetadata(req.Metadata)
	resolution := metadataStringValue(req.Metadata, "resolution")
	duration, _ := ResolveSeedanceDurationEx(&req)
	modelVersion := ResolveSeedancePriceModel(info.OriginModelName, info.GetUpstreamModelName())

	estimate := EstimateSeedancePricing(SeedanceBillingParams{
		Model:      modelVersion,
		Resolution: resolution,
		Duration:   duration,
		HasVideo:   hasVideo,
		UsePrice:   info.PriceData.UsePrice,
	})
	return estimate.PricingRatios
}

// PreConsumeMultiplier 返回仅预扣缓冲（时长 duration/5 × 无表模型保守系数）。
// 只用于提交前保守预留，**绝不**进入真实 Token 结算（结算倍率只来自
// EstimateBilling 返回的定价倍率）。固定价格模式（UsePrice）费用固定、
// 无超支风险，返回 1.0（预扣 = 实际价，结算跳过，无多收）。
func (a *TaskAdaptor) PreConsumeMultiplier(c *gin.Context, info *relaycommon.RelayInfo) float64 {
	req, err := relaycommon.GetTaskRequest(c)
	if err != nil {
		return 1.0
	}
	hasVideo := hasVideoInMetadata(req.Metadata)
	resolution := metadataStringValue(req.Metadata, "resolution")
	duration, _ := ResolveSeedanceDurationEx(&req)
	modelVersion := ResolveSeedancePriceModel(info.OriginModelName, info.GetUpstreamModelName())
	estimate := EstimateSeedancePricing(SeedanceBillingParams{
		Model:      modelVersion,
		Resolution: resolution,
		Duration:   duration,
		HasVideo:   hasVideo,
		UsePrice:   info.PriceData.UsePrice,
	})
	return estimate.PreConsumeMultiplier
}

// hasVideoInMetadata 直接检查 metadata 的 content 数组是否包含 video_url 条目，
// 避免构建完整的上游 requestPayload。
func hasVideoInMetadata(metadata map[string]interface{}) bool {
	if metadata == nil {
		return false
	}
	contentRaw, ok := metadata["content"]
	if !ok {
		return false
	}
	contentSlice, ok := contentRaw.([]interface{})
	if !ok {
		return false
	}
	for _, item := range contentSlice {
		itemMap, ok := item.(map[string]interface{})
		if !ok {
			continue
		}
		if itemMap["type"] == "video_url" {
			return true
		}
		if _, has := itemMap["video_url"]; has {
			return true
		}
	}
	return false
}

// BuildRequestBody converts request into Doubao specific format.
func (a *TaskAdaptor) BuildRequestBody(c *gin.Context, info *relaycommon.RelayInfo) (io.Reader, error) {
	req, err := relaycommon.GetTaskRequest(c)
	if err != nil {
		return nil, err
	}

	body, err := a.convertToRequestPayload(&req)
	if err != nil {
		return nil, errors.Wrap(err, "convert request payload failed")
	}
	if info.IsModelMapped {
		body.Model = info.UpstreamModelName
	} else {
		info.UpstreamModelName = body.Model
	}
	data, err := common.Marshal(body)
	if err != nil {
		return nil, err
	}
	return bytes.NewReader(data), nil
}

// DoRequest delegates to common helper.
func (a *TaskAdaptor) DoRequest(c *gin.Context, info *relaycommon.RelayInfo, requestBody io.Reader) (*http.Response, error) {
	return channel.DoTaskApiRequest(a, c, info, requestBody)
}

// DoResponse handles upstream response, returns taskID etc.
func (a *TaskAdaptor) DoResponse(c *gin.Context, resp *http.Response, info *relaycommon.RelayInfo) (taskID string, taskData []byte, taskErr *taskdto.TaskError) {
	responseBody, err := io.ReadAll(resp.Body)
	if err != nil {
		taskErr = service.TaskErrorWrapper(err, "read_response_body_failed", http.StatusInternalServerError)
		return
	}
	_ = resp.Body.Close()

	// Parse Doubao response
	var dResp responsePayload
	if err := common.Unmarshal(responseBody, &dResp); err != nil {
		taskErr = service.TaskErrorWrapper(errors.Wrapf(err, "body: %s", responseBody), "unmarshal_response_body_failed", http.StatusInternalServerError)
		return
	}

	if dResp.ID == "" {
		taskErr = service.TaskErrorWrapper(fmt.Errorf("task_id is empty"), "invalid_response", http.StatusInternalServerError)
		return
	}

	ov := dto.NewOpenAIVideo()
	ov.ID = info.PublicTaskID
	ov.TaskID = info.PublicTaskID
	ov.CreatedAt = time.Now().Unix()
	ov.Model = info.OriginModelName

	c.JSON(http.StatusOK, ov)
	return dResp.ID, responseBody, nil
}

// FetchTask fetch task status
func (a *TaskAdaptor) FetchTask(baseUrl, key string, body map[string]any, proxy string) (*http.Response, error) {
	taskID, ok := body["task_id"].(string)
	if !ok {
		return nil, fmt.Errorf("invalid task_id")
	}

	uri := fmt.Sprintf("%s/api/v3/contents/generations/tasks/%s", baseUrl, taskID)

	req, err := http.NewRequest(http.MethodGet, uri, nil)
	if err != nil {
		return nil, err
	}

	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+key)

	client, err := service.GetHttpClientWithProxy(proxy)
	if err != nil {
		return nil, fmt.Errorf("new proxy http client failed: %w", err)
	}
	return client.Do(req)
}

// flexibleInt64 兼容上游返回的字符串或整数时间戳。
type flexibleInt64 int64

func (f *flexibleInt64) UnmarshalJSON(b []byte) error {
	if len(b) == 0 || string(b) == "null" {
		*f = 0
		return nil
	}
	var n int64
	if err := common.Unmarshal(b, &n); err == nil {
		*f = flexibleInt64(n)
		return nil
	}
	var s string
	if err := common.Unmarshal(b, &s); err == nil {
		if v, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64); err == nil {
			*f = flexibleInt64(v)
			return nil
		}
	}
	return fmt.Errorf("cannot parse int64 from %s", string(b))
}

// TaskListItem 查询内容生成任务列表接口返回的任务项（候选发现所需字段）。
type TaskListItem struct {
	ID        string        `json:"id"`
	Model     string        `json:"model"`
	Status    string        `json:"status"`
	CreatedAt flexibleInt64 `json:"created_at"`
	Content   []ContentItem `json:"content"`
}

type taskListResponse struct {
	Items []TaskListItem `json:"items"`
	Total int            `json:"total"`
}

// ListTasks 查询内容生成任务列表（恢复流程的"候选发现"用）。
// body 支持 model（filter.model）与 page_size；时间窗过滤由调用方对
// created_at 做客户端侧判断（不依赖未声明的服务端参数）。
// 注意：该查询只用于候选发现/确认，不能替代服务端幂等。
func (a *TaskAdaptor) ListTasks(baseUrl, key string, body map[string]any, proxy string) (*http.Response, error) {
	uri := fmt.Sprintf("%s/api/v3/contents/generations/tasks", baseUrl)
	req, err := http.NewRequest(http.MethodGet, uri, nil)
	if err != nil {
		return nil, err
	}
	q := req.URL.Query()
	if model, ok := body["model"].(string); ok && model != "" {
		q.Set("filter.model", model)
	}
	pageSize := 100
	if ps, ok := body["page_size"].(int); ok && ps > 0 {
		pageSize = ps
	}
	q.Set("page_size", strconv.Itoa(pageSize))
	req.URL.RawQuery = q.Encode()

	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+key)

	client, err := service.GetHttpClientWithProxy(proxy)
	if err != nil {
		return nil, fmt.Errorf("new proxy http client failed: %w", err)
	}
	return client.Do(req)
}

// ParseTaskList 解析查询列表响应为任务项。
func ParseTaskList(respBody []byte) ([]TaskListItem, error) {
	var resp taskListResponse
	if err := common.Unmarshal(respBody, &resp); err != nil {
		return nil, err
	}
	return resp.Items, nil
}

// ContentFingerprint 计算上游任务项的内容指纹，与客户端 SubmitContentFingerprint
// 使用相同的规范（model + 文本 + 图片数量），仅用于候选发现。
func (item TaskListItem) ContentFingerprint() string {
	var texts []string
	imageCount := 0
	for _, c := range item.Content {
		switch c.Type {
		case "text":
			texts = append(texts, c.Text)
		case "image_url":
			imageCount++
		}
	}
	return relaycommon.ContentFingerprintFromParts(item.Model, texts, imageCount)
}

func (a *TaskAdaptor) GetModelList() []string {
	return ModelList
}

func (a *TaskAdaptor) GetChannelName() string {
	return ChannelName
}

func (a *TaskAdaptor) convertToRequestPayload(req *relaycommon.TaskSubmitReq) (*requestPayload, error) {
	r := requestPayload{
		Model:   req.Model,
		Content: []ContentItem{},
	}

	// Add images if present
	if req.HasImage() {
		for _, imgURL := range req.Images {
			r.Content = append(r.Content, ContentItem{
				Type: "image_url",
				ImageURL: &MediaURL{
					URL: imgURL,
				},
			})
		}
	}

	metadata := req.Metadata
	if err := taskcommon.UnmarshalMetadata(metadata, &r); err != nil {
		return nil, errors.Wrap(err, "unmarshal metadata failed")
	}

	if sec, _ := strconv.Atoi(req.Seconds); sec > 0 {
		r.Duration = lo.ToPtr(dto.IntValue(sec))
	}

	r.Content = lo.Reject(r.Content, func(c ContentItem, _ int) bool { return c.Type == "text" })
	r.Content = append(r.Content, ContentItem{
		Type: "text",
		Text: req.Prompt,
	})

	return &r, nil
}

func (a *TaskAdaptor) ParseTaskResult(respBody []byte) (*relaycommon.TaskInfo, error) {
	resTask := responseTask{}
	if err := common.Unmarshal(respBody, &resTask); err != nil {
		return nil, errors.Wrap(err, "unmarshal task result failed")
	}

	taskResult := relaycommon.TaskInfo{
		Code: 0,
	}

	// Map Doubao status to internal status
	switch resTask.Status {
	case "pending", "queued":
		taskResult.Status = model.TaskStatusQueued
		taskResult.Progress = "10%"
	case "processing", "running":
		taskResult.Status = model.TaskStatusInProgress
		taskResult.Progress = "50%"
	case "succeeded":
		taskResult.Status = model.TaskStatusSuccess
		taskResult.Progress = "100%"
		taskResult.Url = resTask.Content.VideoURL
		// 解析 usage 信息用于按倍率计费
		taskResult.CompletionTokens = resTask.Usage.CompletionTokens
		taskResult.TotalTokens = resTask.Usage.TotalTokens
	case "failed":
		taskResult.Status = model.TaskStatusFailure
		taskResult.Progress = "100%"
		taskResult.Reason = resTask.Error.Message
	default:
		// Unknown status, treat as processing
		taskResult.Status = model.TaskStatusInProgress
		taskResult.Progress = "30%"
	}

	return &taskResult, nil
}

func (a *TaskAdaptor) ConvertToOpenAIVideo(originTask *model.Task) ([]byte, error) {
	var dResp responseTask
	if err := common.Unmarshal(originTask.Data, &dResp); err != nil {
		return nil, errors.Wrap(err, "unmarshal doubao task data failed")
	}

	openAIVideo := dto.NewOpenAIVideo()
	openAIVideo.ID = originTask.TaskID
	openAIVideo.TaskID = originTask.TaskID
	openAIVideo.Status = originTask.Status.ToVideoStatus()
	openAIVideo.SetProgressStr(originTask.Progress)
	// Seedance 资源禁止在接口响应中返回可绕过本地鉴权的上游直链：
	// metadata.url 一律指向本服务鉴权代理入口（/v1/videos/:task_id/content），
	// 由代理侧校验任务所有权后再从上游取流转发。
	openAIVideo.SetMetadata("url", taskcommon.BuildProxyURL(originTask.TaskID))
	openAIVideo.CreatedAt = originTask.CreatedAt
	openAIVideo.CompletedAt = originTask.UpdatedAt
	openAIVideo.Model = originTask.Properties.OriginModelName

	if dResp.Status == "failed" {
		// 上游错误文本可能携带资源直链/域名/IP（例如指向失败结果的查询地址），
		// 一律按敏感信息掩码处理，不向客户端泄露可直接访问的上游地址。
		openAIVideo.Error = &dto.OpenAIVideoError{
			Message: common.MaskSensitiveInfo(dResp.Error.Message),
			Code:    common.MaskSensitiveInfo(dResp.Error.Code),
		}
	}

	return common.Marshal(openAIVideo)
}
