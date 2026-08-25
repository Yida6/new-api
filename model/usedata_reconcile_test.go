package model

import (
	"fmt"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// 异步任务实际 token 计入数据看板（quota_data.token_used）回归测试
// ---------------------------------------------------------------------------
// 背景：异步任务（Seedance 等）提交瞬间上游无 usage，提交消费日志写 quota_data
// 时 TokenUsed=0；结算完成后由 RecordTaskTotalTokensToQuotaData 持久化到 task 行
// TotalTokens（durable），后台 ReconcileTaskTokensToQuotaData 以"值差额"幂等语义
// 把 (TotalTokens - TokenQuotaSynced) 累加进对应小时桶。排行榜（读
// quota_data.token_used）据此统计模型。
// ---------------------------------------------------------------------------

// seedReconcileTask 造一个已结算任务（TotalTokens 已持久化），并预置该任务的
// 提交消费日志对应 quota_data 桶（模拟提交时 TokenUsed=0 的记录）。
func seedReconcileTask(t *testing.T, userID int, submitTime int64, preConsumed int64, totalTokens int) *Task {
	t.Helper()
	require.NoError(t, DB.Create(&User{Id: userID, Username: fmt.Sprintf("reconcile_%d", userID), Quota: 100000, Status: common.UserStatusEnabled}).Error)
	require.NoError(t, DB.Create(&Token{Id: userID, UserId: userID, Key: fmt.Sprintf("sk-r-%d", userID), Name: "t", Status: common.TokenStatusEnabled}).Error)
	require.NoError(t, DB.Create(&Channel{Id: userID, Name: "c", Key: "k", Status: common.ChannelStatusEnabled}).Error)

	task := &Task{
		TaskID:    fmt.Sprintf("task_rec_%d", userID),
		UserId:    userID,
		ChannelId: userID,
		Group:     "default",
		Quota:     preConsumed,
		Status:    TaskStatusSuccess,
		SubmitTime: submitTime,
		CreatedAt:  submitTime,
		Properties: Properties{OriginModelName: "doubao-seedance-2-0-260128"},
		PrivateData: TaskPrivateData{
			TokenId:  userID,
			NodeName: "node-a",
		},
	}
	require.NoError(t, DB.Create(task).Error)

	// 模拟结算：持久化 TotalTokens 到 task 行（生产路径由
	// RecordTaskTotalTokensToQuotaData 在结算时完成；totalTokens<=0 时不持久化）。
	if totalTokens > 0 {
		ok, err := RecordTaskTotalTokensToQuotaData(task, totalTokens)
		require.NoError(t, err)
		require.True(t, ok)
	}

	// 模拟提交消费日志写入的 quota_data 桶（TokenUsed=0），结算补录应命中同一桶。
	hour := submitTime - (submitTime % 3600)
	require.NoError(t, DB.Table("quota_data").Create(&QuotaData{
		UserID:    userID,
		Username:  fmt.Sprintf("reconcile_%d", userID),
		ModelName: "doubao-seedance-2-0-260128",
		CreatedAt: hour,
		UseGroup:  "default",
		TokenID:   userID,
		ChannelID: userID,
		NodeName:  "node-a",
		Count:     1,
		Quota:     preConsumed,
		TokenUsed: 0,
	}).Error)
	return task
}

func enableReconcileExport(t *testing.T) {
	t.Helper()
	old := common.DataExportEnabled
	common.DataExportEnabled = true
	t.Cleanup(func() { common.DataExportEnabled = old })
}

// 场景1：异步任务结算 40594 Token 后，排行榜能统计到该模型。
func TestReconcileTaskTokens_RankingReflectsTotalTokens(t *testing.T) {
	truncateTables(t)
	clearQuotaDataCache(t)
	enableReconcileExport(t)

	submitTime := time.Now().Unix()
	seedReconcileTask(t, 7001, submitTime, 5000, 40594)

	require.Equal(t, 1, ReconcileTaskTokensToQuotaData())

	rows, err := GetRankingQuotaTotals(submitTime-3600, submitTime+3600)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, "doubao-seedance-2-0-260128", rows[0].ModelName)
	assert.EqualValues(t, 40594, rows[0].TotalTokens)
}

// 场景2：同一个任务以 40594 重复结算两次，最终只增加 40594。
func TestReconcileTaskTokens_RepeatedSettleDoesNotDoubleCount(t *testing.T) {
	truncateTables(t)
	clearQuotaDataCache(t)
	enableReconcileExport(t)

	submitTime := time.Now().Unix()
	task := seedReconcileTask(t, 7002, submitTime, 5000, 40594)

	// 第一次结算 → 落 40594
	ok1, err1 := RecordTaskTotalTokensToQuotaData(task, 40594)
	require.NoError(t, err1)
	require.True(t, ok1)
	require.Equal(t, 1, ReconcileTaskTokensToQuotaData())

	// 重复结算：TotalTokens 仍为 40594，差额为 0，不再累计
	ok2, err2 := RecordTaskTotalTokensToQuotaData(task, 40594)
	require.NoError(t, err2)
	require.True(t, ok2)
	require.Equal(t, 0, ReconcileTaskTokensToQuotaData())

	rows, err := GetRankingQuotaTotals(submitTime-3600, submitTime+3600)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.EqualValues(t, 40594, rows[0].TotalTokens)
}

// 场景3：已同步 40000，随后更正为 40594，最终总量为 40594（只追加差额 594）。
func TestReconcileTaskTokens_CorrectionAppendsOnlyDelta(t *testing.T) {
	truncateTables(t)
	clearQuotaDataCache(t)
	enableReconcileExport(t)

	submitTime := time.Now().Unix()
	task := seedReconcileTask(t, 7003, submitTime, 5000, 40000)

	ok1, err1 := RecordTaskTotalTokensToQuotaData(task, 40000)
	require.NoError(t, err1)
	require.True(t, ok1)
	require.Equal(t, 1, ReconcileTaskTokensToQuotaData())

	// 更正：TotalTokens 40000 → 40594，只追加 594
	ok2, err2 := RecordTaskTotalTokensToQuotaData(task, 40594)
	require.NoError(t, err2)
	require.True(t, ok2)
	require.Equal(t, 1, ReconcileTaskTokensToQuotaData())

	rows, err := GetRankingQuotaTotals(submitTime-3600, submitTime+3600)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.EqualValues(t, 40594, rows[0].TotalTokens)
}

// 场景4：quotaDelta == 0（预扣精确命中）仍同步 Token —— 结算路径仍持久化
// TotalTokens，补录照样累计。
func TestReconcileTaskTokens_ZeroQuotaDeltaStillSyncsTokens(t *testing.T) {
	truncateTables(t)
	clearQuotaDataCache(t)
	enableReconcileExport(t)

	submitTime := time.Now().Unix()
	// preConsumed 与实际总 token 无必然关系：quotaDelta==0 不影响 token 统计
	seedReconcileTask(t, 7004, submitTime, 40594, 40594)

	require.Equal(t, 1, ReconcileTaskTokensToQuotaData())

	rows, err := GetRankingQuotaTotals(submitTime-3600, submitTime+3600)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.EqualValues(t, 40594, rows[0].TotalTokens)
}

// 场景5：补扣和退款日志不会再次累计 Token —— Token 统计只来自 TotalTokens，
// 与计费调整日志（Adjustment 记录）完全解耦。
func TestReconcileTaskTokens_AdjustmentsDoNotAddTokens(t *testing.T) {
	truncateTables(t)
	clearQuotaDataCache(t)
	enableReconcileExport(t)

	submitTime := time.Now().Unix()
	seedReconcileTask(t, 7005, submitTime, 5000, 40594)

	// 模拟补扣 + 退款调整日志写 quota_data（TokenUsed 均为 0）
	RecordTaskBillingLog(RecordTaskBillingLogParams{
		UserId: 7005, LogType: LogTypeConsume, Content: "adaptor计费调整",
		ChannelId: 7005, ModelName: "doubao-seedance-2-0-260128", Quota: 2000, Group: "default",
		Other: map[string]interface{}{"task_id": "t5", "is_task": true},
	})
	RecordTaskBillingLog(RecordTaskBillingLogParams{
		UserId: 7005, LogType: LogTypeRefund, Content: "",
		ChannelId: 7005, ModelName: "doubao-seedance-2-0-260128", Quota: 3000, Group: "default",
		Other: map[string]interface{}{"task_id": "t5", "is_task": true},
	})

	require.Equal(t, 1, ReconcileTaskTokensToQuotaData())

	rows, err := GetRankingQuotaTotals(submitTime-3600, submitTime+3600)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	// 只有提交桶的 40594，调整日志不增加 token
	assert.EqualValues(t, 40594, rows[0].TotalTokens)
}

// 场景6：totalTokens <= 0 不写入（RecordTaskTotalTokensToQuotaData 直接拒绝）。
func TestReconcileTaskTokens_NonPositiveTotalTokensIgnored(t *testing.T) {
	truncateTables(t)
	clearQuotaDataCache(t)
	enableReconcileExport(t)

	submitTime := time.Now().Unix()
	task := seedReconcileTask(t, 7006, submitTime, 5000, 0)

	ok0, err0 := RecordTaskTotalTokensToQuotaData(task, 0)
	assert.NoError(t, err0)
	assert.False(t, ok0)
	okN, errN := RecordTaskTotalTokensToQuotaData(task, -5)
	assert.NoError(t, errN)
	assert.False(t, okN)
	assert.Equal(t, 0, ReconcileTaskTokensToQuotaData())

	rows, err := GetRankingQuotaTotals(submitTime-3600, submitTime+3600)
	require.NoError(t, err)
	require.Empty(t, rows)
}

// 场景7：普通同步请求原有 Token 统计不受影响（RecordConsumeLog 直接写
// TokenUsed，reconcile 不触碰无 TotalTokens 的任务）。
func TestReconcileTaskTokens_SyncRequestTokensUnaffected(t *testing.T) {
	truncateTables(t)
	clearQuotaDataCache(t)
	enableReconcileExport(t)

	submitTime := time.Now().Unix()
	require.NoError(t, DB.Create(&User{Id: 7101, Username: "sync_user", Quota: 100000, Status: common.UserStatusEnabled}).Error)
	// 同步请求直接带 TokenUsed=123 写入
	LogQuotaData(QuotaDataLogParams{
		UserID:    7101,
		Username:  "sync_user",
		ModelName: "deepseek-v3",
		Quota:     1000,
		CreatedAt: submitTime,
		TokenUsed: 123,
		UseGroup:  "default",
	})
	SaveQuotaDataCache()

	// 无异步任务需要补录
	assert.Equal(t, 0, ReconcileTaskTokensToQuotaData())

	rows, err := GetRankingQuotaTotals(submitTime-3600, submitTime+3600)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, "deepseek-v3", rows[0].ModelName)
	assert.EqualValues(t, 123, rows[0].TotalTokens)
}

// 场景8：排行榜查询能返回模型、历史桶和供应商份额。
func TestRankingQueriesReturnTotalsBucketsAndShares(t *testing.T) {
	truncateTables(t)
	clearQuotaDataCache(t)
	enableReconcileExport(t)

	submitTime := time.Now().Unix()
	seedReconcileTask(t, 7201, submitTime, 5000, 20000)
	require.Equal(t, 1, ReconcileTaskTokensToQuotaData())

	// 模型总榜
	totals, err := GetRankingQuotaTotals(submitTime-3600, submitTime+3600)
	require.NoError(t, err)
	require.Len(t, totals, 1)
	assert.Equal(t, "doubao-seedance-2-0-260128", totals[0].ModelName)
	assert.EqualValues(t, 20000, totals[0].TotalTokens)

	// 历史桶（按小时桶）
	buckets, err := GetRankingQuotaBuckets(submitTime-3600, submitTime+3600, 3600)
	require.NoError(t, err)
	require.Len(t, buckets, 1)
	assert.Equal(t, "doubao-seedance-2-0-260128", buckets[0].ModelName)
	assert.EqualValues(t, 20000, buckets[0].Tokens)

	// 供应商份额：按模型分组占比=100%
	shares, err := GetRankingQuotaTotals(submitTime-3600, submitTime+3600)
	require.NoError(t, err)
	require.Len(t, shares, 1)
	assert.EqualValues(t, 20000, shares[0].TotalTokens)
}

// 场景9（P1 自愈）：存量任务 tasks.total_tokens 恒为 0（字段迁移默认值），但
// task.Data 原始上游响应含 usage.total_tokens。ReconcileTaskTokensToQuotaData 的
// recoverTasksFromData 应能从 task.Data 解析出实际 total_tokens，持久化后补录进
// quota_data，使看板/排行榜恢复历史统计。重复执行不重复累计。
func TestReconcileTaskTokens_RecoversFromTaskData(t *testing.T) {
	truncateTables(t)
	clearQuotaDataCache(t)
	enableReconcileExport(t)

	submitTime := time.Now().Unix()
	hour := submitTime - (submitTime % 3600)
	const totalTokens = 40594

	require.NoError(t, DB.Create(&User{Id: 7301, Username: "recover_7301", Quota: 100000, Status: common.UserStatusEnabled}).Error)
	require.NoError(t, DB.Create(&Token{Id: 7301, UserId: 7301, Key: "sk-r-7301", Name: "t", Status: common.TokenStatusEnabled}).Error)
	require.NoError(t, DB.Create(&Channel{Id: 7301, Name: "c", Key: "k", Status: common.ChannelStatusEnabled}).Error)

	// 历史任务：total_tokens=0、token_quota_synced=0、终态成功，但 task.Data 含 usage。
	data, _ := common.Marshal(map[string]interface{}{
		"status": "success",
		"usage":  map[string]interface{}{"total_tokens": totalTokens},
	})
	task := &Task{
		TaskID:    "task_recover_7301",
		UserId:    7301,
		ChannelId: 7301,
		Group:     "default",
		Quota:     5000,
		Status:    TaskStatusSuccess,
		SubmitTime: submitTime,
		CreatedAt:  submitTime,
		Data:       data,
		Properties: Properties{OriginModelName: "doubao-seedance-2-0-260128"},
		PrivateData: TaskPrivateData{
			TokenId:  7301,
			NodeName: "node-a",
		},
	}
	require.NoError(t, DB.Create(task).Error)
	// 模拟提交消费日志桶（TokenUsed=0）
	require.NoError(t, DB.Table("quota_data").Create(&QuotaData{
		UserID:    7301,
		Username:  "recover_7301",
		ModelName: "doubao-seedance-2-0-260128",
		CreatedAt: hour,
		UseGroup:  "default",
		TokenID:   7301,
		ChannelID: 7301,
		NodeName:  "node-a",
		Count:     1,
		Quota:     5000,
		TokenUsed: 0,
	}).Error)

	// 自愈：从 task.Data 解析 total_tokens 并补录
	require.Equal(t, 1, ReconcileTaskTokensToQuotaData())

	rows, err := GetRankingQuotaTotals(submitTime-3600, submitTime+3600)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, "doubao-seedance-2-0-260128", rows[0].ModelName)
	assert.EqualValues(t, totalTokens, rows[0].TotalTokens)

	// 幂等：再跑一次不再累计
	require.Equal(t, 0, ReconcileTaskTokensToQuotaData())
	rows2, err := GetRankingQuotaTotals(submitTime-3600, submitTime+3600)
	require.NoError(t, err)
	require.Len(t, rows2, 1)
	assert.EqualValues(t, totalTokens, rows2[0].TotalTokens)

	// task 行已持久化 total_tokens
	var reloaded Task
	require.NoError(t, DB.Select("total_tokens, token_quota_synced").Where("id = ?", task.ID).First(&reloaded).Error)
	assert.Equal(t, totalTokens, reloaded.TotalTokens)
	assert.Equal(t, totalTokens, reloaded.TokenQuotaSynced)
}

// 场景11（P1 日志兜底自愈）：存量任务 task.Data 无 usage（上游响应未含 usage），
// 但结算/提交日志 other.total_tokens 已含实际 token（older 版本结算写入）。
// recoverTasksFromLogs 应按 task_id 从日志回填 tasks.total_tokens 并补录进
// quota_data，使看板/排行榜恢复历史统计。重复执行不重复累计。
func TestReconcileTaskTokens_RecoversFromLogs(t *testing.T) {
	truncateTables(t)
	clearQuotaDataCache(t)
	enableReconcileExport(t)

	submitTime := time.Now().Unix()
	hour := submitTime - (submitTime % 3600)
	const totalTokens = 40594

	require.NoError(t, DB.Create(&User{Id: 7401, Username: "recover_log_7401", Quota: 100000, Status: common.UserStatusEnabled}).Error)
	require.NoError(t, DB.Create(&Token{Id: 7401, UserId: 7401, Key: "sk-rl-7401", Name: "t", Status: common.TokenStatusEnabled}).Error)
	require.NoError(t, DB.Create(&Channel{Id: 7401, Name: "c", Key: "k", Status: common.ChannelStatusEnabled}).Error)

	// 历史任务：total_tokens=0、token_quota_synced=0、终态成功、task.Data 无 usage。
	task := &Task{
		TaskID:    "task_recover_log_7401",
		UserId:    7401,
		ChannelId: 7401,
		Group:     "default",
		Quota:     5000,
		Status:    TaskStatusSuccess,
		SubmitTime: submitTime,
		CreatedAt:  submitTime,
		// Data 刻意不含 usage（区别于 recoverTasksFromData 场景）
		Properties: Properties{OriginModelName: "doubao-seedance-2-0-260128"},
		PrivateData: TaskPrivateData{
			TokenId:  7401,
			NodeName: "node-a",
		},
	}
	require.NoError(t, DB.Create(task).Error)

	// 模拟结算/调整日志已含 other.task_id 与 other.total_tokens（older 版本路径）。
	RecordTaskBillingLog(RecordTaskBillingLogParams{
		UserId: 7401, LogType: LogTypeConsume, Content: "历史结算",
		ChannelId: 7401, ModelName: "doubao-seedance-2-0-260128", Quota: 5000,
		TokenId: 7401, Group: "default",
		Other:       map[string]interface{}{"task_id": task.TaskID, "is_task": true},
		TotalTokens: totalTokens,
	})
	// 模拟提交消费日志桶（TokenUsed=0），补录应命中同一小时桶。
	require.NoError(t, DB.Table("quota_data").Create(&QuotaData{
		UserID:    7401,
		Username:  "recover_log_7401",
		ModelName: "doubao-seedance-2-0-260128",
		CreatedAt: hour,
		UseGroup:  "default",
		TokenID:   7401,
		ChannelID: 7401,
		NodeName:  "node-a",
		Count:     1,
		Quota:     5000,
		TokenUsed: 0,
	}).Error)

	// 自愈：从日志 other.total_tokens 回填并补录。
	require.Equal(t, 1, ReconcileTaskTokensToQuotaData())

	rows, err := GetRankingQuotaTotals(submitTime-3600, submitTime+3600)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, "doubao-seedance-2-0-260128", rows[0].ModelName)
	assert.EqualValues(t, totalTokens, rows[0].TotalTokens)

	// 幂等：再跑一次不再累计。
	require.Equal(t, 0, ReconcileTaskTokensToQuotaData())
	rows2, err := GetRankingQuotaTotals(submitTime-3600, submitTime+3600)
	require.NoError(t, err)
	require.Len(t, rows2, 1)
	assert.EqualValues(t, totalTokens, rows2[0].TotalTokens)

	// task 行已持久化 total_tokens。
	var reloaded Task
	require.NoError(t, DB.Select("total_tokens, token_quota_synced").Where("id = ?", task.ID).First(&reloaded).Error)
	assert.Equal(t, totalTokens, reloaded.TotalTokens)
	assert.Equal(t, totalTokens, reloaded.TokenQuotaSynced)
}

// 场景12（P1 日志兜底负例）：日志无 task_id 匹配 / 无 total_tokens / task_id 前缀
// 相同但实际不同时，不读取伪造数据，也不触发补录。
func TestReconcileTaskTokens_RecoverFromLogsSkipsNoToken(t *testing.T) {
	truncateTables(t)
	clearQuotaDataCache(t)
	enableReconcileExport(t)

	submitTime := time.Now().Unix()
	require.NoError(t, DB.Create(&User{Id: 7402, Username: "recover_log_7402", Quota: 100000, Status: common.UserStatusEnabled}).Error)

	// 任务无 task.Data，且其日志缺失 total_tokens（仅 task_id）。
	taskNoToken := &Task{
		TaskID:    "task_recover_log_7402_notoken",
		UserId:    7402,
		ChannelId: 7402,
		Group:     "default",
		Quota:     1000,
		Status:    TaskStatusSuccess,
		SubmitTime: submitTime,
		CreatedAt:  submitTime,
		Properties: Properties{OriginModelName: "doubao-seedance-2-0-260128"},
		PrivateData: TaskPrivateData{TokenId: 7402, NodeName: "node-a"},
	}
	require.NoError(t, DB.Create(taskNoToken).Error)
	// 日志仅含 task_id、无 total_tokens。
	RecordTaskBillingLog(RecordTaskBillingLogParams{
		UserId: 7402, LogType: LogTypeConsume, Content: "无 token 的历史结算",
		ChannelId: 7402, ModelName: "doubao-seedance-2-0-260128", Quota: 1000,
		TokenId: 7402, Group: "default",
		Other: map[string]interface{}{"task_id": taskNoToken.TaskID, "is_task": true},
	})

	// 另一任务：日志含 total_tokens 但 task_id 为"同前缀不同值"，不得误读。
	taskOther := &Task{
		TaskID:    "task_recover_log_7402_other_prefix",
		UserId:    7402,
		ChannelId: 7402,
		Group:     "default",
		Quota:     1000,
		Status:    TaskStatusSuccess,
		SubmitTime: submitTime,
		CreatedAt:  submitTime,
		Properties: Properties{OriginModelName: "doubao-seedance-2-0-260128"},
		PrivateData: TaskPrivateData{TokenId: 7402, NodeName: "node-a"},
	}
	require.NoError(t, DB.Create(taskOther).Error)
	// 该日志的 task_id 前缀与 taskNoToken 相同（"task_recover_log_7402_..."），
	// 但 total_tokens 归属于 other_prefix 任务，不得被 notoken 任务误读。
	RecordTaskBillingLog(RecordTaskBillingLogParams{
		UserId: 7402, LogType: LogTypeConsume, Content: "其他任务结算",
		ChannelId: 7402, ModelName: "doubao-seedance-2-0-260128", Quota: 1000,
		TokenId: 7402, Group: "default",
		Other:       map[string]interface{}{"task_id": taskOther.TaskID, "is_task": true},
		TotalTokens: 9999,
	})

	// 均不应被恢复：notoken 任务日志无 total_tokens；other_prefix 任务日志有
	// total_tokens 但其 task_id 与 notoken 不同，且该任务自身会被日志回填成 9999。
	require.Equal(t, 1, ReconcileTaskTokensToQuotaData())

	var reloaded Task
	require.NoError(t, DB.Select("total_tokens").Where("id = ?", taskNoToken.ID).First(&reloaded).Error)
	assert.Equal(t, 0, reloaded.TotalTokens, "无 total_tokens 的日志不得伪造 token")
}

// 场景10（P1 自愈负例）：task.Data 缺失或 usage.total_tokens<=0 时，不写入伪造数据，
// 也不触发补录。
func TestReconcileTaskTokens_RecoverSkipsNoTokenData(t *testing.T) {
	truncateTables(t)
	clearQuotaDataCache(t)
	enableReconcileExport(t)

	submitTime := time.Now().Unix()
	require.NoError(t, DB.Create(&User{Id: 7302, Username: "recover_7302", Quota: 100000, Status: common.UserStatusEnabled}).Error)

	// 无 Data 的历史任务
	taskNoData := &Task{
		TaskID:    "task_recover_7302_nodata",
		UserId:    7302,
		ChannelId: 7302,
		Group:     "default",
		Quota:     1000,
		Status:    TaskStatusSuccess,
		SubmitTime: submitTime,
		CreatedAt:  submitTime,
		Properties: Properties{OriginModelName: "doubao-seedance-2-0-260128"},
		PrivateData: TaskPrivateData{TokenId: 7302, NodeName: "node-a"},
	}
	require.NoError(t, DB.Create(taskNoData).Error)

	// Data 含 usage.total_tokens=0
	taskZero := &Task{
		TaskID:    "task_recover_7302_zero",
		UserId:    7302,
		ChannelId: 7302,
		Group:     "default",
		Quota:     1000,
		Status:    TaskStatusSuccess,
		SubmitTime: submitTime,
		CreatedAt:  submitTime,
		Data:       []byte(`{"usage":{"total_tokens":0}}`),
		Properties: Properties{OriginModelName: "doubao-seedance-2-0-260128"},
		PrivateData: TaskPrivateData{TokenId: 7302, NodeName: "node-a"},
	}
	require.NoError(t, DB.Create(taskZero).Error)

	// 均不应被恢复（无伪造数据）
	require.Equal(t, 0, ReconcileTaskTokensToQuotaData())
	rows, err := GetRankingQuotaTotals(submitTime-3600, submitTime+3600)
	require.NoError(t, err)
	require.Empty(t, rows)
}
