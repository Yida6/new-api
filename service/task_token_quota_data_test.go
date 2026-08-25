package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	relaycommon "github.com/QuantumNous/new-api/relay/common"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// ---------------------------------------------------------------------------
// 数据看板总 Token 修复回归测试
// 背景：异步任务（Seedance 等）提交瞬间上游无 usage，提交消费日志写 quota_data
// 时 TokenUsed=0；结算完成后拿到实际 total_tokens。此前只回填 logs.other.total_tokens，
// 看板（读 quota_data.token_used）仍为 0。本组测试验证：
//   结算 → 持久化 task.TotalTokens → SaveQuotaDataCache 内 ReconcileTaskTokensToQuotaData
//   把 total_tokens 累加进 quota_data.token_used，且幂等、不改变 count/quota。
// ---------------------------------------------------------------------------

// sumTokenByUser 汇总某用户 quota_data 的 token_used / count / quota。
func sumTokenByUser(t *testing.T, userID int) (tokens int, count int, quota int64) {
	t.Helper()
	rows, err := model.GetQuotaDataByUserId(userID, 0, time.Now().Add(time.Hour).Unix())
	require.NoError(t, err)
	for _, r := range rows {
		tokens += r.TokenUsed
		count += r.Count
		quota += r.Quota
	}
	return tokens, count, quota
}

// settleTaskWithTokens 直接调用 RecalculateTaskQuota（结算路径）并携带 totalTokens，
// 模拟任务完成结算拿到上游实际 token 数。
func settleTaskWithTokens(ctx context.Context, task *model.Task, actualQuota int64, totalTokens int) {
	RecalculateTaskQuota(ctx, task, actualQuota, "test结算", totalTokens)
}

// 场景1（含补扣/退款/预扣命中）：结算拿到 total_tokens 后，看板 token_used 累加为
// 实际值，且 count 仍为 1（请求只计一次）、quota 为净消费，与计费差额解耦。
func TestTaskTokenBackfill_CountsActualTokensRegardlessOfDelta(t *testing.T) {
	truncate(t)
	enableDataExport(t)
	ctx := context.Background()

	const userID, tokenID, channelID = 8101, 8101, 8101
	const preConsumed = 5000
	const totalTokens = 12345

	cases := []struct {
		name        string
		actualQuota int64 // 实际应扣额度
		deltaLabel  string
	}{
		{"补扣 delta>0", 7000, "topup"},
		{"退款 delta<0", 1000, "refund"},
		{"预扣命中 delta==0", 5000, "exact"},
	}

	for _, tc := range cases {
		t.Run(tc.deltaLabel, func(t *testing.T) {
			truncate(t)
			seedUser(t, userID, 100000)
			seedToken(t, tokenID, userID, "sk-token-backfill-"+tc.deltaLabel, 100000)
			seedChannel(t, channelID)
			seedUsedQuota(t, userID, channelID, preConsumed)

			task := simulateTaskSubmit(t, userID, channelID, tokenID, preConsumed)
			settleTaskWithTokens(ctx, task, tc.actualQuota, totalTokens)

			// 结算后 task.TotalTokens 已持久化
			var reloaded model.Task
			require.NoError(t, model.DB.Select("total_tokens").Where("id = ?", task.ID).First(&reloaded).Error)
			assert.Equal(t, totalTokens, reloaded.TotalTokens)

			// flush（缓存落库 + token 补录）
			flushQuotaData(t)
			tokens, count, quota := sumTokenByUser(t, userID)
			assert.Equal(t, totalTokens, tokens, "看板 token_used 必须等于实际 total_tokens（与差额方向无关）")
			assert.Equal(t, 1, count, "补录 token 不得增加请求计数")
			assert.Equal(t, tc.actualQuota, quota, "quota 保持净消费，不受 token 补录影响")
		})
	}
}

// 场景5：同一任务重复结算/重复轮询不会重复累计 Token（幂等）。
func TestTaskTokenBackfill_IdempotentAcrossRepeatSettle(t *testing.T) {
	truncate(t)
	enableDataExport(t)
	ctx := context.Background()

	const userID, tokenID, channelID = 8102, 8102, 8102
	const preConsumed, totalTokens = 5000, 8888

	seedUser(t, userID, 100000)
	seedToken(t, tokenID, userID, "sk-token-idem", 100000)
	seedChannel(t, channelID)
	seedUsedQuota(t, userID, channelID, preConsumed)

	task := simulateTaskSubmit(t, userID, channelID, tokenID, preConsumed)

	// 第一次结算
	settleTaskWithTokens(ctx, task, preConsumed, totalTokens)
	flushQuotaData(t)
	tokens1, _, _ := sumTokenByUser(t, userID)
	assert.Equal(t, totalTokens, tokens1)

	// 重复结算（模拟重复轮询/结算重试：任务已终态但再次进入结算）
	settleTaskWithTokens(ctx, task, preConsumed, totalTokens)
	flushQuotaData(t)
	tokens2, count2, _ := sumTokenByUser(t, userID)
	assert.Equal(t, totalTokens, tokens2, "重复结算不得重复累计 Token")
	assert.Equal(t, 1, count2, "count 不受影响")

	// 直接重复调用持久化函数（服务重启后重新结算同一任务）仍幂等
	okIdem, errIdem := model.RecordTaskTotalTokensToQuotaData(task, totalTokens)
	assert.NoError(t, errIdem)
	assert.True(t, okIdem)
	flushQuotaData(t)
	tokens3, _, _ := sumTokenByUser(t, userID)
	assert.Equal(t, totalTokens, tokens3, "重复持久化+补录不得重复累计")
}

// 场景6：多个任务落入同一小时、同一模型聚合桶时能正确累加。
func TestTaskTokenBackfill_AggregatesMultipleTasksSameBucket(t *testing.T) {
	truncate(t)
	enableDataExport(t)
	ctx := context.Background()

	const userID, tokenID, channelID = 8103, 8103, 8103
	const preConsumed = 1000

	seedUser(t, userID, 1000000)
	seedToken(t, tokenID, userID, "sk-token-agg", 1000000)
	seedChannel(t, channelID)
	seedUsedQuota(t, userID, channelID, preConsumed)

	// 同一小时、同一模型的多个任务
	total := 0
	for i := 0; i < 3; i++ {
		task := simulateTaskSubmit(t, userID, channelID, tokenID, preConsumed)
		tokens := 1000 + i*100 // 1000, 1100, 1200
		settleTaskWithTokens(ctx, task, preConsumed, tokens)
		total += tokens
	}
	flushQuotaData(t)

	tokens, count, quota := sumTokenByUser(t, userID)
	assert.Equal(t, total, tokens, "多任务同桶必须正确累加 token_used")
	assert.Equal(t, 3, count, "三次请求各计一次")
	assert.Equal(t, int64(3*preConsumed), quota)
}

// 场景7：任务完成早于 quota_data 缓存首次刷盘时不会丢失。
// 提交写缓存（Count=1,Token=0）→ 结算持久化 TotalTokens → 尚未 flush；
// 首次 flush 必须把 Token 补录进去。
func TestTaskTokenBackfill_NoLossBeforeFirstFlush(t *testing.T) {
	truncate(t)
	enableDataExport(t)
	ctx := context.Background()

	const userID, tokenID, channelID = 8104, 8104, 8104
	const preConsumed, totalTokens = 3000, 6666

	seedUser(t, userID, 100000)
	seedToken(t, tokenID, userID, "sk-token-flush", 100000)
	seedChannel(t, channelID)
	seedUsedQuota(t, userID, channelID, preConsumed)

	// 提交写缓存（尚未刷盘）
	task := simulateTaskSubmit(t, userID, channelID, tokenID, preConsumed)
	// 任务立即完成并结算（此时缓存仍未被 SaveQuotaDataCache 刷盘）
	settleTaskWithTokens(ctx, task, preConsumed, totalTokens)

	// 首次 flush：提交统计 + token 补录一起落库
	flushQuotaData(t)
	tokens, count, quota := sumTokenByUser(t, userID)
	assert.Equal(t, totalTokens, tokens, "首次刷盘不得丢失补录 Token")
	assert.Equal(t, 1, count)
	assert.Equal(t, int64(preConsumed), quota)
}

// 场景8：total_tokens 缺失或为 0 时不写入伪造数据。
func TestTaskTokenBackfill_IgnoresMissingOrZeroTokens(t *testing.T) {
	truncate(t)
	enableDataExport(t)
	ctx := context.Background()

	const userID, tokenID, channelID = 8105, 8105, 8105
	const preConsumed = 2000

	seedUser(t, userID, 100000)
	seedToken(t, tokenID, userID, "sk-token-zero", 100000)
	seedChannel(t, channelID)
	seedUsedQuota(t, userID, channelID, preConsumed)

	task := simulateTaskSubmit(t, userID, channelID, tokenID, preConsumed)
	// total_tokens 缺失（0）：不得持久化，也不得写 quota_data
	okZero, errZero := model.RecordTaskTotalTokensToQuotaData(task, 0)
	assert.NoError(t, errZero)
	assert.False(t, okZero)
	okNeg, errNeg := model.RecordTaskTotalTokensToQuotaData(task, -5)
	assert.NoError(t, errNeg)
	assert.False(t, okNeg)

	// 结算但 totalTokens=0（无 usage）：不写伪造 token
	settleTaskWithTokens(ctx, task, preConsumed, 0)
	flushQuotaData(t)
	tokens, count, quota := sumTokenByUser(t, userID)
	assert.Equal(t, 0, tokens, "无 total_tokens 不得写入伪造数据")
	assert.Equal(t, 1, count)
	assert.Equal(t, int64(preConsumed), quota)

	// task.TotalTokens 保持 0
	var reloaded model.Task
	require.NoError(t, model.DB.Select("total_tokens, token_quota_synced").Where("id = ?", task.ID).First(&reloaded).Error)
	assert.Equal(t, 0, reloaded.TotalTokens)
	assert.Equal(t, 0, reloaded.TokenQuotaSynced, "未回填的任务不得置位已同步标记")
}

// 场景9：Count、Quota 和现有日志统计行为不发生回归。
// 补录 token 前后，SumUsedQuota 的净消费、请求计数与日志类型分布保持一致。
func TestTaskTokenBackfill_NoStatRegression(t *testing.T) {
	truncate(t)
	enableDataExport(t)
	ctx := context.Background()

	const userID, tokenID, channelID = 8106, 8106, 8106
	const preConsumed, actualQuota, totalTokens = 5000, 1000, 7777

	seedUser(t, userID, 100000)
	seedToken(t, tokenID, userID, "sk-token-reg", 100000)
	seedChannel(t, channelID)
	seedUsedQuota(t, userID, channelID, preConsumed)

	task := simulateTaskSubmit(t, userID, channelID, tokenID, preConsumed)
	settleTaskWithTokens(ctx, task, actualQuota, totalTokens)

	// 补录前统计口径（service 测试环境未调用 model.initCol，group 过滤列未初始化，
	// 因此与既有 service 测试一致，group 传空串）
	statBefore, err := model.SumUsedQuota(0, 0, 0, "test-model", "test_user", "test_token", channelID, "", "", "")
	require.NoError(t, err)

	flushQuotaData(t)

	// 补录后统计口径（token 补录不参与 SumUsedQuota，只改 quota_data.token_used）
	statAfter, err := model.SumUsedQuota(0, 0, 0, "test-model", "test_user", "test_token", channelID, "", "", "")
	require.NoError(t, err)
	assert.Equal(t, statBefore.Quota, statAfter.Quota, "净消费统计不受 token 补录影响")
	assert.Equal(t, statBefore.Rpm, statAfter.Rpm, "RPM 不受 token 补录影响")

	// 请求计数与日志类型分布（消费 1 条 + 退款/补扣 1 条）
	var consumeLogs, refundLogs int64
	model.LOG_DB.Model(&model.Log{}).Where("user_id = ? AND type = ?", userID, model.LogTypeConsume).Count(&consumeLogs)
	model.LOG_DB.Model(&model.Log{}).Where("user_id = ? AND type = ?", userID, model.LogTypeRefund).Count(&refundLogs)
	assert.EqualValues(t, 1, consumeLogs)
	assert.EqualValues(t, 1, refundLogs)

	// quota_data 汇总：count=1、quota=净消费、token=实际 total_tokens
	tokens, count, quota := sumTokenByUser(t, userID)
	assert.Equal(t, totalTokens, tokens)
	assert.Equal(t, 1, count)
	assert.Equal(t, int64(actualQuota), quota)
}

// DataExportEnabled=false 时，结算不持久化 TotalTokens，也不补录 quota_data。
func TestTaskTokenBackfill_DisabledExportKeepsBehavior(t *testing.T) {
	truncate(t)
	ctx := context.Background()
	// common.DataExportEnabled 默认 true，这里显式关掉以验证保持原行为
	prevExport := common.DataExportEnabled
	common.DataExportEnabled = false
	t.Cleanup(func() { common.DataExportEnabled = prevExport })

	const userID, tokenID, channelID = 8107, 8107, 8107
	const preConsumed = 2000

	seedUser(t, userID, 100000)
	seedToken(t, tokenID, userID, "sk-token-disabled", 100000)
	seedChannel(t, channelID)
	seedUsedQuota(t, userID, channelID, preConsumed)

	task := simulateTaskSubmit(t, userID, channelID, tokenID, preConsumed)

	// DataExportEnabled=false：RecordTaskTotalTokensToQuotaData 直接拒绝
	okDisabled, errDisabled := model.RecordTaskTotalTokensToQuotaData(task, 1234)
	assert.NoError(t, errDisabled)
	assert.False(t, okDisabled)

	// 结算后 task.TotalTokens 不持久化
	settleTaskWithTokens(ctx, task, preConsumed, 1234)
	var reloaded model.Task
	require.NoError(t, model.DB.Select("total_tokens").Where("id = ?", task.ID).First(&reloaded).Error)
	assert.Equal(t, 0, reloaded.TotalTokens, "DataExportEnabled=false 时不持久化 token")
}

// 场景11（P2）：total_tokens 持久化失败（数据库短暂不可用）时，结算必须转为可重试，
// 不得静默忽略导致 Token 永久缺失。通过注入 UPDATE 失败回调模拟数据库故障。
func TestTaskTokenBackfill_PersistFailureMakesSettleRetryable(t *testing.T) {
	truncate(t)
	enableDataExport(t)
	ctx := context.Background()

	const userID, tokenID, channelID = 8108, 8108, 8108
	const preConsumed, totalTokens = 5000, 9999

	seedUser(t, userID, 100000)
	seedToken(t, tokenID, userID, "sk-token-fail", 100000)
	seedChannel(t, channelID)
	seedUsedQuota(t, userID, channelID, preConsumed)

	task := simulateTaskSubmit(t, userID, channelID, tokenID, preConsumed)

	// 注入：对 tasks 表 UPDATE 强制失败（模拟数据库故障）
	callbackName := "test:fail_task_total_tokens_persist"
	injected := errors.New("injected task total_tokens persist failure")
	require.NoError(t, model.DB.Callback().Update().Before("gorm:update").Register(callbackName, func(tx *gorm.DB) {
		if tx.Statement != nil && tx.Statement.Table == "tasks" {
			tx.AddError(injected)
		}
	}))
	t.Cleanup(func() { _ = model.DB.Callback().Update().Remove(callbackName) })

	// 1. RecordTaskTotalTokensToQuotaData 应返回 error
	okFail, errFail := model.RecordTaskTotalTokensToQuotaData(task, totalTokens)
	assert.False(t, okFail)
	assert.ErrorIs(t, errFail, injected)

	// 2. RecalculateTaskQuota 携带 totalTokens 时应因持久化失败返回 false（结算可重试）
	assert.False(t, RecalculateTaskQuota(ctx, task, preConsumed, "test", totalTokens),
		"total_tokens 持久化失败必须使结算可重试，而非静默成功")

	// 3. settleTaskBillingOnComplete 同样返回 false（保持任务非终态，下轮重试）
	taskResult := &relaycommon.TaskInfo{Status: model.TaskStatusSuccess, TotalTokens: totalTokens}
	assert.False(t, settleTaskBillingOnComplete(ctx, &mockAdaptor{}, task, taskResult),
		"persist 失败必须返回 false，使任务回退非终态")

	// 移除故障回调后，结算可正常完成
	_ = model.DB.Callback().Update().Remove(callbackName)
	assert.True(t, RecalculateTaskQuota(ctx, task, preConsumed, "test", totalTokens))
	flushQuotaData(t)
	tokens, count, _ := sumTokenByUser(t, userID)
	assert.Equal(t, totalTokens, tokens, "故障恢复后 token 正常补录")
	assert.Equal(t, 1, count)
}
