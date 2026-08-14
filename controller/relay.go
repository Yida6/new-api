package controller

import (
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	taskdto "github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/logger"
	"github.com/QuantumNous/new-api/middleware"
	"github.com/QuantumNous/new-api/model"
	perfmetrics "github.com/QuantumNous/new-api/pkg/perf_metrics"
	"github.com/QuantumNous/new-api/relay"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	relayconstant "github.com/QuantumNous/new-api/relay/constant"
	"github.com/QuantumNous/new-api/relay/helper"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting"
	"github.com/QuantumNous/new-api/setting/operation_setting"

	"github.com/bytedance/gopkg/util/gopool"
	"github.com/samber/lo"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
)

func relayHandler(c *gin.Context, info *relaycommon.RelayInfo) *types.NewAPIError {
	var err *types.NewAPIError
	switch info.RelayMode {
	case relayconstant.RelayModeImagesGenerations, relayconstant.RelayModeImagesEdits:
		err = relay.ImageHelper(c, info)
	case relayconstant.RelayModeAudioSpeech:
		fallthrough
	case relayconstant.RelayModeAudioTranslation:
		fallthrough
	case relayconstant.RelayModeAudioTranscription:
		err = relay.AudioHelper(c, info)
	case relayconstant.RelayModeRerank:
		err = relay.RerankHelper(c, info)
	case relayconstant.RelayModeEmbeddings:
		err = relay.EmbeddingHelper(c, info)
	case relayconstant.RelayModeResponses, relayconstant.RelayModeResponsesCompact:
		err = relay.ResponsesHelper(c, info)
	case relayconstant.RelayModeAlphaSearch:
		err = relay.AlphaSearchHelper(c, info)
	default:
		err = relay.TextHelper(c, info)
	}
	return err
}

func geminiRelayHandler(c *gin.Context, info *relaycommon.RelayInfo) *types.NewAPIError {
	var err *types.NewAPIError
	if strings.Contains(c.Request.URL.Path, "embed") {
		err = relay.GeminiEmbeddingHandler(c, info)
	} else {
		err = relay.GeminiHelper(c, info)
	}
	return err
}

func Relay(c *gin.Context, relayFormat types.RelayFormat) {

	requestId := c.GetString(common.RequestIdKey)
	//group := common.GetContextKeyString(c, constant.ContextKeyUsingGroup)
	//originalModel := common.GetContextKeyString(c, constant.ContextKeyOriginalModel)

	var (
		newAPIError *types.NewAPIError
		ws          *websocket.Conn
	)

	if relayFormat == types.RelayFormatOpenAIRealtime {
		var err error
		ws, err = upgrader.Upgrade(c.Writer, c.Request, nil)
		if err != nil {
			helper.WssError(c, ws, types.NewError(err, types.ErrorCodeGetChannelFailed, types.ErrOptionWithSkipRetry()).ToOpenAIError())
			return
		}
		defer ws.Close()
	}

	defer func() {
		if newAPIError != nil {
			logger.LogError(c, fmt.Sprintf("relay error: %s", common.LocalLogPreview(newAPIError.Error())))
			newAPIError.SetMessage(common.MessageWithRequestId(newAPIError.Error(), requestId))
			switch relayFormat {
			case types.RelayFormatOpenAIRealtime:
				helper.WssError(c, ws, newAPIError.ToOpenAIError())
			case types.RelayFormatClaude:
				c.JSON(newAPIError.StatusCode, gin.H{
					"type":  "error",
					"error": newAPIError.ToClaudeError(),
				})
			default:
				c.JSON(newAPIError.StatusCode, gin.H{
					"error": newAPIError.ToOpenAIError(),
				})
			}
		}
	}()

	request, err := helper.GetAndValidateRequest(c, relayFormat)
	if err != nil {
		// Map "request body too large" to 413 so clients can handle it correctly
		if common.IsRequestBodyTooLargeError(err) || errors.Is(err, common.ErrRequestBodyTooLarge) {
			newAPIError = types.NewErrorWithStatusCode(err, types.ErrorCodeReadRequestBodyFailed, http.StatusRequestEntityTooLarge, types.ErrOptionWithSkipRetry())
		} else {
			newAPIError = types.NewError(err, types.ErrorCodeInvalidRequest)
		}
		return
	}

	relayInfo, err := relaycommon.GenRelayInfo(c, relayFormat, request, ws)
	if err != nil {
		newAPIError = types.NewError(err, types.ErrorCodeGenRelayInfoFailed)
		return
	}

	needSensitiveCheck := setting.ShouldCheckPromptSensitive()
	needCountToken := constant.CountToken
	// Avoid building huge CombineText (strings.Join) when token counting and sensitive check are both disabled.
	var meta *types.TokenCountMeta
	if needSensitiveCheck || needCountToken {
		meta = request.GetTokenCountMeta()
	} else {
		meta = fastTokenCountMetaForPricing(request)
	}

	if needSensitiveCheck && meta != nil {
		contains, words := service.CheckSensitiveText(meta.CombineText)
		if contains {
			logger.LogWarn(c, fmt.Sprintf("user sensitive words detected: %s", strings.Join(words, ", ")))
			newAPIError = types.NewError(err, types.ErrorCodeSensitiveWordsDetected)
			return
		}
	}

	tokens, err := service.EstimateRequestToken(c, meta, relayInfo)
	if err != nil {
		newAPIError = types.NewError(err, types.ErrorCodeCountTokenFailed)
		return
	}

	relayInfo.SetEstimatePromptTokens(tokens)

	priceData, err := helper.ModelPriceHelper(c, relayInfo, tokens, meta)
	if err != nil {
		newAPIError = types.NewError(err, types.ErrorCodeModelPriceError, types.ErrOptionWithStatusCode(http.StatusBadRequest))
		return
	}

	// common.SetContextKey(c, constant.ContextKeyTokenCountMeta, meta)

	if priceData.FreeModel {
		logger.LogInfo(c, fmt.Sprintf("模型 %s 免费，跳过预扣费", relayInfo.OriginModelName))
	} else {
		newAPIError = service.PreConsumeBilling(c, priceData.QuotaToPreConsume, relayInfo)
		if newAPIError != nil {
			return
		}
	}

	defer func() {
		// Only return quota if downstream failed and quota was actually pre-consumed
		if newAPIError != nil {
			newAPIError = service.NormalizeViolationFeeError(newAPIError)
			if relayInfo.Billing != nil {
				relayInfo.Billing.Refund(c)
			}
			service.ChargeViolationFeeIfNeeded(c, relayInfo, newAPIError)
		}
	}()

	retryParam := &service.RetryParam{
		Ctx:         c,
		TokenGroup:  relayInfo.TokenGroup,
		ModelName:   relayInfo.OriginModelName,
		RequestPath: c.Request.URL.Path,
		Retry:       common.GetPointer(0),
	}
	relayInfo.RetryIndex = 0
	relayInfo.LastError = nil

	for ; retryParam.GetRetry() <= common.RetryTimes; retryParam.IncreaseRetry() {
		relayInfo.RetryIndex = retryParam.GetRetry()
		channel, channelErr := getChannel(c, relayInfo, retryParam)
		if channelErr != nil {
			logger.LogError(c, channelErr.Error())
			newAPIError = channelErr
			break
		}
		addUsedChannel(c, channel.Id)
		if billingErr := service.PrepareTieredBillingForSelectedGroup(c, relayInfo); billingErr != nil {
			newAPIError = billingErr
			break
		}

		bodyStorage, bodyErr := common.GetBodyStorage(c)
		if bodyErr != nil {
			// Ensure consistent 413 for oversized bodies even when error occurs later (e.g., retry path)
			if common.IsRequestBodyTooLargeError(bodyErr) || errors.Is(bodyErr, common.ErrRequestBodyTooLarge) {
				newAPIError = types.NewErrorWithStatusCode(bodyErr, types.ErrorCodeReadRequestBodyFailed, http.StatusRequestEntityTooLarge, types.ErrOptionWithSkipRetry())
			} else {
				newAPIError = types.NewErrorWithStatusCode(bodyErr, types.ErrorCodeReadRequestBodyFailed, http.StatusBadRequest, types.ErrOptionWithSkipRetry())
			}
			break
		}
		c.Request.Body = io.NopCloser(bodyStorage)

		switch relayFormat {
		case types.RelayFormatOpenAIRealtime:
			newAPIError = relay.WssHelper(c, relayInfo)
		case types.RelayFormatClaude:
			newAPIError = relay.ClaudeHelper(c, relayInfo)
		case types.RelayFormatGemini:
			newAPIError = geminiRelayHandler(c, relayInfo)
		default:
			newAPIError = relayHandler(c, relayInfo)
		}

		if newAPIError == nil {
			relayInfo.LastError = nil
			return
		}

		newAPIError = service.NormalizeViolationFeeError(newAPIError)
		relayInfo.LastError = newAPIError

		processChannelError(c, *types.NewChannelError(channel.Id, channel.Type, channel.Name, channel.ChannelInfo.IsMultiKey, common.GetContextKeyString(c, constant.ContextKeyChannelKey), channel.GetAutoBan()), newAPIError)

		if !shouldRetry(c, newAPIError, common.RetryTimes-retryParam.GetRetry()) {
			break
		}
	}

	useChannel := c.GetStringSlice("use_channel")
	if len(useChannel) > 1 {
		retryLogStr := fmt.Sprintf("重试：%s", strings.Trim(strings.Join(strings.Fields(fmt.Sprint(useChannel)), "->"), "[]"))
		logger.LogInfo(c, retryLogStr)
	}
	if newAPIError != nil {
		gopool.Go(func() {
			perfmetrics.RecordRelaySample(relayInfo, false, 0)
		})
	}
}

var upgrader = websocket.Upgrader{
	Subprotocols: []string{"realtime"}, // WS 握手支持的协议，如果有使用 Sec-WebSocket-Protocol，则必须在此声明对应的 Protocol TODO add other protocol
	CheckOrigin: func(r *http.Request) bool {
		return true // 允许跨域
	},
}

func addUsedChannel(c *gin.Context, channelId int) {
	useChannel := c.GetStringSlice("use_channel")
	useChannel = append(useChannel, fmt.Sprintf("%d", channelId))
	c.Set("use_channel", useChannel)
}

func fastTokenCountMetaForPricing(request dto.Request) *types.TokenCountMeta {
	if request == nil {
		return &types.TokenCountMeta{}
	}
	meta := &types.TokenCountMeta{
		TokenType: types.TokenTypeTokenizer,
	}
	switch r := request.(type) {
	case *dto.GeneralOpenAIRequest:
		maxCompletionTokens := lo.FromPtrOr(r.MaxCompletionTokens, uint(0))
		maxTokens := lo.FromPtrOr(r.MaxTokens, uint(0))
		if maxCompletionTokens > maxTokens {
			meta.MaxTokens = int(maxCompletionTokens)
		} else {
			meta.MaxTokens = int(maxTokens)
		}
	case *dto.OpenAIResponsesRequest:
		meta.MaxTokens = int(lo.FromPtrOr(r.MaxOutputTokens, uint(0)))
	case *dto.ClaudeRequest:
		meta.MaxTokens = int(lo.FromPtr(r.MaxTokens))
	case *dto.ImageRequest:
		// Pricing for image requests depends on ImagePriceRatio; safe to compute even when CountToken is disabled.
		return r.GetTokenCountMeta()
	default:
		// Best-effort: leave CombineText empty to avoid large allocations.
	}
	return meta
}

func getChannel(c *gin.Context, info *relaycommon.RelayInfo, retryParam *service.RetryParam) (*model.Channel, *types.NewAPIError) {
	if info.ChannelMeta == nil {
		autoBan := c.GetBool("auto_ban")
		autoBanInt := 1
		if !autoBan {
			autoBanInt = 0
		}
		return &model.Channel{
			Id:      c.GetInt("channel_id"),
			Type:    c.GetInt("channel_type"),
			Name:    c.GetString("channel_name"),
			AutoBan: &autoBanInt,
		}, nil
	}
	channel, selectGroup, err := service.CacheGetRandomSatisfiedChannel(retryParam)
	if err != nil {
		return nil, types.NewError(fmt.Errorf("获取分组 %s 下模型 %s 的可用渠道失败（retry）: %s", selectGroup, info.OriginModelName, err.Error()), types.ErrorCodeGetChannelFailed, types.ErrOptionWithSkipRetry())
	}
	if channel == nil {
		return nil, types.NewError(fmt.Errorf("分组 %s 下模型 %s 的可用渠道不存在（retry）", selectGroup, info.OriginModelName), types.ErrorCodeGetChannelFailed, types.ErrOptionWithSkipRetry())
	}

	info.PriceData.GroupRatioInfo = helper.HandleGroupRatio(c, info)

	newAPIError := middleware.SetupContextForSelectedChannel(c, channel, info.OriginModelName)
	if newAPIError != nil {
		return nil, newAPIError
	}
	return channel, nil
}

func shouldRetry(c *gin.Context, openaiErr *types.NewAPIError, retryTimes int) bool {
	if openaiErr == nil {
		return false
	}
	if service.ShouldSkipRetryAfterChannelAffinityFailure(c) {
		return false
	}
	if types.IsChannelError(openaiErr) {
		return true
	}
	if types.IsSkipRetryError(openaiErr) {
		return false
	}
	if retryTimes <= 0 {
		return false
	}
	if _, ok := c.Get("specific_channel_id"); ok {
		return false
	}
	code := openaiErr.StatusCode
	if code >= 200 && code < 300 {
		return false
	}
	if code < 100 || code > 599 {
		return true
	}
	if operation_setting.IsAlwaysSkipRetryCode(openaiErr.GetErrorCode()) {
		return false
	}
	return operation_setting.ShouldRetryByStatusCode(code)
}

func processChannelError(c *gin.Context, channelError types.ChannelError, err *types.NewAPIError) {
	logger.LogError(c, fmt.Sprintf("channel error (channel #%d, status code: %d): %s", channelError.ChannelId, err.StatusCode, common.LocalLogPreview(err.Error())))
	// 不要使用context获取渠道信息，异步处理时可能会出现渠道信息不一致的情况
	// do not use context to get channel info, there may be inconsistent channel info when processing asynchronously
	if service.ShouldDisableChannel(err) && channelError.AutoBan {
		gopool.Go(func() {
			service.DisableChannel(channelError, err.ErrorWithStatusCode())
		})
	}

	if constant.ErrorLogEnabled && types.IsRecordErrorLog(err) {
		// 保存错误日志到mysql中
		userId := c.GetInt("id")
		tokenName := c.GetString("token_name")
		modelName := c.GetString("original_model")
		tokenId := c.GetInt("token_id")
		userGroup := c.GetString("group")
		channelId := c.GetInt("channel_id")
		other := make(map[string]interface{})
		if c.Request != nil && c.Request.URL != nil {
			other["request_path"] = c.Request.URL.Path
		}
		other["error_type"] = err.GetErrorType()
		other["error_code"] = err.GetErrorCode()
		other["status_code"] = err.StatusCode
		other["channel_id"] = channelId
		other["channel_name"] = c.GetString("channel_name")
		other["channel_type"] = c.GetInt("channel_type")
		adminInfo := make(map[string]interface{})
		adminInfo["use_channel"] = c.GetStringSlice("use_channel")
		isMultiKey := common.GetContextKeyBool(c, constant.ContextKeyChannelIsMultiKey)
		if isMultiKey {
			adminInfo["is_multi_key"] = true
			adminInfo["multi_key_index"] = common.GetContextKeyInt(c, constant.ContextKeyChannelMultiKeyIndex)
		}
		service.AppendChannelAffinityAdminInfo(c, adminInfo)
		other["admin_info"] = adminInfo
		startTime := common.GetContextKeyTime(c, constant.ContextKeyRequestStartTime)
		if startTime.IsZero() {
			startTime = time.Now()
		}
		useTimeSeconds := int(time.Since(startTime).Seconds())
		model.RecordErrorLog(c, userId, channelId, modelName, tokenName, err.MaskSensitiveErrorWithStatusCode(), tokenId, useTimeSeconds, common.GetContextKeyBool(c, constant.ContextKeyIsStream), userGroup, other)
	}

}

func RelayMidjourney(c *gin.Context) {
	relayInfo, err := relaycommon.GenRelayInfo(c, types.RelayFormatMjProxy, nil, nil)

	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"description": fmt.Sprintf("failed to generate relay info: %s", err.Error()),
			"type":        "upstream_error",
			"code":        4,
		})
		return
	}

	var mjErr *taskdto.MidjourneyResponse
	switch relayInfo.RelayMode {
	case relayconstant.RelayModeMidjourneyNotify:
		mjErr = relay.RelayMidjourneyNotify(c)
	case relayconstant.RelayModeMidjourneyTaskFetch, relayconstant.RelayModeMidjourneyTaskFetchByCondition:
		mjErr = relay.RelayMidjourneyTask(c, relayInfo.RelayMode)
	case relayconstant.RelayModeMidjourneyTaskImageSeed:
		mjErr = relay.RelayMidjourneyTaskImageSeed(c)
	case relayconstant.RelayModeSwapFace:
		mjErr = relay.RelaySwapFace(c, relayInfo)
	default:
		mjErr = relay.RelayMidjourneySubmit(c, relayInfo)
	}
	//err = relayMidjourneySubmit(c, relayMode)
	// 标准库 log 出口不经过 logger.logHelper/SysLog，直接对错误文本脱敏，
	// 防止 MJ 上游错误内容携带的凭据落入 stderr/控制台日志。
	log.Println(common.RedactCredentials(fmt.Sprintf("%v", mjErr)))
	if mjErr != nil {
		statusCode := http.StatusBadRequest
		if mjErr.Code == 30 {
			mjErr.Result = "当前分组负载已饱和，请稍后再试，或升级账户以提升服务质量。"
			statusCode = http.StatusTooManyRequests
		}
		c.JSON(statusCode, gin.H{
			"description": fmt.Sprintf("%s %s", mjErr.Description, mjErr.Result),
			"type":        "upstream_error",
			"code":        mjErr.Code,
		})
		channelId := c.GetInt("channel_id")
		logger.LogError(c, fmt.Sprintf("relay error (channel #%d, status code %d): %s", channelId, statusCode, fmt.Sprintf("%s %s", mjErr.Description, mjErr.Result)))
	}
}

func RelayNotImplemented(c *gin.Context) {
	err := types.OpenAIError{
		Message: "API not implemented",
		Type:    "new_api_error",
		Param:   "",
		Code:    "api_not_implemented",
	}
	c.JSON(http.StatusNotImplemented, gin.H{
		"error": err,
	})
}

func RelayNotFound(c *gin.Context) {
	err := types.OpenAIError{
		Message: fmt.Sprintf("Invalid URL (%s %s)", c.Request.Method, c.Request.URL.Path),
		Type:    "invalid_request_error",
		Param:   "",
		Code:    "",
	}
	c.JSON(http.StatusNotFound, gin.H{
		"error": err,
	})
}

func RelayTaskFetch(c *gin.Context) {
	relayInfo, err := relaycommon.GenRelayInfo(c, types.RelayFormatTask, nil, nil)
	if err != nil {
		c.JSON(http.StatusInternalServerError, &taskdto.TaskError{
			Code:       "gen_relay_info_failed",
			Message:    common.RedactCredentials(err.Error()),
			StatusCode: http.StatusInternalServerError,
		})
		return
	}
	if taskErr := relay.RelayTaskFetch(c, relayInfo.RelayMode); taskErr != nil {
		respondTaskError(c, taskErr)
	}
}

func RelayTask(c *gin.Context) {
	relayInfo, err := relaycommon.GenRelayInfo(c, types.RelayFormatTask, nil, nil)
	if err != nil {
		c.JSON(http.StatusInternalServerError, &taskdto.TaskError{
			Code:       "gen_relay_info_failed",
			Message:    common.RedactCredentials(err.Error()),
			StatusCode: http.StatusInternalServerError,
		})
		// 恢复入口：初始化失败（请求从未发出，服务端不可能已创建）也要恢复
		// 父记录为可操作状态，避免 recreated 状态悬空
		if rid := c.GetInt64("task_recovery_id"); rid > 0 {
			backfillRecoveryParentNote(c, rid, "人工重试未进入创建流程：请求初始化失败，记录已重新打开，可再次尝试")
		}
		return
	}

	// ── 恢复入口注入（尽早执行；defer 保证无论走哪条返回路径——早退/成功/失败——
	//    都会把新尝试结果回填到父恢复记录，recreated 状态不会永久悬空）──
	var result *relay.TaskSubmitResult
	var taskErr *taskdto.TaskError
	recoveryParentID := c.GetInt64("task_recovery_id")
	if recoveryParentID > 0 {
		relayInfo.TaskRelayInfo.RecoveryParentID = recoveryParentID
		relayInfo.TaskRelayInfo.ConfirmDuplicateRisk = c.GetBool("task_recovery_confirm")
		defer func() {
			backfillRecoveryParent(c, relayInfo, recoveryParentID, result, taskErr)
		}()
	}

	// ── Seedance 并发名额预留（单个用户同时运行任务数上限）──
	// 进入重试循环、确定渠道为 Seedance 家族后原子预留一个名额（+1）。名额的
	// 转移对象有三种，任一成立则请求结束时不释放（名额继续被占用）：
	//   1. 任务行创建成功（concurrencySlotTransferred / defer 行存在检测）——
	//      由轮询器在任务到达终态时幂等释放；
	//   2. 创建结果未知（outcome_unknown）持久化了恢复记录（recoverySlotTransferred）——
	//      上游可能已创建任务，名额由恢复记录持有，直到超时由对账清理；
	//   3. 已取得上游 task_id 但本地落库失败，恢复记录创建成功（recoverySlotTransferred）——
	//      上游任务确定存在，同样由恢复记录持有名额。
	// 若最终未转移（上游明确失败且无任务行/恢复记录），由 defer 立即释放。
	// 预留与释放均基于共享数据库行锁，多实例部署下并发创建被串行化，不会绕过限制
	// （见 model/task_concurrency.go）。
	concurrencySlotReserved := false    // 本次请求是否已预留名额
	concurrencySlotTransferred := false // 任务行是否已创建（名额转交任务生命周期）
	recoverySlotTransferred := false    // 恢复记录是否已创建（名额转交恢复记录）
	defer func() {
		if !concurrencySlotReserved || concurrencySlotTransferred {
			return
		}
		if recoverySlotTransferred {
			return // 恢复记录持有名额（outcome_unknown / 落库失败），由对账超时清理
		}
		// 任务行存在（含首次 Insert 失败后 RecoverTaskAfterInsertFailure 重试
		// 成功的路径）：名额归任务生命周期，不得在此释放。
		if relayInfo.TaskRelayInfo != nil && relayInfo.TaskRelayInfo.PublicTaskID != "" {
			if _, exist, qErr := model.GetByTaskId(relayInfo.UserId, relayInfo.TaskRelayInfo.PublicTaskID); qErr == nil && exist {
				return
			}
		}
		// 查询失败时保守释放（宁可多释放一次，计数偏低由对账无条件回补，也不允许泄漏）
		if rErr := model.ReleaseTaskConcurrencySlot(relayInfo.UserId); rErr != nil {
			common.SysError("release task concurrency slot error: " + rErr.Error())
		}
	}()

	if e := relay.ResolveOriginTask(c, relayInfo); e != nil {
		taskErr = e
		respondTaskError(c, e)
		return
	}

	// ── 幂等性准备（必须在进入重试循环之前完成）──
	// 1. 生成"本次逻辑任务"的本地幂等键 + X-Client-Request-Id：
	//    同一次任务的自动重试全程复用，绝不重新生成；全新任务（新 HTTP 请求 →
	//    新 RelayInfo）自然得到新值。二者均只用于本地去重/日志串联/审计，
	//    不具备服务端幂等语义（Seedance 未声明支持 Idempotency-Key）。
	idemKey := relayInfo.EnsureTaskIdempotencyKey()
	clientRequestID := relayInfo.EnsureTaskClientRequestID()

	// 2. 客户端提交锁：同用户同参数的并发/重复提交，在请求未完成（含完成后短
	//    宽限期）时直接拒绝（409），避免双击导致多次 POST 创建多个任务。
	//    注意：该锁仅防止"单实例或共享锁范围（Redis/数据库）内"的重复提交，
	//    多实例部署依赖共享后端；共享后端故障时 fail-closed（503），
	//    绝不静默降级为进程内锁（否则跨实例重复提交无法拦截）。
	fingerprint, releaseSubmitLock, acquired, lockErr := service.TryAcquireTaskSubmitLock(c)
	if lockErr != nil {
		c.JSON(http.StatusServiceUnavailable, &taskdto.TaskError{
			Code:       "task_submit_lock_unavailable",
			Message:    "提交去重锁暂不可用，请稍后重试（已停止本次提交以避免重复创建）",
			StatusCode: http.StatusServiceUnavailable,
		})
		return
	}
	if !acquired {
		respondDuplicateSubmission(c)
		return
	}
	defer releaseSubmitLock()

	defer func() {
		if taskErr != nil && relayInfo.Billing != nil {
			relayInfo.Billing.Refund(c)
		}
	}()
	defer func() {
		if relayInfo.TaskRelayInfo == nil || relayInfo.TaskRelayInfo.SeedanceCostMicros <= 0 {
			return
		}
		// 只有 Seedance 明确创建成功或结果未知时保留预留。若安全重试后来切换
		// 到其他供应商并成功，先前 Seedance 发送前失败的预留仍会正确释放。
		if relayInfo.TaskRelayInfo.SeedanceCostCommitted {
			return
		}
		if releaseErr := service.ReleaseSeedanceCost(relayInfo.TaskRelayInfo.SeedanceCostPeriod, relayInfo.TaskRelayInfo.SeedanceCostMicros); releaseErr != nil {
			common.SysError("release Seedance cost reservation error: " + releaseErr.Error())
		}
		relayInfo.TaskRelayInfo.SeedanceCostMicros = 0
	}()

	retryParam := &service.RetryParam{
		Ctx:         c,
		TokenGroup:  relayInfo.TokenGroup,
		ModelName:   relayInfo.OriginModelName,
		RequestPath: c.Request.URL.Path,
		Retry:       common.GetPointer(0),
	}

retryLoop:
	for ; retryParam.GetRetry() <= common.RetryTimes; retryParam.IncreaseRetry() {
		var channel *model.Channel

		if lockedCh, ok := relayInfo.LockedChannel.(*model.Channel); ok && lockedCh != nil {
			channel = lockedCh
			if retryParam.GetRetry() > 0 {
				if setupErr := middleware.SetupContextForSelectedChannel(c, channel, relayInfo.OriginModelName); setupErr != nil {
					taskErr = service.TaskErrorWrapperLocal(setupErr.Err, "setup_locked_channel_failed", http.StatusInternalServerError)
					break
				}
			}
		} else {
			var channelErr *types.NewAPIError
			channel, channelErr = getChannel(c, relayInfo, retryParam)
			if channelErr != nil {
				logger.LogError(c, channelErr.Error())
				taskErr = service.TaskErrorWrapperLocal(channelErr.Err, "get_channel_failed", http.StatusInternalServerError)
				break
			}
		}

		// Seedance 并发名额：确定渠道为 Seedance 家族后，仅在首次尝试时预留名额。
		// 已达上限（current >= limit）时拒绝创建并返回 429 + 明确错误码，
		// 前端应展示提示且不自动重复提交（自动重试只会继续撞 429）。
		if !concurrencySlotReserved && service.IsSeedanceChannelType(channel.Type) {
			limit := service.MaxConcurrentSeedanceTasks()
			reserved, current, cErr := model.ReserveTaskConcurrencySlot(relayInfo.UserId, limit)
			if cErr != nil {
				taskErr = service.TaskErrorWrapperLocal(cErr, "seedance_concurrency_check_failed", http.StatusInternalServerError)
				break
			}
			if !reserved {
				respondTaskConcurrencyLimit(c, current, limit)
				return
			}
			concurrencySlotReserved = true
		}

		addUsedChannel(c, channel.Id)
		bodyStorage, bodyErr := common.GetBodyStorage(c)
		if bodyErr != nil {
			if common.IsRequestBodyTooLargeError(bodyErr) || errors.Is(bodyErr, common.ErrRequestBodyTooLarge) {
				taskErr = service.TaskErrorWrapperLocal(bodyErr, "read_request_body_failed", http.StatusRequestEntityTooLarge)
			} else {
				taskErr = service.TaskErrorWrapperLocal(bodyErr, "read_request_body_failed", http.StatusBadRequest)
			}
			break
		}
		c.Request.Body = io.NopCloser(bodyStorage)

		result, taskErr = relay.RelayTaskSubmit(c, relayInfo)

		// 记录幂等键/请求ID与本次尝试结果，便于排查重复创建（不记录请求体等敏感内容）
		if idemKey != "" {
			attemptStatus := http.StatusOK
			if taskErr != nil {
				attemptStatus = taskErr.StatusCode
			}
			logger.LogDebug(c, "task submit attempt: idempotency_key=%s client_request_id=%s retry=%d status_code=%d", idemKey, clientRequestID, retryParam.GetRetry(), attemptStatus)
		}

		if taskErr == nil {
			break
		}
		// 成本熔断/检查故障发生在发送上游请求之前，且是全站状态；切换渠道
		// 重试没有意义，也不能借重试绕过保护。
		if taskErr.Code == "SEEDANCE_COST_CIRCUIT_OPEN" || taskErr.Code == "seedance_cost_check_failed" {
			break
		}

		// 渠道健康信息上报（原有行为，仅非本地错误）
		if !taskErr.LocalError {
			processChannelError(c,
				*types.NewChannelError(channel.Id, channel.Type, channel.Name, channel.ChannelInfo.IsMultiKey,
					common.GetContextKeyString(c, constant.ContextKeyChannelKey), channel.GetAutoBan()),
				types.NewOpenAIError(taskErr.Error, types.ErrorCodeBadResponseStatusCode, taskErr.StatusCode))
		}

		// ── 结果分类决策（仅 Seedance/豆包渠道启用严格策略；其他渠道维持原行为）──
		if relaycommon.IsStrictIdempotencyChannel(relayInfo.ChannelType) {
			outcome := relaycommon.TaskSubmitOutcomeUnset
			if relayInfo.TaskRelayInfo != nil {
				outcome = relayInfo.TaskRelayInfo.SubmitOutcome
			}
			switch outcome {
			case relaycommon.TaskSubmitOutcomeOutcomeUnknown:
				// 结果未知（读取响应超时/连接中断/200 但无 task_id）：停止自动重试，
				// 持久化恢复记录。服务端可能已创建任务，自动重发会产生重复视频任务。
				// 恢复记录持有并发名额（上游可能已创建任务），不随请求结束释放。
				recoverySlotTransferred = persistOutcomeUnknownRecovery(c, relayInfo, fingerprint, taskErr, concurrencySlotReserved)
				taskErr = outcomeUnknownTaskError(taskErr)
				break retryLoop
			case relaycommon.TaskSubmitOutcomeConfirmedFailure:
				// 明确收到失败响应：未通过查询唯一确认"未创建"，不自动重试。
				break retryLoop
			}
			// 未命中上述分支 = pre_send_failure（请求字节未发出，服务端不可能已创建）
			// → 落入下方通用重试判定，按预算安全重试。
		}

		if !shouldRetryTaskRelay(c, channel.Id, taskErr, common.RetryTimes-retryParam.GetRetry()) {
			break
		}
	}

	useChannel := c.GetStringSlice("use_channel")
	if len(useChannel) > 1 {
		retryLogStr := fmt.Sprintf("重试：%s", strings.Trim(strings.Join(strings.Fields(fmt.Sprint(useChannel)), "->"), "[]"))
		logger.LogInfo(c, retryLogStr)
	}

	// 汇总日志：幂等键、请求ID、重试次数、响应状态码、任务 ID（上游 Seedance 任务 ID 与公开 ID）。
	// 刻意不记录请求体，避免敏感内容入日志。
	{
		statusCode := http.StatusOK
		upstreamTaskID := ""
		publicTaskID := ""
		if taskErr != nil {
			statusCode = taskErr.StatusCode
		}
		if result != nil {
			upstreamTaskID = result.UpstreamTaskID
		}
		if relayInfo.TaskRelayInfo != nil {
			publicTaskID = relayInfo.TaskRelayInfo.PublicTaskID
		}
		logger.LogInfo(c, fmt.Sprintf("task submit finished: idempotency_key=%s client_request_id=%s retries=%d status_code=%d upstream_task_id=%s public_task_id=%s",
			idemKey, clientRequestID, retryParam.GetRetry(), statusCode, upstreamTaskID, publicTaskID))
	}

	// ── 成功：结算 + 日志 + 插入任务 ──
	if taskErr == nil {
		if settleErr := service.SettleBilling(c, relayInfo, result.Quota); settleErr != nil {
			common.SysError("settle task billing error: " + settleErr.Error())
		}
		consumeLogRecorded, preConsumeErr := service.LogTaskConsumption(c, relayInfo)
		billingStatsFailed := false
		if preConsumeErr != nil {
			// 预扣累计消耗失败（用户/渠道行缺失或数据库错误）：上游任务已创建、
			// 钱包已预扣，本地仍必须创建任务行——这是唯一的任务生命周期记录
			// （轮询/结算/退款都依赖它），绝不能"收费但任务丢失"。标记
			// BillingStatsFailed=true：used_quota/request_count 从未累加，退款方向
			// 结算将跳过累计消耗冲减（见 ApplyTaskQuotaDelta），避免
			// used_quota >= refund 守卫永久卡死。
			billingStatsFailed = true
			common.SysError("pre-consume task used quota failed: " + preConsumeErr.Error())
		}

		task := model.InitTask(result.Platform, relayInfo)
		task.PrivateData.UpstreamTaskID = result.UpstreamTaskID
		task.PrivateData.BillingSource = relayInfo.BillingSource
		task.PrivateData.SubscriptionId = relayInfo.SubscriptionId
		task.PrivateData.TokenId = relayInfo.TokenId
		task.PrivateData.NodeName = common.NodeName
		task.PrivateData.ConsumeLogRecorded = consumeLogRecorded
		task.PrivateData.BillingStatsFailed = billingStatsFailed
		task.PrivateData.BillingContext = &model.TaskBillingContext{
			ModelPrice:      relayInfo.PriceData.ModelPrice,
			GroupRatio:      relayInfo.PriceData.GroupRatioInfo.GroupRatio,
			ModelRatio:      relayInfo.PriceData.ModelRatio,
			OtherRatios:     relayInfo.PriceData.OtherRatios(),
			OriginModelName: relayInfo.OriginModelName,
			PerCallBilling:  common.StringsContains(constant.TaskPricePatches, relayInfo.OriginModelName) || relayInfo.PriceData.UsePrice,
			// 仅预扣缓冲只用于预留审计，绝不进入轮询结算（结算只读 OtherRatios）
			PreConsumeMultiplier: relayInfo.PriceData.PreConsumeMultiplier,
		}
		task.Quota = result.Quota
		task.Data = result.TaskData
		task.Action = relayInfo.Action
		if insertErr := task.Insert(); insertErr != nil {
			common.SysError("insert task error: " + insertErr.Error())
			// 已取得上游 task_id 但本地落库失败：用该 task_id 调用查询接口恢复状态，
			// 绝不再次 POST（否则会重复创建视频任务）。重试成功则任务行已存在
			// （名额归任务）；仍失败则恢复记录持有名额（上游任务确定存在）。
			recoverySlotTransferred = relay.RecoverTaskAfterInsertFailure(c, relayInfo, task, result.UpstreamTaskID, insertErr, concurrencySlotReserved)
		} else {
			// 任务行创建成功：并发名额转交任务生命周期（轮询器在终态时释放）
			concurrencySlotTransferred = true
		}
	}

	if taskErr != nil {
		respondTaskError(c, taskErr)
	}
}

// respondTaskError 统一输出 Task 错误响应（含 429 限流提示改写）
func respondTaskError(c *gin.Context, taskErr *taskdto.TaskError) {
	if taskErr.StatusCode == http.StatusTooManyRequests {
		taskErr.Message = "当前分组上游负载已饱和，请稍后再试"
	}
	// 兜底脱敏凭据类信息（方舟 Endpoint ID / API Key / Bearer Token），
	// 防止任何上游错误文本中的敏感信息透传给客户端。
	taskErr.Message = common.RedactCredentials(taskErr.Message)
	c.JSON(taskErr.StatusCode, taskErr)
}

// respondTaskConcurrencyLimit 拒绝"达到 Seedance 并发上限"的创建请求（429）。
// 使用独立响应器而非 respondTaskError：后者会把 429 文案改写为上游负载提示，
// 而并发上限必须保留明确错误码与引导文案；前端据此展示提示，不自动重复提交。
func respondTaskConcurrencyLimit(c *gin.Context, current, limit int) {
	c.JSON(http.StatusTooManyRequests, &taskdto.TaskError{
		Code:       "SEEDANCE_CONCURRENCY_LIMIT_EXCEEDED",
		Message:    fmt.Sprintf("你当前已有 %d 个 Seedance 任务正在运行，最多可同时运行 %d 个。请等待任务完成或取消任务后再试。", current, limit),
		StatusCode: http.StatusTooManyRequests,
	})
}

// respondDuplicateSubmission 拒绝重复提交：相同请求仍在处理中（双击/并发重复）。
// 客户端应忽略该响应（任务正在创建中），而非重试创建新任务。
func respondDuplicateSubmission(c *gin.Context) {
	c.JSON(http.StatusConflict, &taskdto.TaskError{
		Code:       "duplicate_submission",
		Message:    "检测到相同的提交正在处理中，请勿重复提交",
		StatusCode: http.StatusConflict,
	})
}

// backfillRecoveryParent 把人工重试（recreate）的新尝试结果回填到父恢复记录，
// 保证 recreated 状态在任何返回路径（早退/成功/失败）下都不会永久悬空。
// 由 RelayTask 在注入恢复上下文时以 defer 注册；只有真正发起本次尝试的请求
// 才会写入（recreate 的原子占位已保证并发只有一个请求能进入创建流程）。
//
// 恢复路径（可执行性不变量）：
//   - 成功创建任务 → 父保持 recreated（终态），回填任务 ID；
//   - 结果未知（outcome_unknown）且子恢复记录已创建 → 父保持 recreated，
//     回填子记录 ID（恢复路径在子记录上）；
//   - 结果未知但子记录落库失败 → 父保持 recreated（绝不重新打开为 unknown，
//     否则会误导"可安全重试"，而服务端可能已创建任务），备注提示人工查询确认；
//   - 明确失败且无子记录（渠道选择失败、confirmed_failure、pre-send 重试耗尽、
//     锁 503/409 等）→ 把父记录重新打开为 unknown，用户可再次执行
//     关联/候选发现/人工重试，绝不把用户困在无出路的终态。
func backfillRecoveryParent(c *gin.Context, info *relaycommon.RelayInfo, parentID int64, result *relay.TaskSubmitResult, taskErr *taskdto.TaskError) {
	if parentID <= 0 {
		return
	}
	userId := int64(info.UserId)
	switch {
	case taskErr == nil && result != nil:
		publicTaskID := ""
		if info.TaskRelayInfo != nil {
			publicTaskID = info.TaskRelayInfo.PublicTaskID
		}
		if e := model.UpdateRecoveryNote(parentID, userId, fmt.Sprintf("人工重试已完成：公开任务ID=%s 上游任务ID=%s", publicTaskID, result.UpstreamTaskID)); e != nil {
			common.SysError("update recovery parent note error: " + e.Error())
		}
	case taskErr != nil:
		// 结果未知时 persistOutcomeUnknown 应已创建子恢复记录（ParentID 关联）
		if child, e := model.GetTaskSubmitRecoveryByParent(parentID); e == nil && child != nil {
			if e := model.UpdateRecoveryNote(parentID, userId, fmt.Sprintf("人工重试结果未知，已生成新恢复记录(ID=%d)，请在该记录上继续恢复", child.ID)); e != nil {
				common.SysError("update recovery parent note error: " + e.Error())
			}
			return
		}
		// 结果未知但子记录未创建（落库失败）：服务端可能已创建任务，
		// 绝不能重新打开为 unknown（会误导可安全重试），保持 recreated 并提示人工确认。
		outcome := relaycommon.TaskSubmitOutcomeUnset
		if info.TaskRelayInfo != nil {
			outcome = info.TaskRelayInfo.SubmitOutcome
		}
		if outcome == relaycommon.TaskSubmitOutcomeOutcomeUnknown {
			if e := model.UpdateRecoveryNote(parentID, userId,
				"人工重试结果未知，且新恢复记录落库失败，请通过官方查询接口人工确认任务是否已创建后处理"); e != nil {
				common.SysError("update recovery parent note error: " + e.Error())
			}
			return
		}
		// 明确失败且无子记录 → 重新打开为 unknown（可再次关联/发现/重试）
		status := 0
		if taskErr.StatusCode > 0 {
			status = taskErr.StatusCode
		}
		note := fmt.Sprintf("人工重试未成功(code=%s status=%d)，记录已重新打开，可再次执行关联/候选发现/重试", taskErr.Code, status)
		if e := model.ResetRecoveryForRetry(parentID, userId, note); e != nil {
			common.SysError("reset recovery for retry error: " + e.Error())
		}
	default:
		// 未进入创建流程（锁 503/409 等早退）→ 重新打开为 unknown
		if e := model.ResetRecoveryForRetry(parentID, userId, "人工重试未进入创建流程，记录已重新打开，可再次尝试"); e != nil {
			common.SysError("reset recovery for retry error: " + e.Error())
		}
	}
}

// backfillRecoveryParentNote 极端早退路径（RelayInfo 初始化失败等）的处理：
// 该早退发生在任何上游请求发出之前（请求从未进入创建流程，服务端不可能已创建
// 任务），因此父记录必须从 recreated 重新打开为 unknown（可操作状态），
// 否则会停留在"人工重试占位但永远无结果"的终态。注意与 backfillRecoveryParent
// 不同——后者处理的是结果未知的尝试，绝不能重新打开。
func backfillRecoveryParentNote(c *gin.Context, parentID int64, note string) {
	if parentID <= 0 {
		return
	}
	if e := model.ResetRecoveryForRetry(parentID, int64(c.GetInt("id")), note); e != nil {
		common.SysError("reset recovery for retry error: " + e.Error())
	}
}

// persistOutcomeUnknownRecovery 持久化"结果未知"的创建请求（含请求指纹、本地
// 幂等键、X-Client-Request-Id、首次提交时间、渠道信息等），此后禁止自动重发。
// persistOutcomeUnknownRecovery 持久化"结果未知"恢复记录。
// 返回该记录是否持有 Seedance 并发名额（reservedSlot && 创建成功），
// 供控制器 defer 决定是否释放名额（上游可能已创建任务，名额不能随请求释放）。
func persistOutcomeUnknownRecovery(c *gin.Context, info *relaycommon.RelayInfo, fingerprint string, taskErr *taskdto.TaskError, reservedSlot bool) bool {
	contentFingerprint := ""
	if req, err := relaycommon.GetTaskRequest(c); err == nil {
		// 存在模型映射时，上游任务项带的是上游模型名；指纹必须用上游模型计算，
		// 否则候选发现时内容指纹永远匹配不上。
		if info.UpstreamModelName != "" {
			req.Model = info.UpstreamModelName
		}
		contentFingerprint = relaycommon.SubmitContentFingerprint(req)
	}
	rec, err := relay.PersistOutcomeUnknown(c, info, fingerprint, contentFingerprint, taskErr.Error, reservedSlot)
	if err != nil {
		logger.LogError(c, "persist outcome_unknown recovery failed: "+err.Error())
		return false
	}
	return rec != nil && rec.ConcurrencyReserved
}

// outcomeUnknownTaskError 面向客户端的"结果未知"错误：明确告知已记录恢复信息、
// 切勿直接重发（服务端可能已创建任务）。
func outcomeUnknownTaskError(original *taskdto.TaskError) *taskdto.TaskError {
	var cause error
	if original != nil {
		cause = original.Error
	}
	return &taskdto.TaskError{
		Code:       "task_submit_outcome_unknown",
		Message:    "创建请求结果未知（超时或连接中断），服务端可能已创建任务，本网关已停止自动重试并记录恢复信息。请通过任务恢复入口查询确认，切勿直接重发。",
		StatusCode: http.StatusBadGateway,
		Error:      cause,
	}
}

func shouldRetryTaskRelay(c *gin.Context, channelId int, taskErr *taskdto.TaskError, retryTimes int) bool {
	if taskErr == nil {
		return false
	}
	if service.ShouldSkipRetryAfterChannelAffinityFailure(c) {
		return false
	}
	if retryTimes <= 0 {
		return false
	}
	if _, ok := c.Get("specific_channel_id"); ok {
		return false
	}
	if taskErr.StatusCode == http.StatusTooManyRequests {
		return true
	}
	if taskErr.StatusCode == 307 {
		return true
	}
	if taskErr.StatusCode/100 == 5 {
		// 超时不重试
		if operation_setting.IsAlwaysSkipRetryStatusCode(taskErr.StatusCode) {
			return false
		}
		return true
	}
	if taskErr.StatusCode == http.StatusBadRequest {
		return false
	}
	if taskErr.StatusCode == 408 {
		// azure处理超时不重试
		return false
	}
	if taskErr.LocalError {
		return false
	}
	if taskErr.StatusCode/100 == 2 {
		return false
	}
	return true
}
