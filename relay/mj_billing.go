package relay

import (
	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/service"

	"github.com/gin-gonic/gin"
)

// consumeMjTaskBilling 执行 Midjourney 提交成功后的统一计费块：
//  1. 钱包扣款（PostConsumeQuota，尽力而为，失败仅告警）；
//  2. 消费日志（返回是否实际写入——LogConsumeEnabled 关闭或日志库写入失败均为 false）；
//  3. 预扣累计消耗（用户 used_quota + request_count + 渠道 used_quota），失败返回错误。
//
// 统计写入失败的语义：上游任务已创建、钱包已扣款，调用方必须**仍然**创建任务行并
// 标记 BillingStatsFailed=true（used_quota 从未累加，退款方向结算跳过累计消耗冲减，
// 见 ApplyWalletRefundUsedQuota），绝不能中止落库造成"扣款但本地无任务"。
//
// 返回值：消费日志是否实际写入成功。调用方应把它持久化为任务的 ConsumeLogRecorded，
// 保证"消费-退款/补扣"日志口径对称，绝不使用全局开关 common.LogConsumeEnabled 代替。
func consumeMjTaskBilling(c *gin.Context, info *relaycommon.RelayInfo, quota int, logContent string, other map[string]interface{}, modelName string) (bool, error) {
	if err := service.PostConsumeQuota(info, quota, 0, true); err != nil {
		common.SysLog("error consuming token remain quota: " + err.Error())
	}
	tokenName := c.GetString("token_name")
	consumeLogRecorded := model.RecordConsumeLog(c, info.UserId, model.RecordConsumeLogParams{
		ChannelId: info.ChannelId,
		ModelName: modelName,
		TokenName: tokenName,
		Quota:     quota,
		Content:   logContent,
		TokenId:   info.TokenId,
		Group:     info.UsingGroup,
		Other:     other,
	})
	if err := model.ApplyPreConsumeUsedQuota(info.UserId, info.ChannelId, quota); err != nil {
		return consumeLogRecorded, err
	}
	return consumeLogRecorded, nil
}

// applyMjBillingAndMark 执行 Midjourney 提交成功后的计费块并回填任务标记：
//   - ConsumeLogRecorded ← RecordConsumeLog 实际写入结果；
//   - 统计写入失败（ApplyPreConsumeUsedQuota 失败）时置 task.BillingStatsFailed=true。
//
// 统计失败不中止任务落库（上游已创建、钱包已扣款，本地必须保留任务生命周期记录），
// 退款路径按 BillingStatsFailed 跳过累计消耗冲减（见 ApplyWalletRefundUsedQuota）。
// 供 RelayMidjourneySubmit / RelaySwapFace 共用，保证两条路径标记逻辑一致。
func applyMjBillingAndMark(c *gin.Context, info *relaycommon.RelayInfo, task *model.Midjourney, quota int, logContent string, other map[string]interface{}, modelName string) {
	consumeLogRecorded, bErr := consumeMjTaskBilling(c, info, quota, logContent, other, modelName)
	task.ConsumeLogRecorded = consumeLogRecorded
	if bErr != nil {
		// 预扣累计消耗失败（用户/渠道行缺失或数据库错误）：标记统计缺失，
		// 任务行照常插入——否则"扣款且上游已创建但本地无任务"，退款/轮询
		// 全部失效。退款路径按 BillingStatsFailed 跳过累计消耗冲减。
		task.BillingStatsFailed = true
		common.SysError("midjourney pre-consume used quota failed: " + bErr.Error())
	}
}
