package model

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ===========================================================================
// 问题一 P0：有限 Token 负余额窗口的 Redis + BatchUpdateEnabled 回归测试
//
// 修复前缺陷链：Redis 已扣 Seedance 预扣但 DB 仍为旧余额（persistTokenQuotaDelta
// 在批量模式下只入队、扣减无守卫）→ ApplySeedanceSettle / RepayTaskBillingDebt
// 按旧 DB 余额批准差额 → syncTokenQuotaCacheAfterCommit 把缓存扣成负数 →
// batchUpdate 随后通过 increaseTokenQuota 无守卫应用预扣，DB 也变成负数。
//
// 修复后不变量（有限 Token）：授权扣减无论批量模式都同步直写带守卫
// `remain_quota >= quota`；任何路径都不得产生 remain_quota<0 或 used_quota<0；
// remain_quota + used_quota 恒定。
// ===========================================================================

var finiteTokenTaskSeq int64

// seedFiniteTokenTask 创建有限 Token 结算任务（不做额外预扣——预扣由
// TryReserveTokenQuota 在测试中显式执行，避免与 seedTokenSettleTask 的
// fixture 预扣重复）。
func seedFiniteTokenTask(t *testing.T, userID, tokenID int, preConsumed int64) *Task {
	t.Helper()
	seq := atomic.AddInt64(&finiteTokenTaskSeq, 1)
	task := &Task{
		TaskID:    fmt.Sprintf("task-finite-%d-%d", userID, seq),
		UserId:    userID,
		Quota:     preConsumed,
		Status:    TaskStatusInProgress,
		Group:     "default",
		Data:      []byte(`{}`),
		CreatedAt: time.Now().Unix(),
		UpdatedAt: time.Now().Unix(),
		PrivateData: TaskPrivateData{
			BillingSource:      "wallet",
			TokenId:            tokenID,
			ConsumeLogRecorded: true,
		},
	}
	require.NoError(t, DB.Create(task).Error)
	return task
}

// 开启批量模式（resetBatchUpdateTestState 会把开关置 false，此处显式开启）。
func enableBatchMode(t *testing.T) {
	t.Helper()
	resetBatchUpdateTestState(t)
	oldBatch := common.BatchUpdateEnabled
	common.BatchUpdateEnabled = true
	t.Cleanup(func() { common.BatchUpdateEnabled = oldBatch })
}

// 场景 1：有限 Token 余额只能支付预扣，随后 Seedance 正差额结算不得产生负余额。
// 预扣 400（批量模式下 DB 立即直写 1000→600）→ 结算补扣 500（守卫 600>=500）→
// remain=100、used=900，remain+used 恒定，绝不为负。
func TestFiniteToken_ReserveThenPositiveSettleNoNegative(t *testing.T) {
	truncateTables(t)
	enableBatchMode(t)
	useQuotaCacheMiniRedis(t)

	user := createReserveTestUser(t, 5000)
	tok := createReserveTestToken(t, 1000)
	require.NoError(t, DB.Model(&Token{}).Where("id = ?", tok.Id).Update("user_id", user.Id).Error)
	seedUsedQuotaForUser(t, user.Id, 400) // 预扣 400 对应的累计消耗

	reserved, err := TryReserveTokenQuota(tok.Id, tok.Key, 400, false)
	require.NoError(t, err)
	require.True(t, reserved)
	assert.Equal(t, int64(600), getTokenFromDB(t, tok.Id).RemainQuota, "预扣后 DB 必须立即落账（批量模式也直写带守卫）")
	assert.Equal(t, int64(400), getTokenFromDB(t, tok.Id).UsedQuota)

	task := seedFiniteTokenTask(t, user.Id, tok.Id, 400)
	res, tokenRes := ApplySeedanceSettle(task, 500, false, TaskQuotaDeltaOptions{GuardPositiveDelta: true})
	require.Equal(t, TaskQuotaDeltaSuccess, res)
	require.Equal(t, TokenAdjustOK, tokenRes)

	after := getTokenFromDB(t, tok.Id)
	assert.Equal(t, int64(100), after.RemainQuota, "结算补扣基于已直写的余额")
	assert.Equal(t, int64(900), after.UsedQuota)
	assert.GreaterOrEqual(t, int64(after.RemainQuota), int64(0), "有限 Token 余额绝不为负")
	assert.GreaterOrEqual(t, int64(after.UsedQuota), int64(0), "有限 Token used 绝不为负")
	assert.Equal(t, int64(1000), after.RemainQuota+after.UsedQuota, "remain+used 恒定")
	assert.Equal(t, 0, tokenPending(t, task.ID), "余额充足时不得进入 pending")
}

// 场景 2：预扣尚未批量刷新时执行 Seedance 退款，最终 remain/used 正确。
// 预扣 400（DB 600）→ 退款 300 → remain=900、used=100，remain+used 恒定，
// used 绝不为负。
func TestFiniteToken_BatchModeRefundBeforeFlush(t *testing.T) {
	truncateTables(t)
	enableBatchMode(t)
	useQuotaCacheMiniRedis(t)

	user := createReserveTestUser(t, 5000)
	tok := createReserveTestToken(t, 1000)
	require.NoError(t, DB.Model(&Token{}).Where("id = ?", tok.Id).Update("user_id", user.Id).Error)
	seedUsedQuotaForUser(t, user.Id, 400)

	reserved, err := TryReserveTokenQuota(tok.Id, tok.Key, 400, false)
	require.NoError(t, err)
	require.True(t, reserved)
	assert.Equal(t, int64(600), getTokenFromDB(t, tok.Id).RemainQuota)

	task := seedFiniteTokenTask(t, user.Id, tok.Id, 400)
	res, tokenRes := ApplySeedanceSettle(task, -300, false, TaskQuotaDeltaOptions{})
	require.Equal(t, TaskQuotaDeltaSuccess, res)
	require.Equal(t, TokenAdjustOK, tokenRes)

	after := getTokenFromDB(t, tok.Id)
	assert.Equal(t, int64(900), after.RemainQuota, "退款后 remain 加回 abs(delta)")
	assert.Equal(t, int64(100), after.UsedQuota, "退款后 used 冲减 abs(delta)")
	assert.GreaterOrEqual(t, int64(after.UsedQuota), int64(0), "used 绝不为负")
	assert.Equal(t, int64(1000), after.RemainQuota+after.UsedQuota, "remain+used 恒定")
	assert.Equal(t, 0, tokenPending(t, task.ID))
}

// 场景 3：预扣尚未刷新时执行欠款清偿，不能基于旧余额错误批准。
// 预扣 400（DB 600）→ 欠款 500 → 清偿扣 500 → remain=100、used=900。
// 旧实现 DB 仍是 1000（预扣未落账）会错误批准成 remain=500（少扣 400）。
func TestFiniteToken_BatchModeDebtRepayNotApprovedOnStaleBalance(t *testing.T) {
	truncateTables(t)
	enableBatchMode(t)
	useQuotaCacheMiniRedis(t)

	user := createReserveTestUser(t, 2000)
	tok := createReserveTestToken(t, 1000)
	require.NoError(t, DB.Model(&Token{}).Where("id = ?", tok.Id).Update("user_id", user.Id).Error)
	seedUsedQuotaForUser(t, user.Id, 400)
	task := seedFiniteTokenTask(t, user.Id, tok.Id, 400)

	reserved, err := TryReserveTokenQuota(tok.Id, tok.Key, 400, false)
	require.NoError(t, err)
	require.True(t, reserved)
	assert.Equal(t, int64(600), getTokenFromDB(t, tok.Id).RemainQuota, "预扣必须已直写落账（否则清偿基于旧余额错误批准）")

	_, _, _, err = CreateDebtAndFreeze(DebtInput{
		UserId: user.Id, TaskId: task.TaskID, PreConsumedQuota: 400, ActualQuota: 900, DeltaQuota: 500,
		TokenId: tok.Id, BillingSource: "wallet",
	})
	require.NoError(t, err)
	debt, err := GetTaskBillingDebtByTaskId(task.TaskID)
	require.NoError(t, err)

	require.NoError(t, RepayTaskBillingDebt(user.Id, debt.ID, RepayDebtOptions{}, 0))
	after := getTokenFromDB(t, tok.Id)
	assert.Equal(t, int64(100), after.RemainQuota, "清偿基于已直写的余额扣减（旧余额 1000 会错误批准成 500）")
	assert.Equal(t, int64(900), after.UsedQuota)
	assert.GreaterOrEqual(t, int64(after.RemainQuota), int64(0), "有限 Token 余额绝不为负")
	assert.Equal(t, int64(1000), after.RemainQuota+after.UsedQuota, "remain+used 恒定")
	assert.Equal(t, int64(2000-500), getUserQuotaFromDB(t, user.Id), "钱包收款差额")
}

// 场景 4：Redis 缓存余额比数据库余额偏高时（补扣已提交但缓存未同步），
// 数据库守卫拒绝并补偿缓存，绝不按旧缓存把 DB 扣成负数。
func TestFiniteToken_DBGuardRejectsStaleCacheAndCompensates(t *testing.T) {
	truncateTables(t)
	enableBatchMode(t)
	useQuotaCacheMiniRedis(t)

	user := createReserveTestUser(t, 100)
	tok := createReserveTestToken(t, 50)
	require.NoError(t, DB.Model(&Token{}).Where("id = ?", tok.Id).Update("user_id", user.Id).Error)

	// 水合缓存（DB 50）后直接把缓存余额字段改成 100，模拟"补扣已提交但缓存
	// 未同步"（used 保持 0，避免正增量把 used 冲成负数）。
	_, err := GetTokenByKey(tok.Key, true)
	require.NoError(t, err)
	require.NoError(t, common.RDB.HSet(context.Background(), getTokenCacheKey(tok.Key), "RemainQuota", "100").Err())

	reserved, err := TryReserveTokenQuota(tok.Id, tok.Key, 60, false)
	require.NoError(t, err, "守卫拒绝应视为额度不足而非数据库错误")
	assert.False(t, reserved, "缓存按旧值授权但 DB 守卫必须拒绝")

	assert.Equal(t, int64(50), getTokenFromDB(t, tok.Id).RemainQuota, "DB 余额不得被扣成负数")
	cached, err := cacheGetTokenByKey(tok.Key)
	require.NoError(t, err)
	assert.Equal(t, int64(100), cached.RemainQuota, "授权失败后缓存必须补偿回原值")
	assert.Zero(t, cached.UsedQuota)
}

// 场景 5：批量刷新前后始终满足有限 Token 不变量：
// remain_quota >= 0、used_quota >= 0、remain_quota + used_quota 恒定；
// 且有限 Token 扣减绝不进入无守卫批量队列。
func TestFiniteToken_InvariantAcrossBatchFlush(t *testing.T) {
	truncateTables(t)
	enableBatchMode(t)
	useQuotaCacheMiniRedis(t)

	user := createReserveTestUser(t, 5000)
	tok := createReserveTestToken(t, 1000)
	require.NoError(t, DB.Model(&Token{}).Where("id = ?", tok.Id).Update("user_id", user.Id).Error)

	for _, q := range []int64{200, 150, 250} {
		reserved, err := TryReserveTokenQuota(tok.Id, tok.Key, q, false)
		require.NoError(t, err)
		require.True(t, reserved)
	}

	mid := getTokenFromDB(t, tok.Id)
	assert.Equal(t, int64(400), mid.RemainQuota)
	assert.Equal(t, int64(600), mid.UsedQuota)
	assert.GreaterOrEqual(t, int64(mid.RemainQuota), int64(0))
	assert.GreaterOrEqual(t, int64(mid.UsedQuota), int64(0))
	assert.Equal(t, int64(1000), mid.RemainQuota+mid.UsedQuota, "批量刷新前 remain+used 恒定")

	// 有限 Token 扣减不得进入无守卫批量队列（批量队列只承载加方向增量）
	batchUpdateLocks[BatchUpdateTypeTokenQuota].Lock()
	_, enqueued := batchUpdateStores[BatchUpdateTypeTokenQuota][tok.Id]
	batchUpdateLocks[BatchUpdateTypeTokenQuota].Unlock()
	assert.False(t, enqueued, "有限 Token 扣减绝不入批量队列")

	batchUpdate()
	after := getTokenFromDB(t, tok.Id)
	assert.Equal(t, int64(400), after.RemainQuota, "批量刷新不改变已直写的余额")
	assert.Equal(t, int64(600), after.UsedQuota)
	assert.GreaterOrEqual(t, int64(after.RemainQuota), int64(0))
	assert.GreaterOrEqual(t, int64(after.UsedQuota), int64(0))
	assert.Equal(t, int64(1000), after.RemainQuota+after.UsedQuota, "批量刷新后 remain+used 恒定")

	// 加方向（退款/充值）仍走批量队列（不影响守卫路径）
	require.NoError(t, persistTokenQuotaDelta(tok.Id, +100))
	batchUpdateLocks[BatchUpdateTypeTokenQuota].Lock()
	enqueuedVal, ok := batchUpdateStores[BatchUpdateTypeTokenQuota][tok.Id]
	batchUpdateLocks[BatchUpdateTypeTokenQuota].Unlock()
	assert.True(t, ok, "加方向应进入批量队列")
	assert.Equal(t, int64(100), enqueuedVal)
}
