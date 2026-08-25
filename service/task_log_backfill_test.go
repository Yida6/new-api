package service

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/types"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// 提交消费日志 task_id 关联 + 结算后 total_tokens 回填（方案 A）
// 背景：异步任务（Seedance 等）提交瞬间上游无 usage，提交日志的 Tokens 列
// 只能等结算完成后由 model.BackfillTaskConsumeLogTotalTokens 按 other.task_id
// 回填 other.total_tokens。
// ---------------------------------------------------------------------------

// findTaskSubmitLog 按 user + 消费类型 + other 中的 is_task 标记与 task_id 值
// 定位"提交消费日志"（与结算/退款调整日志区分：后者 other 无 is_task）。
// 注意不用 `%"task_id":"<id>%"` 整键 pattern：glebarez/sqlite 对 LIKE 末尾
// `%"` 序列匹配异常，拆成 `%"is_task":true%` 与 `%<taskID>%` 两个独立 pattern。
func findTaskSubmitLog(t *testing.T, userID int, taskID string) *model.Log {
	t.Helper()
	pattern := "%" + taskID + "%"
	var logRow model.Log
	err := model.LOG_DB.Where("user_id = ? AND type = ? AND other LIKE ? AND other LIKE ?",
		userID, model.LogTypeConsume, `%"is_task":true%`, pattern).
		Order("id desc").Limit(1).First(&logRow).Error
	require.NoError(t, err)
	return &logRow
}

// 提交日志必须携带 other.task_id（与 task.TaskID 一致），否则结算回填匹配不到。
func TestLogTaskConsumption_WritesTaskID(t *testing.T) {
	truncate(t)
	const userID, channelID, tokenID = 7001, 7001, 7001
	seedUser(t, userID, 100000)
	seedToken(t, tokenID, userID, "sk-taskid-log", 100000)
	seedChannel(t, channelID)
	seedUsedQuota(t, userID, channelID, 1000)

	c := testGinContext("test_user")
	c.Request = httptest.NewRequest("POST", "/v1/videos/generations", nil)
	info := &relaycommon.RelayInfo{
		UserId:          userID,
		UsingGroup:      "default",
		OriginModelName: "doubao-seedance-2-0-260128",
		PriceData: types.PriceData{
			ModelPrice: 0.02,
			ModelRatio: 3.15,
			GroupRatioInfo: types.GroupRatioInfo{
				GroupRatio: 1.0,
			},
		},
		ChannelMeta: &relaycommon.ChannelMeta{
			ChannelId: channelID,
		},
		TaskRelayInfo: &relaycommon.TaskRelayInfo{
			PublicTaskID: "task_log_abc",
			Action:       "task_submit",
		},
	}
	recorded, err := LogTaskConsumption(c, info)
	require.NoError(t, err)
	require.True(t, recorded)

	last := getLastLog(t)
	require.NotNil(t, last)
	assert.Equal(t, model.LogTypeConsume, last.Type)
	var other map[string]interface{}
	require.NoError(t, json.Unmarshal([]byte(last.Other), &other))
	assert.Equal(t, "task_log_abc", other["task_id"], "提交日志必须携带与 task.TaskID 一致的 task_id")
	assert.Equal(t, true, other["is_task"], "任务类日志标记 is_task")
}

// model.BackfillTaskConsumeLogTotalTokens 的基础行为：回填 / 幂等 / 不匹配 / 非法参数。
func TestBackfillTaskConsumeLogTotalTokens(t *testing.T) {
	truncate(t)
	const userID = 7002
	seedUser(t, userID, 100000)
	c := testGinContext("test_user")

	// 造一条提交消费日志（other 含 task_id，即生产路径 LogTaskConsumption 的行为）
	recorded := model.RecordConsumeLog(c, userID, model.RecordConsumeLogParams{
		ChannelId: 1,
		ModelName: "doubao-seedance-2-0-260128",
		TokenName: "test_token",
		Quota:     1000,
		Content:   "操作 task_submit",
		TokenId:   0,
		Group:     "default",
		Other:     map[string]interface{}{"is_task": true, "task_id": "task_backfill_1"},
	})
	require.True(t, recorded)

	// 回填
	assert.True(t, model.BackfillTaskConsumeLogTotalTokens(userID, "task_backfill_1", 40594))
	other := getLastLogOtherMap(t)
	assert.Equal(t, float64(40594), other["total_tokens"], "提交日志应回显结算后的总 token")
	assert.Equal(t, "task_backfill_1", other["task_id"], "回填不得丢失原有字段")

	// 幂等：同值重复回填仍成功且不产生新日志
	logCount := countLogs(t)
	assert.True(t, model.BackfillTaskConsumeLogTotalTokens(userID, "task_backfill_1", 40594))
	assert.Equal(t, logCount, countLogs(t), "幂等回填不得新增日志")

	// 不匹配的 task_id → false（不误改其他日志）
	assert.False(t, model.BackfillTaskConsumeLogTotalTokens(userID, "task_nonexistent", 40594))

	// 非法参数 → false
	assert.False(t, model.BackfillTaskConsumeLogTotalTokens(userID, "task_backfill_1", 0))
	assert.False(t, model.BackfillTaskConsumeLogTotalTokens(userID, "", 40594))
}

// Seedance 结算（退款方向，delta<0）后，提交消费日志被回填 total_tokens。
func TestSeedanceSettle_BackfillsSubmitLogTotalTokens(t *testing.T) {
	truncate(t)
	ctx := context.Background()
	const userID, tokenID, channelID = 7003, 7003, 7003
	const preConsumed, totalTokens = 12600, 3000
	seedTaskLogRatioEnv(t, "doubao-seedance-2-0-260128", 3.15)
	seedSeedanceUser(t, userID, 100000, common.RoleCommonUser)
	seedToken(t, tokenID, userID, "sk-seed-backfill", 100000)
	seedChannel(t, channelID)
	seedUsedQuota(t, userID, channelID, preConsumed)

	task := seedSeedanceTask(t, userID, channelID, tokenID, preConsumed)

	// 模拟提交时的消费日志（other 含 task_id，生产路径由 LogTaskConsumption 写入）
	c := testGinContext("test_user")
	require.True(t, model.RecordConsumeLog(c, userID, model.RecordConsumeLogParams{
		ChannelId: channelID,
		ModelName: "doubao-seedance-2-0-260128",
		TokenName: "test_token",
		Quota:     preConsumed,
		Content:   "操作 task_submit",
		TokenId:   tokenID,
		Group:     "default",
		Other:     map[string]interface{}{"is_task": true, "task_id": task.TaskID},
	}))

	// adaptor 不调整（adjustReturn=0）→ 走 taskResult.TotalTokens 重算分支
	adaptor := &mockAdaptor{}
	outcome := SettleSeedanceTaskBilling(ctx, adaptor, task, &relaycommon.TaskInfo{
		Status:      model.TaskStatusSuccess,
		TotalTokens: totalTokens,
	})
	require.Equal(t, SeedanceSettleSuccess, outcome)

	submitLog := findTaskSubmitLog(t, userID, task.TaskID)
	require.NotNil(t, submitLog)
	var other map[string]interface{}
	require.NoError(t, json.Unmarshal([]byte(submitLog.Other), &other))
	assert.Equal(t, float64(totalTokens), other["total_tokens"], "提交日志应在 Seedance 结算后回填 total_tokens")
}

// 通用 token 重算（RecalculateTaskQuotaByTokens）同样回填提交日志。
func TestRecalculateTaskQuota_BackfillsSubmitLogTotalTokens(t *testing.T) {
	truncate(t)
	ctx := context.Background()
	const userID, tokenID, channelID = 7004, 7004, 7004
	const preConsumed, totalTokens = 1000, 2000
	seedTaskLogRatioEnv(t, "test-model", 2.0)
	seedUser(t, userID, 100000)
	seedToken(t, tokenID, userID, "sk-recalc-backfill", 100000)
	seedChannel(t, channelID)
	seedUsedQuota(t, userID, channelID, preConsumed)

	task := makeTask(userID, channelID, preConsumed, tokenID, BillingSourceWallet, 0)
	require.NoError(t, model.DB.Create(task).Error)

	c := testGinContext("test_user")
	require.True(t, model.RecordConsumeLog(c, userID, model.RecordConsumeLogParams{
		ChannelId: channelID,
		ModelName: "test-model",
		TokenName: "test_token",
		Quota:     preConsumed,
		Content:   "操作 task_submit",
		TokenId:   tokenID,
		Group:     "default",
		Other:     map[string]interface{}{"is_task": true, "task_id": task.TaskID},
	}))

	assert.True(t, RecalculateTaskQuotaByTokens(ctx, task, totalTokens))

	submitLog := findTaskSubmitLog(t, userID, task.TaskID)
	require.NotNil(t, submitLog)
	var other map[string]interface{}
	require.NoError(t, json.Unmarshal([]byte(submitLog.Other), &other))
	assert.Equal(t, float64(totalTokens), other["total_tokens"], "提交日志应在 token 重算后回填 total_tokens")
}
