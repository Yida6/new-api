package service

import (
	"context"
	"fmt"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/logger"
	"github.com/QuantumNous/new-api/model"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/setting/ratio_setting"
	"github.com/QuantumNous/new-api/types"
	"github.com/gin-gonic/gin"
)

// LogTaskConsumption 记录任务消费日志和统计信息（仅记录，不涉及实际扣费）。
// 实际扣费已由 BillingSession（PreConsumeBilling + SettleBilling）完成。
// 返回 (消费日志是否实际写入成功, 预扣累计消耗是否成功)。调用方据此持久化
// task.ConsumeLogRecorded，结算/退款时按该标记决定是否写计费调整日志。
// 预扣累计消耗在单个事务内完成（用户 used_quota + request_count + 渠道 used_quota），
// 失败返回 error 供调用方终止任务提交。
func LogTaskConsumption(c *gin.Context, info *relaycommon.RelayInfo) (bool, error) {
	tokenName := c.GetString("token_name")
	logContent := fmt.Sprintf("操作 %s", info.Action)
	// 支持任务仅按次计费
	if common.StringsContains(constant.TaskPricePatches, info.OriginModelName) {
		logContent = fmt.Sprintf("%s，按次计费", logContent)
	} else {
		if otherRatios := info.PriceData.OtherRatios(); len(otherRatios) > 0 {
			var contents []string
			for key, ra := range otherRatios {
				if 1.0 != ra {
					contents = append(contents, fmt.Sprintf("%s: %.2f", key, ra))
				}
			}
			if len(contents) > 0 {
				logContent = fmt.Sprintf("%s, 计算参数：%s", logContent, strings.Join(contents, ", "))
			}
		}
	}
	other := make(map[string]interface{})
	other["is_task"] = true
	other["request_path"] = c.Request.URL.Path
	other["model_price"] = info.PriceData.ModelPrice
	if info.PriceData.ModelRatio > 0 {
		other["model_ratio"] = info.PriceData.ModelRatio
	}
	other["group_ratio"] = info.PriceData.GroupRatioInfo.GroupRatio
	if info.PriceData.GroupRatioInfo.HasSpecialRatio {
		other["user_group_ratio"] = info.PriceData.GroupRatioInfo.GroupSpecialRatio
	}
	if info.IsModelMapped {
		other["is_model_mapped"] = true
		other["upstream_model_name"] = info.UpstreamModelName
	}
	attachQuotaSaturation(c, info, other)
	consumeLogRecorded := model.RecordConsumeLog(c, info.UserId, model.RecordConsumeLogParams{
		ChannelId: info.ChannelId,
		ModelName: info.OriginModelName,
		TokenName: tokenName,
		Quota:     info.PriceData.Quota,
		Content:   logContent,
		TokenId:   info.TokenId,
		Group:     info.UsingGroup,
		Other:     other,
	})
	// 原子预扣累计消耗（用户 used_quota + request_count + 渠道 used_quota）。
	if err := model.ApplyPreConsumeUsedQuota(info.UserId, info.ChannelId, info.PriceData.Quota); err != nil {
		return consumeLogRecorded, err
	}
	return consumeLogRecorded, nil
}

// ---------------------------------------------------------------------------
// 异步任务计费辅助函数
// ---------------------------------------------------------------------------

// resolveTokenKey 通过 TokenId 运行时获取令牌 Key（用于 Redis 缓存操作）。
// 如果令牌已被删除或查询失败，返回空字符串。
func resolveTokenKey(ctx context.Context, tokenId int, taskID string) string {
	token, err := model.GetTokenById(tokenId)
	if err != nil {
		logger.LogWarn(ctx, fmt.Sprintf("获取令牌 key 失败 (tokenId=%d, task=%s): %s", tokenId, taskID, err.Error()))
		return ""
	}
	return token.Key
}

// taskIsSubscription 判断任务是否通过订阅计费。
func taskIsSubscription(task *model.Task) bool {
	return task.PrivateData.BillingSource == BillingSourceSubscription && task.PrivateData.SubscriptionId > 0
}

// taskAdjustTokenQuota 调整任务的令牌额度，delta > 0 表示扣费，delta < 0 表示退还。
// 需要通过 resolveTokenKey 运行时获取 key（不从 PrivateData 中读取）。
func taskAdjustTokenQuota(ctx context.Context, task *model.Task, delta int64) {
	if task.PrivateData.TokenId <= 0 || delta == 0 {
		return
	}
	tokenKey := resolveTokenKey(ctx, task.PrivateData.TokenId, task.TaskID)
	if tokenKey == "" {
		return
	}
	var err error
	if delta > 0 {
		err = model.DecreaseTokenQuota(task.PrivateData.TokenId, tokenKey, delta)
	} else {
		err = model.IncreaseTokenQuota(task.PrivateData.TokenId, tokenKey, -delta)
	}
	if err != nil {
		logger.LogWarn(ctx, fmt.Sprintf("调整令牌额度失败 (delta=%d, task=%s): %s", delta, task.TaskID, err.Error()))
	}
}

// taskBillingOther 从 task 的 BillingContext 构建日志 Other 字段。
func taskBillingOther(task *model.Task) map[string]interface{} {
	other := make(map[string]interface{})
	if bc := task.PrivateData.BillingContext; bc != nil {
		other["model_price"] = bc.ModelPrice
		if bc.ModelRatio > 0 {
			other["model_ratio"] = bc.ModelRatio
		}
		other["group_ratio"] = bc.GroupRatio
		if priceData := taskBillingContextPriceData(bc); priceData != nil {
			for k, v := range priceData.OtherRatios() {
				other[k] = v
			}
		}
	}
	props := task.Properties
	if props.UpstreamModelName != "" && props.UpstreamModelName != props.OriginModelName {
		other["is_model_mapped"] = true
		other["upstream_model_name"] = props.UpstreamModelName
	}
	return other
}

func taskBillingContextPriceData(bc *model.TaskBillingContext) *types.PriceData {
	if bc == nil || len(bc.OtherRatios) == 0 {
		return nil
	}
	priceData := &types.PriceData{}
	if !priceData.ReplaceOtherRatios(bc.OtherRatios) {
		return nil
	}
	return priceData
}

// taskModelName 从 BillingContext 或 Properties 中获取模型名称。
func taskModelName(task *model.Task) string {
	if bc := task.PrivateData.BillingContext; bc != nil && bc.OriginModelName != "" {
		return bc.OriginModelName
	}
	return task.Properties.OriginModelName
}

// RefundTaskQuota 统一的任务失败退款逻辑。
// 当异步任务失败时，将预扣的 quota 退还给用户（支持钱包和订阅），并退还令牌额度。
// 返回资金来源是否已成功退还；失败时保留 quota，供显式重试或人工对账。
func RefundTaskQuota(ctx context.Context, task *model.Task, reason string) bool {
	quota := task.Quota
	if quota == 0 {
		return true
	}

	// 欠款防御：任务已存在未清欠款（Seedance 差额未收）时禁止退款——
	// 已预扣的钱属于"已收部分"，差额由欠款闭环单独收款，绝不能在此把预扣
	// 退回后留下"既欠款又已退款"的资金分叉。欠款任务已在终态，正常轮询不会
	// 触发退款；此检查覆盖超时 sweep / 恢复等异常路径的兜底。
	if hasDebt, err := model.HasOpenDebtForTask(task.TaskID); err == nil && hasDebt {
		logger.LogError(ctx, fmt.Sprintf("拒绝退款 task %s: 存在未清欠款，走欠款收款闭环（refund=%d）", task.TaskID, quota))
		return false
	}

	// 原子完成：资金退款（钱包/订阅）+ 用户/渠道累计消耗冲减 + task.Quota 清零。
	// 退款守卫失败或任一步失败整体回滚，任务保持未退款状态可重试，杜绝部分写入的永久分叉。
	if !model.ApplyTaskQuotaDelta(task, -quota, taskIsSubscription(task)) {
		logger.LogError(ctx, fmt.Sprintf("退款失败 task %s: refund=%d，请运行历史修复脚本对账后重试", task.TaskID, quota))
		return false
	}

	// 令牌额度退款（尽力而为）
	taskAdjustTokenQuota(ctx, task, -quota)

	// 退款日志：仅当提交时记录了消费日志才写，保证"消费-退款"日志口径对称。
	if task.PrivateData.ConsumeLogRecorded {
		other := taskBillingOther(task)
		other["task_id"] = task.TaskID
		other["reason"] = reason
		model.RecordTaskBillingLog(model.RecordTaskBillingLogParams{
			UserId:    task.UserId,
			LogType:   model.LogTypeRefund,
			Content:   "",
			ChannelId: task.ChannelId,
			ModelName: taskModelName(task),
			Quota:     quota,
			TokenId:   task.PrivateData.TokenId,
			Group:     task.Group,
			Other:     other,
		})
	}
	return true
}

// RecalculateTaskQuota 通用的异步差额结算。
// actualQuota 是任务完成后的实际应扣额度，与预扣额度 (task.Quota) 做差额结算。
// reason 用于日志记录（例如 "token重算" 或 "adaptor调整"）。
// totalTokens 任务结算时的上游实际 token 总数（>0 时写入调整日志 other.total_tokens；
// adaptor 调整等无 token 语义的路径传 0）。
// clamps 可选：若计算 actualQuota 时发生额度饱和，将其记入日志 admin_info（仅管理员可见）。
func RecalculateTaskQuota(ctx context.Context, task *model.Task, actualQuota int64, reason string, totalTokens int, clamps ...*common.QuotaClamp) bool {
	if actualQuota <= 0 {
		return true
	}
	preConsumedQuota := task.Quota
	quotaDelta := actualQuota - preConsumedQuota

	if quotaDelta == 0 {
		logger.LogInfo(ctx, fmt.Sprintf("任务 %s 预扣费准确（%s，%s）",
			task.TaskID, logger.LogQuota(actualQuota), reason))
		return true
	}

	logger.LogInfo(ctx, fmt.Sprintf("任务 %s 差额结算：delta=%s（实际：%s，预扣：%s，%s）",
		task.TaskID,
		logger.LogQuota(quotaDelta),
		logger.LogQuota(actualQuota),
		logger.LogQuota(preConsumedQuota),
		reason,
	))

	// 原子完成：资金调整 + 用户/渠道累计消耗调整 + task.Quota 更新。
	// 退款方向守卫失败或任一步失败整体回滚，任务保持未结算状态可重试。
	if !model.ApplyTaskQuotaDelta(task, quotaDelta, taskIsSubscription(task)) {
		logger.LogError(ctx, fmt.Sprintf("差额结算失败 task %s: delta=%d，请运行历史修复脚本对账后重试", task.TaskID, quotaDelta))
		return false
	}

	// 调整令牌额度（尽力而为）
	taskAdjustTokenQuota(ctx, task, quotaDelta)

	// 计费调整日志（消费/退款口径对称）
	recordTaskQuotaAdjustLog(task, quotaDelta, preConsumedQuota, actualQuota, reason, totalTokens, clamps...)
	return true
}

// recordTaskQuotaAdjustLog 写一条任务差额调整日志（补扣 LogTypeConsume /
// 退款 LogTypeRefund），仅当提交时记录了消费日志才写，保证"消费-退款/补扣"
// 日志口径对称。Seedance 统一结算与通用差额结算共用此函数，避免口径分叉。
// totalTokens 为任务结算时的上游实际 token 总数（>0 时写入 other.total_tokens，
// 供前端 Tokens 列回显；未知/不适用传 0）。
func recordTaskQuotaAdjustLog(task *model.Task, delta, preConsumedQuota, actualQuota int64, reason string, totalTokens int, clamps ...*common.QuotaClamp) {
	if !task.PrivateData.ConsumeLogRecorded {
		return
	}
	var logType int
	var logQuota int64
	if delta > 0 {
		logType = model.LogTypeConsume
		logQuota = delta
	} else {
		logType = model.LogTypeRefund
		logQuota = -delta
	}
	other := taskBillingOther(task)
	other["task_id"] = task.TaskID
	other["pre_consumed_quota"] = preConsumedQuota
	other["actual_quota"] = actualQuota
	for _, clamp := range clamps {
		attachQuotaSaturationToOther(other, clamp)
	}
	model.RecordTaskBillingLog(model.RecordTaskBillingLogParams{
		UserId:      task.UserId,
		LogType:     logType,
		Content:     reason,
		ChannelId:   task.ChannelId,
		ModelName:   taskModelName(task),
		Quota:       logQuota,
		TokenId:     task.PrivateData.TokenId,
		Group:       task.Group,
		Other:       other,
		NodeName:    task.PrivateData.NodeName,
		TotalTokens: totalTokens,
	})
}

// RecalculateTaskQuotaByTokens 根据实际 token 消耗重新计费（异步差额结算）。
// 当任务成功且返回了 totalTokens 时，根据模型倍率和分组倍率重新计算实际扣费额度，
// 与预扣费的差额进行补扣或退还。支持钱包和订阅计费来源。
func RecalculateTaskQuotaByTokens(ctx context.Context, task *model.Task, totalTokens int) bool {
	if totalTokens <= 0 {
		return true
	}
	actualQuota, ok, _ := computeTaskQuotaByTokens(task, totalTokens)
	if !ok {
		return true
	}
	reason := fmt.Sprintf("token重算：tokens=%d", totalTokens)
	return RecalculateTaskQuota(ctx, task, actualQuota, reason, totalTokens)
}

// computeTaskQuotaByTokens 计算任务按 token 重算后的实际额度：
// actualQuota = totalTokens × modelRatio × groupRatio × otherMultiplier
// （饱和转换，防止溢出成负数）。ok=false 表示该任务不适用 token 重算
// （无倍率配置/固定价格/无法获取用户分组），调用方应保持预扣额度不变。
func computeTaskQuotaByTokens(task *model.Task, totalTokens int) (actualQuota int64, ok bool, clamp *common.QuotaClamp) {
	if task == nil || totalTokens <= 0 {
		return 0, false, nil
	}
	modelName := taskModelName(task)

	// 获取模型价格和倍率
	modelRatio, hasRatioSetting, _ := ratio_setting.GetModelRatio(modelName)
	// 只有配置了倍率(非固定价格)时才按 token 重新计费
	if !hasRatioSetting || modelRatio <= 0 {
		return 0, false, nil
	}

	// 获取用户和组的倍率信息
	group := task.Group
	if group == "" {
		user, err := model.GetUserById(task.UserId, false)
		if err == nil {
			group = user.Group
		}
	}
	if group == "" {
		return 0, false, nil
	}

	groupRatio := ratio_setting.GetGroupRatio(group)
	userGroupRatio, hasUserGroupRatio := ratio_setting.GetGroupGroupRatio(group, group)

	var finalGroupRatio float64
	if hasUserGroupRatio {
		finalGroupRatio = userGroupRatio
	} else {
		finalGroupRatio = groupRatio
	}

	// 计算 OtherRatios 乘积（视频折扣、时长等）
	otherMultiplier := 1.0
	if priceData := taskBillingContextPriceData(task.PrivateData.BillingContext); priceData != nil {
		otherMultiplier = priceData.OtherRatioMultiplier()
	}

	// 计算实际应扣费额度（饱和转换，防止溢出成负数）
	actualQuota, clamp = common.QuotaFromFloatChecked(float64(totalTokens) * modelRatio * finalGroupRatio * otherMultiplier)
	return actualQuota, true, clamp
}

// ---------------------------------------------------------------------------
// Seedance 专用结算闭环（保守预扣的配套资金边界）
// ---------------------------------------------------------------------------

// SeedanceSettleOutcome 是 Seedance 任务完成结算的可识别结果：
//   - Success      结算完成（含差额退款/精确命中）；
//   - DebtCreated  实际费用高于预扣且差额无法扣除 → 欠款已记录 + 用户已冻结
//                  （或管理员已最高级别告警）；任务可进入终态，占用名额释放；
//   - Retryable    数据库错误 → 调用方应把任务回退到非终态，下轮轮询重试。
type SeedanceSettleOutcome int

const (
	SeedanceSettleSuccess SeedanceSettleOutcome = iota
	SeedanceSettleDebtCreated
	SeedanceSettleRetryable
)

// SettleSeedanceTaskBilling 任务完成时的 Seedance 专用计费调整。
// 与通用 settleTaskBillingOnComplete 的差异：
//  1. 实际额度优先由 adaptor.AdjustBillingOnComplete 决定，其次按 token 重算；
//  2. 差额补扣（delta>0）走统一结算（model.ApplySeedanceSettle）：资金守卫
//     （quota >= delta 才扣）+ 用户/渠道累计 + 任务额度 + **Token 额度同一事务**，
//     Token 扣减失败持久化 pending（model.CompensatePendingTokenDeltas 补偿），
//     绝不做"best effort 后宣告完整成功"；
//  3. 差额无法扣除时进入欠款/冻结/告警闭环（欠款幂等、冻结跳过管理员），
//     任务仍可进入终态（上游生命周期与计费生命周期分离），并发名额正常释放；
//  4. 退款/精确方向（delta<=0）同样走统一结算（含 Token 退还与日志），
//     与补扣路径日志口径一致（recordTaskQuotaAdjustLog）。
func SettleSeedanceTaskBilling(ctx context.Context, adaptor TaskPollingAdaptor, task *model.Task, taskResult *relaycommon.TaskInfo) SeedanceSettleOutcome {
	// 0. 按次计费的任务不做差额结算
	if bc := task.PrivateData.BillingContext; bc != nil && bc.PerCallBilling {
		return SeedanceSettleSuccess
	}

	// 1. 计算实际额度（与通用路径同优先级）
	reason := "seedance adaptor计费调整"
	actualQuota := int64(0)
	var clamp *common.QuotaClamp
	// settleTotalTokens 结算时上游返回的实际 token 总数，写入调整日志
	// other.total_tokens 供前端 Tokens 列回显（adaptor 分支同样透传，
	// 上游 usage 有值即记录）。
	settleTotalTokens := 0
	if taskResult != nil {
		settleTotalTokens = taskResult.TotalTokens
	}
	if adj := adaptor.AdjustBillingOnComplete(task, taskResult); adj > 0 {
		actualQuota = adj
	} else if taskResult != nil && taskResult.TotalTokens > 0 {
		quota, ok, c := computeTaskQuotaByTokens(task, taskResult.TotalTokens)
		if !ok {
			// 不适用 token 重算（固定价格/无倍率）→ 保持预扣额度
			return SeedanceSettleSuccess
		}
		actualQuota = quota
		clamp = c
		reason = fmt.Sprintf("token重算：tokens=%d", taskResult.TotalTokens)
	} else {
		return SeedanceSettleSuccess
	}

	preConsumed := task.Quota
	delta := actualQuota - preConsumed
	if delta == 0 {
		logger.LogInfo(ctx, fmt.Sprintf("任务 %s Seedance 预扣费准确（%s，%s）", task.TaskID, logger.LogQuota(actualQuota), reason))
		return SeedanceSettleSuccess
	}

	// 2. 统一结算（资金 + 累计 + 任务额度 + Token 同事务）
	logger.LogInfo(ctx, fmt.Sprintf("任务 %s Seedance 差额结算：delta=%s（实际：%s，预扣：%s，%s）",
		task.TaskID, logger.LogQuota(delta), logger.LogQuota(actualQuota), logger.LogQuota(preConsumed), reason))
	result, tokenResult := model.ApplySeedanceSettle(task, delta, taskIsSubscription(task), model.TaskQuotaDeltaOptions{
		GuardPositiveDelta: delta > 0, // 补扣才需要余额守卫（退款方向无守卫语义）
	})
	switch result {
	case model.TaskQuotaDeltaSuccess:
		// 3. Token 扣减失败：pending 已在 ApplySeedanceSettle 的**同一事务**内
		// 与资金一起落库（task.TokenDeltaPending 内存也已同步），服务层绝不再
		// 依赖提交后的第二次数据库写入来保证恢复信息——"资金已提交但 pending
		// 未落库"的崩溃窗口已消除。此处只保留审计日志。
		if tokenResult == model.TokenAdjustFailed {
			logger.LogWarn(ctx, fmt.Sprintf("Seedance 结算资金已收但 Token 扣减失败，pending 已随资金事务原子落库：task=%s token_delta_pending=%d（后台幂等补偿）",
				task.TaskID, task.TokenDeltaPending))
		}
		// 4. 差额消费日志（含 quota saturation 信息，与退款路径口径一致）
		if clamp != nil {
			recordTaskQuotaAdjustLog(task, delta, preConsumed, actualQuota, reason, settleTotalTokens, clamp)
		} else {
			recordTaskQuotaAdjustLog(task, delta, preConsumed, actualQuota, reason, settleTotalTokens)
		}
		return SeedanceSettleSuccess
	case model.TaskQuotaDeltaInsufficientBalance, model.TaskQuotaDeltaSubscriptionExceeded, model.TaskQuotaDeltaUserNotFound:
		// 差额无法收取 → 欠款 + 冻结 + 告警闭环（已预扣金额保留，绝不错误退款）
		return recordSeedanceDebtAndFreeze(ctx, task, actualQuota, delta, result)
	default:
		// 数据库错误：回退非终态，下轮重试（欠款不落库，避免伪造已处理）
		return SeedanceSettleRetryable
	}
}

// recordSeedanceDebtAndFreeze 创建欠款记录并冻结用户（同一事务），事务提交后
// 再通知 Root 管理员。同一任务重复结算只会命中已有欠款（幂等 no-op），绝不
// 重复补扣、重复冻结或重复累计欠款。
func recordSeedanceDebtAndFreeze(ctx context.Context, task *model.Task, actualQuota, delta int64, cause model.TaskQuotaDeltaResult) SeedanceSettleOutcome {
	input := model.DebtInput{
		UserId:             task.UserId,
		TaskId:             task.TaskID,
		UpstreamTaskId:     task.GetUpstreamTaskID(),
		ModelName:          taskModelName(task),
		ChannelId:          task.ChannelId,
		PreConsumedQuota:   task.Quota,
		ActualQuota:        actualQuota,
		DeltaQuota:         delta,
		Reason:             fmt.Sprintf("seedance差额补扣失败(%s)", cause),
		BillingSource:      task.PrivateData.BillingSource,
		SubscriptionId:     task.PrivateData.SubscriptionId,
		TokenId:            task.PrivateData.TokenId,
		Group:              task.Group,
		ConsumeLogRecorded: task.PrivateData.ConsumeLogRecorded,
		BillingStatsFailed: task.PrivateData.BillingStatsFailed,
	}
	created, frozen, isAdmin, err := model.CreateDebtAndFreeze(input)
	if err != nil {
		logger.LogError(ctx, fmt.Sprintf("记录 Seedance 欠款失败 task %s: %v（保留非终态重试）", task.TaskID, err))
		return SeedanceSettleRetryable
	}
	if !created {
		// 幂等命中：同一任务欠款已存在（可能上次轮询已处理）→ 不重复冻结/累计
		logger.LogInfo(ctx, fmt.Sprintf("任务 %s 欠款已存在，跳过重复创建（幂等）", task.TaskID))
		return SeedanceSettleDebtCreated
	}
	switch {
	case frozen:
		logger.LogWarn(ctx, fmt.Sprintf("Seedance 欠款触发用户冻结：user=%d task=%s delta=%d（差额未收，已阻止继续创建付费任务）",
			task.UserId, task.TaskID, delta))
	case isAdmin:
		// 管理员不冻结，但必须最高级别告警（调用方通知逻辑处理）
		logger.LogError(ctx, fmt.Sprintf("Seedance 欠款发生于管理员账号（不冻结但最高级别告警）：user=%d task=%s delta=%d",
			task.UserId, task.TaskID, delta))
	default:
		logger.LogWarn(ctx, fmt.Sprintf("Seedance 欠款已记录（未冻结：用户不存在或已冻结）：user=%d task=%s delta=%d",
			task.UserId, task.TaskID, delta))
	}
	// 欠款记录成功提交后再通知 Root（发送失败保留 AlertSent=false，可重试）
	notifySeedanceDebt(task, input, isAdmin)
	return SeedanceSettleDebtCreated
}

// notifySeedanceDebt 向 Root 管理员发送欠款告警（含用户/任务/预扣/实际/差额）。
// 流程：原子 claim（多实例去重）→ 校验通知渠道已配置 → 发送成功才标记
// AlertSent；失败/未配置 → 释放 claim 保留重试（RetryPendingDebtAlerts），
// 绝不丢失事件，也绝不在未真正投递时误标已发送。
func notifySeedanceDebt(task *model.Task, input model.DebtInput, isAdmin bool) {
	debt, err := model.GetTaskBillingDebtByTaskId(input.TaskId)
	if err != nil || debt == nil || debt.AlertSent {
		return
	}
	// 原子 claim：其他实例已 claim（未超租约）→ 本实例跳过
	claimed, err := model.ClaimDebtAlert(debt.ID, seedanceDebtAlertLeaseSeconds)
	if err != nil || !claimed {
		return
	}
	if !sendSeedanceDebtAlert(input, isAdmin, false) {
		// 未配置或发送失败：释放 claim，保留 AlertSent=false 供下轮重试
		if releaseErr := model.ReleaseDebtAlert(debt.ID); releaseErr != nil {
			logger.LogError(context.Background(), fmt.Sprintf("释放 Seedance 欠款告警 claim 失败：debt=%d err=%v", debt.ID, releaseErr))
		}
		return
	}
	if _, err := model.MarkTaskBillingDebtAlertSent(debt.ID); err != nil {
		logger.LogError(context.Background(), fmt.Sprintf("标记 Seedance 欠款告警失败：task=%s err=%v", input.TaskId, err))
	}
}

// seedanceDebtAlertLeaseSeconds 欠款告警 claim 租约（秒）：claim 后超时未
// 完成投递的会被其他实例回收重试（进程崩溃兜底）。
const seedanceDebtAlertLeaseSeconds = 120

// sendSeedanceDebtAlert 构造并发送欠款告警，返回是否**真正投递成功**。
// root 未配置任何通知渠道（无邮箱/webhook/bark/gotify）→ 返回 false 且不
// 标记 AlertSent（NotifyUser 对未配置渠道会静默返回 nil，绝不能据此视为成功）。
func sendSeedanceDebtAlert(input model.DebtInput, isAdmin, retry bool) bool {
	root := model.GetRootUser()
	if root == nil {
		logger.LogError(context.Background(), fmt.Sprintf("Seedance 欠款告警发送失败：找不到 Root 用户（task=%s）", input.TaskId))
		return false
	}
	if !rootNotifyConfigured(root) {
		logger.LogWarn(context.Background(), fmt.Sprintf("Seedance 欠款告警跳过：Root 未配置任何通知渠道（task=%s，欠款保留可重试/管理端可查看）", input.TaskId))
		return false
	}
	subject := "Seedance 任务差额欠款"
	if isAdmin {
		subject = "【最高级别】Seedance 管理员账号任务差额欠款"
	}
	if retry {
		subject = "Seedance 任务差额欠款（重试）"
	}
	message := fmt.Sprintf("用户 %d 的 Seedance 任务 %s（上游 %s，模型 %s，渠道 %d）实际费用高于预扣：预扣 %s，实际 %s，差额 %s 未能收取。%s",
		input.UserId, input.TaskId, input.UpstreamTaskId, input.ModelName, input.ChannelId,
		logger.FormatQuota(input.PreConsumedQuota), logger.FormatQuota(input.ActualQuota), logger.FormatQuota(input.DeltaQuota), input.Reason)
	err := NotifyUser(root.Id, root.Email, root.GetSetting(), dto.NewNotify(dto.NotifyTypeQuotaExceed, subject, message, nil))
	if err != nil {
		logger.LogError(context.Background(), fmt.Sprintf("Seedance 欠款告警发送失败（保留重试）：task=%s err=%v", input.TaskId, err))
		return false
	}
	return true
}

// rootNotifyConfigured 判断 Root 用户是否配置了至少一种可用通知渠道
// （NotifyUser 对未配置渠道会静默返回 nil，必须先在此拦截）。
func rootNotifyConfigured(user *model.User) bool {
	if user == nil {
		return false
	}
	setting := user.GetSetting()
	switch setting.NotifyType {
	case dto.NotifyTypeWebhook:
		return setting.WebhookUrl != ""
	case dto.NotifyTypeBark:
		return setting.BarkUrl != ""
	case dto.NotifyTypeGotify:
		return setting.GotifyUrl != "" && setting.GotifyToken != ""
	default: // email（默认渠道）
		if setting.NotificationEmail != "" {
			return true
		}
		return user.Email != ""
	}
}

// RetryPendingDebtAlerts 重试发送"告警未成功"的未清欠款通知（多实例安全：
// 每条先原子 claim，只有 claim 成功的实例才发送；发送失败释放 claim 保留
// 重试）。返回本次成功标记的数量。由 seedance_debt_alert 系统任务定期调用。
func RetryPendingDebtAlerts() int {
	debts, err := model.GetPendingDebtsWithAlertPending(100)
	if err != nil {
		logger.LogError(context.Background(), fmt.Sprintf("查询待重试欠款告警失败: %v", err))
		return 0
	}
	sent := 0
	for i := range debts {
		debt := &debts[i]
		// 原子 claim（防多实例重复发送；claim 失败的记录被其他实例处理/未到期）
		claimed, err := model.ClaimDebtAlert(debt.ID, seedanceDebtAlertLeaseSeconds)
		if err != nil || !claimed {
			continue
		}
		input := model.DebtInput{
			UserId:           debt.UserId,
			TaskId:           debt.TaskId,
			UpstreamTaskId:   debt.UpstreamTaskId,
			ModelName:        debt.ModelName,
			ChannelId:        debt.ChannelId,
			PreConsumedQuota: debt.PreConsumedQuota,
			ActualQuota:      debt.ActualQuota,
			DeltaQuota:       debt.DeltaQuota,
			Reason:           debt.Reason,
		}
		if !sendSeedanceDebtAlert(input, false, true) {
			if releaseErr := model.ReleaseDebtAlert(debt.ID); releaseErr != nil {
				logger.LogError(context.Background(), fmt.Sprintf("释放 Seedance 欠款告警 claim 失败：debt=%d err=%v", debt.ID, releaseErr))
			}
			continue
		}
		if ok, err := model.MarkTaskBillingDebtAlertSent(debt.ID); err == nil && ok {
			sent++
		}
	}
	if sent > 0 {
		logger.LogInfo(context.Background(), fmt.Sprintf("Seedance 欠款告警重试完成：成功 %d 条", sent))
	}
	return sent
}
