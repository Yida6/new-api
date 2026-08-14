package relay

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/relay/channel"
	"github.com/QuantumNous/new-api/relay/channel/task/taskcommon"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	relayconstant "github.com/QuantumNous/new-api/relay/constant"
	"github.com/QuantumNous/new-api/relay/helper"
	"github.com/QuantumNous/new-api/service"
	"github.com/gin-gonic/gin"
)

type TaskSubmitResult struct {
	UpstreamTaskID string
	TaskData       []byte
	Platform       constant.TaskPlatform
	Quota          int
	//PerCallPrice   types.PriceData
}

// PreConsumeMultiplierProvider 由需要"仅预扣缓冲"的任务适配器（当前仅
// Seedance/doubao）实现：返回提交前保守预留的额外倍率（时长缓冲、无价格表
// 模型保守系数）。该倍率只放大预扣，**绝不**写入 OtherRatios，因此不会进入
// 真实 Token 结算（见 relay/relay_task.go 第 6 步与 computeTaskQuotaByTokens）。
type PreConsumeMultiplierProvider interface {
	PreConsumeMultiplier(c *gin.Context, info *relaycommon.RelayInfo) float64
}

// ResolveOriginTask 处理基于已有任务的提交（remix / continuation）：
// 查找原始任务、从中提取模型名称、将渠道锁定到原始任务的渠道
// （通过 info.LockedChannel，重试时复用同一渠道并轮换 key），
// 以及提取 OtherRatios（时长、分辨率）。
// 该函数在控制器的重试循环之前调用一次，其结果通过 info 字段和上下文持久化。
func ResolveOriginTask(c *gin.Context, info *relaycommon.RelayInfo) *dto.TaskError {
	// 检测 remix action
	path := c.Request.URL.Path
	if strings.Contains(path, "/v1/videos/") && strings.HasSuffix(path, "/remix") {
		info.Action = constant.TaskActionRemix
	}

	// 提取 remix 任务的 video_id
	if info.Action == constant.TaskActionRemix {
		videoID := c.Param("video_id")
		if strings.TrimSpace(videoID) == "" {
			return service.TaskErrorWrapperLocal(fmt.Errorf("video_id is required"), "invalid_request", http.StatusBadRequest)
		}
		info.OriginTaskID = videoID
	}

	if info.OriginTaskID == "" {
		return nil
	}

	// 查找原始任务
	originTask, exist, err := model.GetByTaskId(info.UserId, info.OriginTaskID)
	if err != nil {
		return service.TaskErrorWrapper(err, "get_origin_task_failed", http.StatusInternalServerError)
	}
	if !exist {
		return service.TaskErrorWrapperLocal(errors.New("task_origin_not_exist"), "task_not_exist", http.StatusBadRequest)
	}

	// 从原始任务推导模型名称
	if info.OriginModelName == "" {
		if originTask.Properties.OriginModelName != "" {
			info.OriginModelName = originTask.Properties.OriginModelName
		} else if originTask.Properties.UpstreamModelName != "" {
			info.OriginModelName = originTask.Properties.UpstreamModelName
		} else {
			var taskData map[string]interface{}
			_ = common.Unmarshal(originTask.Data, &taskData)
			if m, ok := taskData["model"].(string); ok && m != "" {
				info.OriginModelName = m
			}
		}
	}

	// 锁定到原始任务的渠道（重试时复用同一渠道，轮换 key）
	ch, err := model.GetChannelById(originTask.ChannelId, true)
	if err != nil {
		return service.TaskErrorWrapperLocal(err, "channel_not_found", http.StatusBadRequest)
	}
	if ch.Status != common.ChannelStatusEnabled {
		return service.TaskErrorWrapperLocal(errors.New("the channel of the origin task is disabled"), "task_channel_disable", http.StatusBadRequest)
	}
	info.LockedChannel = ch

	if originTask.ChannelId != info.ChannelId {
		key, _, newAPIError := ch.GetNextEnabledKey()
		if newAPIError != nil {
			return service.TaskErrorWrapper(newAPIError, "channel_no_available_key", newAPIError.StatusCode)
		}
		common.SetContextKey(c, constant.ContextKeyChannelKey, key)
		common.SetContextKey(c, constant.ContextKeyChannelType, ch.Type)
		common.SetContextKey(c, constant.ContextKeyChannelBaseUrl, ch.GetBaseURL())
		common.SetContextKey(c, constant.ContextKeyChannelId, originTask.ChannelId)

		info.ChannelBaseUrl = ch.GetBaseURL()
		info.ChannelId = originTask.ChannelId
		info.ChannelType = ch.Type
		info.ApiKey = key
	}

	// 提取 remix 参数（分辨率等定价倍率 → OtherRatios）。
	// 注意：**时长（seconds）不作为结算倍率**——token 数已隐含时长，且时长
	// 预扣缓冲由 doubao adaptor 的 PreConsumeMultiplier 单独承载；若把 seconds
	// 写进 OtherRatios，轮询阶段按 totalTokens 结算时会再次乘上时长倍率，
	// 造成重复计费。旧任务的 BillingContext.OtherRatios 若含 seconds（历史
	// 数据），复制时显式过滤。
	if info.Action == constant.TaskActionRemix {
		if originTask.PrivateData.BillingContext != nil {
			// 新的 remix 逻辑：直接从原始任务的 BillingContext 中提取 OtherRatios
			// （仅定价倍率，过滤时长缓冲）
			for s, f := range originTask.PrivateData.BillingContext.OtherRatios {
				if s == "seconds" {
					continue
				}
				info.PriceData.AddOtherRatio(s, f)
			}
		} else {
			// 旧的 remix 逻辑：直接从 task data 解析 size（如果存在）
			var taskData map[string]interface{}
			_ = common.Unmarshal(originTask.Data, &taskData)
			sizeStr, _ := taskData["size"].(string)
			info.PriceData.AddOtherRatio("size", 1)
			if sizeStr == "1792x1024" || sizeStr == "1024x1792" {
				info.PriceData.AddOtherRatio("size", 1.666667)
			}
		}
	}

	return nil
}

// RelayTaskSubmit 完成 task 提交的全部流程（每次尝试调用一次）：
// 刷新渠道元数据 → 确定 platform/adaptor → 验证请求 →
// 估算计费(EstimateBilling) → 计算价格 → 预扣费（仅首次）→
// 构建/发送/解析上游请求 → 提交后计费调整(AdjustBillingOnSubmit)。
// 控制器负责 defer Refund 和成功后 Settle。
func RelayTaskSubmit(c *gin.Context, info *relaycommon.RelayInfo) (*TaskSubmitResult, *dto.TaskError) {
	info.InitChannelMeta(c)

	// 1. 确定 platform → 创建适配器 → 验证请求
	platform := constant.TaskPlatform(c.GetString("platform"))
	if platform == "" {
		platform = GetTaskPlatform(c)
	}
	adaptor := GetTaskAdaptor(platform)
	if adaptor == nil {
		return nil, service.TaskErrorWrapperLocal(fmt.Errorf("invalid api platform: %s", platform), "invalid_api_platform", http.StatusBadRequest)
	}
	adaptor.Init(info)

	// 2. 确定模型名称（对外公开名；remix 等 action 由 action 推导）
	modelName := info.OriginModelName
	if modelName == "" {
		modelName = service.CoverTaskActionToModelName(platform, info.Action)
	}
	info.OriginModelName = modelName
	info.UpstreamModelName = modelName

	// 2.5 应用渠道的模型映射（与同步任务对齐）。
	// 必须在适配器参数校验（ValidateRequestAndSetAction）**之前**完成：
	// Seedance 的价格表解析同时依赖公开别名与映射后的上游模型版本，校验与
	// 估算必须使用同一映射结果，否则"经渠道映射到规范版本/无表模型"的请求
	// 会在校验阶段被当作无表模型放行（如 fast 模型缺失的 1080p 档），
	// 违反"价格表缺失组合发送上游前必须 400"的契约。
	if err := helper.ModelMappedHelper(c, info, nil); err != nil {
		return nil, service.TaskErrorWrapperLocal(err, "model_mapping_failed", http.StatusBadRequest)
	}

	if taskErr := adaptor.ValidateRequestAndSetAction(c, info); taskErr != nil {
		return nil, taskErr
	}

	// 3. 预生成公开 task ID（仅首次）
	if info.PublicTaskID == "" {
		info.PublicTaskID = model.GenerateTaskID()
	}

	// 4. 价格计算：基础模型价格
	info.OriginModelName = modelName
	priceData, err := helper.ModelPriceHelperPerCall(c, info)
	if err != nil {
		return nil, service.TaskErrorWrapper(err, "model_price_error", http.StatusBadRequest)
	}
	info.PriceData = priceData

	// 5. 计费估算：让适配器根据用户请求提供 OtherRatios（时长、分辨率等）
	//    必须在 ModelPriceHelperPerCall 之后调用（它会重建 PriceData）。
	//    ResolveOriginTask 可能已在 remix 路径中预设了 OtherRatios，此处合并。
	if estimatedRatios := adaptor.EstimateBilling(c, info); len(estimatedRatios) > 0 {
		for k, v := range estimatedRatios {
			info.PriceData.AddOtherRatio(k, v)
		}
	}

	// 6. 将 OtherRatios（定价倍率）应用到基础额度，再叠加仅预扣缓冲
	//    （PreConsumeMultiplier：时长/无表模型保守系数，只用于预留、绝不进入
	//    真实结算），饱和转换防止溢出成负数。
	if !common.StringsContains(constant.TaskPricePatches, modelName) {
		quotaWithRatios := info.PriceData.ApplyOtherRatiosToFloat(float64(info.PriceData.Quota))
		if provider, ok := adaptor.(PreConsumeMultiplierProvider); ok {
			if m := provider.PreConsumeMultiplier(c, info); m > 1.0 {
				quotaWithRatios *= m
				info.PriceData.PreConsumeMultiplier = m
			}
		}
		quota, clamp := common.QuotaFromFloatChecked(quotaWithRatios)
		info.PriceData.Quota = quota
		noteTaskQuotaClamp(info, clamp)
	}

	// 6.5 全站 Seedance 成本保护：在任何上游请求字节发出之前原子预留
	// 当日预计成本。轮询/结算路径不经过此处，所以已提交任务不受熔断影响。
	if relaycommon.IsStrictIdempotencyChannel(info.ChannelType) && info.SeedanceCostMicros == 0 {
		costMicros := service.EstimateSeedanceUpstreamCostMicros(info.PriceData)
		reservation, reserveErr := service.ReserveSeedanceCost(costMicros)
		if reserveErr != nil {
			return nil, service.TaskErrorWrapperLocal(reserveErr, "seedance_cost_check_failed", http.StatusServiceUnavailable)
		}
		if !reservation.Allowed {
			return nil, service.TaskErrorWrapperLocal(errors.New("Seedance 全站成本保护已触发，当前暂不接受新任务。已提交任务不受影响并会继续处理，请稍后再试或联系管理员。"), "SEEDANCE_COST_CIRCUIT_OPEN", http.StatusServiceUnavailable)
		}
		info.SeedanceCostPeriod = reservation.Period
		info.SeedanceCostMicros = costMicros
	}

	// 7. 预扣费（仅首次 — 重试时 info.Billing 已存在，跳过）
	if info.Billing == nil && !info.PriceData.FreeModel {
		info.ForcePreConsume = true
		if apiErr := service.PreConsumeBilling(c, info.PriceData.Quota, info); apiErr != nil {
			return nil, service.TaskErrorFromAPIError(apiErr)
		}
	}

	// 8. 构建请求体
	requestBody, err := adaptor.BuildRequestBody(c, info)
	if err != nil {
		return nil, service.TaskErrorWrapper(err, "build_request_failed", http.StatusInternalServerError)
	}

	// 8.5 确保本次创建请求的 X-Client-Request-Id（日志串联/排查用，非幂等键）。
	// 同一逻辑任务的每次重试复用同一值；全新任务生成新值。
	info.EnsureTaskClientRequestID()

	// 9. 发送请求
	resp, err := adaptor.DoRequest(c, info, requestBody)
	if err != nil {
		// 对结果分类：发送前失败可安全重试；读取响应超时/连接中断等一律视为
		// 结果未知（服务端可能已创建任务），禁止自动重发。
		outcome := relaycommon.ClassifySubmitError(err)
		if info.TaskRelayInfo != nil {
			info.TaskRelayInfo.SubmitOutcome = outcome
		}
		if outcome == relaycommon.TaskSubmitOutcomeOutcomeUnknown {
			info.SeedanceCostCommitted = true
			return nil, service.TaskErrorWrapper(err, "task_submit_outcome_unknown", http.StatusBadGateway)
		}
		return nil, service.TaskErrorWrapper(err, "do_request_failed", http.StatusInternalServerError)
	}
	if resp != nil && resp.StatusCode != http.StatusOK {
		// 明确收到可判定失败的响应（4xx/5xx 错误体）→ confirmed_failure，不自动重发。
		if info.TaskRelayInfo != nil {
			info.TaskRelayInfo.SubmitOutcome = relaycommon.TaskSubmitOutcomeConfirmedFailure
		}
		responseBody, _ := io.ReadAll(resp.Body)
		return nil, service.TaskErrorWrapper(fmt.Errorf("%s", string(responseBody)), "fail_to_fetch_task", resp.StatusCode)
	}

	// 10. 返回 OtherRatios 给下游（header 必须在 DoResponse 写 body 之前设置）
	otherRatios := info.PriceData.OtherRatios()
	if otherRatios == nil {
		otherRatios = map[string]float64{}
	}
	ratiosJSON, _ := common.Marshal(otherRatios)
	c.Header("X-New-Api-Other-Ratios", string(ratiosJSON))

	// 11. 解析响应
	upstreamTaskID, taskData, taskErr := adaptor.DoResponse(c, resp, info)
	if taskErr != nil {
		// 收到 200 但无法解析出上游 task_id：服务端很可能已创建任务但本地拿不到
		// 标识，无法用 filter.task_ids 精确查询 → 结果未知，禁止自动重发。
		if info.TaskRelayInfo != nil {
			info.TaskRelayInfo.SubmitOutcome = relaycommon.TaskSubmitOutcomeOutcomeUnknown
			if relaycommon.IsStrictIdempotencyChannel(info.ChannelType) {
				info.TaskRelayInfo.SeedanceCostCommitted = true
			}
		}
		return nil, taskErr
	}
	if info.TaskRelayInfo != nil {
		info.TaskRelayInfo.SubmitOutcome = relaycommon.TaskSubmitOutcomeConfirmedSuccess
		if relaycommon.IsStrictIdempotencyChannel(info.ChannelType) {
			info.TaskRelayInfo.SeedanceCostCommitted = true
		}
	}

	// 11. 提交后计费调整：让适配器根据上游实际返回调整 OtherRatios
	finalQuota := info.PriceData.Quota
	if adjustedRatios := adaptor.AdjustBillingOnSubmit(info, taskData); len(adjustedRatios) > 0 {
		if adjustedQuota, ok := recalcQuotaFromRatios(info, adjustedRatios); ok {
			// 基于调整后的 ratios 重新计算 quota
			finalQuota = adjustedQuota
			info.PriceData.ReplaceOtherRatios(adjustedRatios)
			info.PriceData.Quota = finalQuota
		}
	}

	return &TaskSubmitResult{
		UpstreamTaskID: upstreamTaskID,
		TaskData:       taskData,
		Platform:       platform,
		Quota:          finalQuota,
	}, nil
}

// recalcQuotaFromRatios 根据 adjustedRatios 重新计算 quota。
// 公式: baseQuota × ∏(ratio) — 其中 baseQuota 是不含 OtherRatios 的基础额度。
func recalcQuotaFromRatios(info *relaycommon.RelayInfo, ratios map[string]float64) (int, bool) {
	// 从 PriceData 获取不含 OtherRatios 的基础价格
	baseQuota := info.PriceData.RemoveOtherRatiosFromFloat(float64(info.PriceData.Quota))
	priceData := info.PriceData
	if !priceData.ReplaceOtherRatios(ratios) {
		return 0, false
	}
	// 应用新的 ratios
	result := priceData.ApplyOtherRatiosToFloat(baseQuota)
	quota, clamp := common.QuotaFromFloatChecked(result)
	noteTaskQuotaClamp(info, clamp)
	return quota, true
}

// noteTaskQuotaClamp records the first quota saturation event onto the task's
// RelayInfo so LogTaskConsumption can surface it on the submit log's
// admin_info. First non-nil clamp wins.
func noteTaskQuotaClamp(info *relaycommon.RelayInfo, clamp *common.QuotaClamp) {
	if clamp == nil || info == nil {
		return
	}
	if info.QuotaClamp == nil {
		info.QuotaClamp = clamp
	}
}

var fetchRespBuilders = map[int]func(c *gin.Context) (respBody []byte, taskResp *dto.TaskError){
	relayconstant.RelayModeSunoFetchByID:  sunoFetchByIDRespBodyBuilder,
	relayconstant.RelayModeSunoFetch:      sunoFetchRespBodyBuilder,
	relayconstant.RelayModeVideoFetchByID: videoFetchByIDRespBodyBuilder,
}

func RelayTaskFetch(c *gin.Context, relayMode int) (taskResp *dto.TaskError) {
	respBuilder, ok := fetchRespBuilders[relayMode]
	if !ok {
		taskResp = service.TaskErrorWrapperLocal(errors.New("invalid_relay_mode"), "invalid_relay_mode", http.StatusBadRequest)
	}

	respBody, taskErr := respBuilder(c)
	if taskErr != nil {
		return taskErr
	}
	if len(respBody) == 0 {
		respBody = []byte("{\"code\":\"success\",\"data\":null}")
	}

	c.Writer.Header().Set("Content-Type", "application/json")
	_, err := io.Copy(c.Writer, bytes.NewBuffer(respBody))
	if err != nil {
		taskResp = service.TaskErrorWrapper(err, "copy_response_body_failed", http.StatusInternalServerError)
		return
	}
	return
}

func sunoFetchRespBodyBuilder(c *gin.Context) (respBody []byte, taskResp *dto.TaskError) {
	userId := c.GetInt("id")
	var condition = struct {
		IDs    []any  `json:"ids"`
		Action string `json:"action"`
	}{}
	err := c.BindJSON(&condition)
	if err != nil {
		taskResp = service.TaskErrorWrapper(err, "invalid_request", http.StatusBadRequest)
		return
	}
	var tasks []any
	if len(condition.IDs) > 0 {
		taskModels, err := model.GetByTaskIds(userId, condition.IDs)
		if err != nil {
			taskResp = service.TaskErrorWrapper(err, "get_tasks_failed", http.StatusInternalServerError)
			return
		}
		for _, task := range taskModels {
			tasks = append(tasks, TaskModel2Dto(task))
		}
	} else {
		tasks = make([]any, 0)
	}
	respBody, err = common.Marshal(dto.TaskResponse[[]any]{
		Code: "success",
		Data: tasks,
	})
	return
}

func sunoFetchByIDRespBodyBuilder(c *gin.Context) (respBody []byte, taskResp *dto.TaskError) {
	taskId := c.Param("id")
	userId := c.GetInt("id")

	originTask, exist, err := model.GetByTaskId(userId, taskId)
	if err != nil {
		taskResp = service.TaskErrorWrapper(err, "get_task_failed", http.StatusInternalServerError)
		return
	}
	if !exist {
		taskResp = service.TaskErrorWrapperLocal(errors.New("task_not_exist"), "task_not_exist", http.StatusBadRequest)
		return
	}

	respBody, err = common.Marshal(dto.TaskResponse[any]{
		Code: "success",
		Data: TaskModel2Dto(originTask),
	})
	return
}

func videoFetchByIDRespBodyBuilder(c *gin.Context) (respBody []byte, taskResp *dto.TaskError) {
	taskId := c.Param("task_id")
	if taskId == "" {
		taskId = c.GetString("task_id")
	}
	userId := c.GetInt("id")

	originTask, exist, err := model.GetByTaskId(userId, taskId)
	if err != nil {
		taskResp = service.TaskErrorWrapper(err, "get_task_failed", http.StatusInternalServerError)
		return
	}
	if !exist {
		taskResp = service.TaskErrorWrapperLocal(errors.New("task_not_exist"), "task_not_exist", http.StatusBadRequest)
		return
	}

	isOpenAIVideoAPI := strings.HasPrefix(c.Request.RequestURI, "/v1/videos/")

	// Gemini/Vertex 支持实时查询：用户 fetch 时直接从上游拉取最新状态
	if realtimeResp := tryRealtimeFetch(originTask, isOpenAIVideoAPI); len(realtimeResp) > 0 {
		respBody = realtimeResp
		return
	}

	// OpenAI Video API 格式: 走各 adaptor 的 ConvertToOpenAIVideo
	if isOpenAIVideoAPI {
		adaptor := GetTaskAdaptor(originTask.Platform)
		if adaptor == nil {
			taskResp = service.TaskErrorWrapperLocal(fmt.Errorf("invalid channel id: %d", originTask.ChannelId), "invalid_channel_id", http.StatusBadRequest)
			return
		}
		if converter, ok := adaptor.(channel.OpenAIVideoConverter); ok {
			openAIVideoData, err := converter.ConvertToOpenAIVideo(originTask)
			if err != nil {
				taskResp = service.TaskErrorWrapper(err, "convert_to_openai_video_failed", http.StatusInternalServerError)
				return
			}
			respBody = openAIVideoData
			return
		}
		taskResp = service.TaskErrorWrapperLocal(fmt.Errorf("not_implemented:%s", originTask.Platform), "not_implemented", http.StatusNotImplemented)
		return
	}

	// 通用 TaskDto 格式
	respBody, err = common.Marshal(dto.TaskResponse[any]{
		Code: "success",
		Data: TaskModel2Dto(originTask),
	})
	if err != nil {
		taskResp = service.TaskErrorWrapper(err, "marshal_response_failed", http.StatusInternalServerError)
	}
	return
}

// tryRealtimeFetch 尝试从上游实时拉取 Gemini/Vertex 任务状态。
// 仅当渠道类型为 Gemini 或 Vertex 时触发；其他渠道或出错时返回 nil。
// 当非 OpenAI Video API 时，还会构建自定义格式的响应体。
func tryRealtimeFetch(task *model.Task, isOpenAIVideoAPI bool) []byte {
	channelModel, err := model.GetChannelById(task.ChannelId, true)
	if err != nil {
		return nil
	}
	if channelModel.Type != constant.ChannelTypeVertexAi && channelModel.Type != constant.ChannelTypeGemini {
		return nil
	}

	baseURL := constant.ChannelBaseURLs[channelModel.Type]
	if channelModel.GetBaseURL() != "" {
		baseURL = channelModel.GetBaseURL()
	}
	proxy := channelModel.GetSetting().Proxy
	adaptor := GetTaskAdaptor(constant.TaskPlatform(strconv.Itoa(channelModel.Type)))
	if adaptor == nil {
		return nil
	}

	resp, err := adaptor.FetchTask(baseURL, channelModel.Key, map[string]any{
		"task_id": task.GetUpstreamTaskID(),
		"action":  task.Action,
	}, proxy)
	if err != nil || resp == nil {
		return nil
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil
	}

	ti, err := adaptor.ParseTaskResult(body)
	if err != nil || ti == nil {
		return nil
	}

	snap := task.Snapshot()

	// 将上游最新状态更新到 task
	if ti.Status != "" {
		task.Status = model.TaskStatus(ti.Status)
	}
	if ti.Progress != "" {
		task.Progress = ti.Progress
	}
	if strings.HasPrefix(ti.Url, "data:") {
		// data: URI — kept in Data, not ResultURL
	} else if ti.Url != "" {
		task.PrivateData.ResultURL = ti.Url
	} else if task.Status == model.TaskStatusSuccess {
		// No URL from adaptor — construct proxy URL using public task ID
		task.PrivateData.ResultURL = taskcommon.BuildProxyURL(task.TaskID)
	}

	if !snap.Equal(task.Snapshot()) {
		_, _ = task.UpdateWithStatus(snap.Status)
	}

	// OpenAI Video API 由调用者的 ConvertToOpenAIVideo 分支处理
	if isOpenAIVideoAPI {
		return nil
	}

	// 非 OpenAI Video API: 构建自定义格式响应
	format := detectVideoFormat(body)
	out := map[string]any{
		"error":    nil,
		"format":   format,
		"metadata": nil,
		"status":   mapTaskStatusToSimple(task.Status),
		"task_id":  task.TaskID,
		"url":      task.GetResultURL(),
	}
	respBody, _ := common.Marshal(dto.TaskResponse[any]{
		Code: "success",
		Data: out,
	})
	return respBody
}

// detectVideoFormat 从 Gemini/Vertex 原始响应中探测视频格式
func detectVideoFormat(rawBody []byte) string {
	var raw map[string]any
	if err := common.Unmarshal(rawBody, &raw); err != nil {
		return "mp4"
	}
	respObj, ok := raw["response"].(map[string]any)
	if !ok {
		return "mp4"
	}
	vids, ok := respObj["videos"].([]any)
	if !ok || len(vids) == 0 {
		return "mp4"
	}
	v0, ok := vids[0].(map[string]any)
	if !ok {
		return "mp4"
	}
	mt, ok := v0["mimeType"].(string)
	if !ok || mt == "" || strings.Contains(mt, "mp4") {
		return "mp4"
	}
	return mt
}

// mapTaskStatusToSimple 将内部 TaskStatus 映射为简化状态字符串
func mapTaskStatusToSimple(status model.TaskStatus) string {
	switch status {
	case model.TaskStatusSuccess:
		return "succeeded"
	case model.TaskStatusFailure:
		return "failed"
	case model.TaskStatusQueued, model.TaskStatusSubmitted:
		return "queued"
	default:
		return "processing"
	}
}

func TaskModel2Dto(task *model.Task) *dto.TaskDto {
	resultURL := common.RedactCredentials(task.GetResultURL())
	data := redactTaskData(task.Data)
	failReason := common.RedactCredentials(task.FailReason)
	if model.IsSeedanceTaskPlatform(task.Platform) {
		// Seedance 资源禁止在接口响应中返回可绕过本地鉴权的上游直链：
		// 结果 URL 一律替换为本服务鉴权代理入口（/v1/videos/:task_id/content），
		// 存储的原始上游状态响应（含 content.video_url）同样脱敏。
		// 内部存储与轮询/结算流程仍读原始值（task.PrivateData.ResultURL 不变）。
		resultURL = taskcommon.BuildProxyURL(task.TaskID)
		data = sanitizeSeedanceTaskData(data)
		// RedactCredentials 只掩码凭证、保留 URL/域名；上游错误文本可能携带
		// 资源直链，Seedance 对外错误信息一律按敏感信息掩码（URL/域名/IP/凭证）。
		failReason = common.MaskSensitiveInfo(task.FailReason)
	}
	return &dto.TaskDto{
		ID:         task.ID,
		CreatedAt:  task.CreatedAt,
		UpdatedAt:  task.UpdatedAt,
		TaskID:     task.TaskID,
		Platform:   string(task.Platform),
		UserId:     task.UserId,
		Group:      task.Group,
		ChannelId:  task.ChannelId,
		Quota:      task.Quota,
		Action:     task.Action,
		Status:     string(task.Status),
		FailReason: failReason,
		// GetResultURL 对旧数据会回退到 FailReason（可能含上游错误原文），
		// 因此结果 URL 同样需要脱敏；正常视频地址（http/data URL）不受影响。
		ResultURL:  resultURL,
		SubmitTime: task.SubmitTime,
		StartTime:  task.StartTime,
		FinishTime: task.FinishTime,
		Progress:   task.Progress,
		// Properties 使用脱敏副本：UpstreamModelName（可能为方舟 Endpoint ID）
		// 绝不暴露给客户端，内部存储与 remix/恢复流程仍读原始值。
		Properties: task.Properties.Sanitized(),
		Username:   task.Username,
		Data:       data,
	}
}

// seedanceUpstreamURLLeafFields 是可能携带上游资源地址的字段名集合。
// Seedance 上游状态响应通过 content.video_url / content[].video_url.url 等
// 字段携带可直接访问的上游地址；命中即整体删除（含嵌套对象/数组中的同名值），
// 不依赖单一字段名。内部存储与轮询/结算流程仍读原始值，不受影响。
var seedanceUpstreamURLLeafFields = map[string]struct{}{
	"url":           {},
	"video_url":     {},
	"result_url":    {},
	"download_url":  {},
	"play_url":      {},
	"cover_url":     {},
	"image_url":     {},
	"thumbnail_url": {},
	"poster_url":    {},
}

// sanitizeSeedanceTaskData 返回 Seedance 任务存储数据中不含上游资源直链的副本。
// 采用 fail-closed 策略：
//   - 仅接受 JSON 对象（Seedance 上游状态响应的契约形状），其他任何结构
//     （数组/标量/null/非 JSON）一律返回 null；
//   - 递归重建整棵对象树，遇到任何可能携带上游资源地址的字段一律删除；
//   - 字符串值中出现 URL（含 "://"）时按敏感信息掩码，防止非 URL 字段名
//     夹带上游地址；
//   - 解析或序列化失败返回 null，绝不回退原始数据（原始数据可能含上游地址）。
func sanitizeSeedanceTaskData(data json.RawMessage) json.RawMessage {
	if len(data) == 0 {
		return data
	}
	var v any
	if err := common.Unmarshal(data, &v); err != nil {
		return json.RawMessage("null")
	}
	root, isObject := v.(map[string]any)
	if !isObject {
		// 非对象结构无法按对象语义可靠清洗，fail-closed 返回 null。
		return json.RawMessage("null")
	}
	cleaned, _ := sanitizeSeedanceValue(root)
	b, err := common.Marshal(cleaned)
	if err != nil {
		return json.RawMessage("null")
	}
	return json.RawMessage(b)
}

// sanitizeSeedanceValue 递归重建节点，返回不含上游资源直链的安全副本。
// 第二个返回值表示该节点是否还有需要保留的数据（false 表示已清空，
// 调用方应删除对应字段，避免保留空壳结构）。
func sanitizeSeedanceValue(v any) (any, bool) {
	switch node := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(node))
		for k, val := range node {
			if _, isURLField := seedanceUpstreamURLLeafFields[k]; isURLField {
				continue
			}
			cleaned, keep := sanitizeSeedanceValue(val)
			if keep {
				out[k] = cleaned
			}
		}
		if len(out) == 0 {
			return nil, false
		}
		return out, true
	case []any:
		out := make([]any, 0, len(node))
		for _, val := range node {
			cleaned, keep := sanitizeSeedanceValue(val)
			if keep {
				out = append(out, cleaned)
			}
		}
		if len(out) == 0 {
			return nil, false
		}
		return out, true
	case string:
		if strings.Contains(node, "://") {
			// 字符串值携带 URL：掩码主机/路径/查询，绝不向客户端泄露可访问地址。
			return common.MaskSensitiveInfo(node), true
		}
		return node, true
	default:
		return v, true
	}
}

// redactTaskData removes credential-like values (e.g. Ark Endpoint IDs echoed
// by the upstream in task status responses) from the stored task payload before
// it is returned to clients. Nil/empty payloads pass through untouched.
func redactTaskData(data json.RawMessage) json.RawMessage {
	if len(data) == 0 {
		return data
	}
	return json.RawMessage(common.RedactCredentials(string(data)))
}
