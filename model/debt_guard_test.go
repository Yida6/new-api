package model

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/QuantumNous/new-api/common"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ===========================================================================
// ApplyTaskQuotaDeltaGuarded — Seedance 差额补扣的原子余额守卫
// 核心设计：单条条件 SQL `UPDATE users SET quota = quota - ? WHERE id = ? AND
// quota >= ?` + RowsAffected==1 校验（MySQL/PostgreSQL 下原子；SQLite 单连接
// 串行化，语义等价）。数据库守卫是最终资金边界，Redis 缓存只优化授权。
// ===========================================================================

func seedGuardTask(t *testing.T, taskID string, userID int, quota int64, preConsumed int64) *Task {
	t.Helper()
	u := &User{Id: userID, Username: fmt.Sprintf("guard_%d", userID), Quota: quota, Status: common.UserStatusEnabled, Role: common.RoleCommonUser, AffCode: fmt.Sprintf("aff-guard-%d", userID)}
	require.NoError(t, DB.Create(u).Error)
	task := &Task{
		TaskID:    taskID,
		UserId:    userID,
		Quota:     preConsumed,
		Status:    TaskStatusInProgress,
		Group:     "default",
		Data:      []byte(`{}`),
		CreatedAt: time.Now().Unix(),
		UpdatedAt: time.Now().Unix(),
		PrivateData: TaskPrivateData{
			BillingSource:  "wallet",
			ConsumeLogRecorded: true,
		},
	}
	require.NoError(t, DB.Create(task).Error)
	return task
}

func TestGuardedSettle_ExactBalance(t *testing.T) {
	truncateTables(t)
	// 余额恰好够差额：扣减后余额为 0
	const userID, quota, preConsumed, actual = 1001, 500, 1000, 1500
	task := seedGuardTask(t, "task-guard-exact", userID, quota, preConsumed)

	res := ApplyTaskQuotaDeltaGuarded(task, actual-preConsumed, false, TaskQuotaDeltaOptions{GuardPositiveDelta: true})
	require.Equal(t, TaskQuotaDeltaSuccess, res)
	assert.Equal(t, int64(0), getUserQuotaDB(t, userID), "余额恰好够差额时结算成功且余额为 0")
	assert.Equal(t, int64(actual), task.Quota, "任务额度更新为实际额度")
}

func TestGuardedSettle_ShortByOneRejected(t *testing.T) {
	truncateTables(t)
	// 余额少 1：拒绝补扣，余额不变且不为负
	const userID, quota, preConsumed, actual = 1002, 499, 1000, 1500
	task := seedGuardTask(t, "task-guard-short", userID, quota, preConsumed)

	res := ApplyTaskQuotaDeltaGuarded(task, actual-preConsumed, false, TaskQuotaDeltaOptions{GuardPositiveDelta: true})
	require.Equal(t, TaskQuotaDeltaInsufficientBalance, res, "余额不足必须返回可识别的 InsufficientBalance")
	assert.Equal(t, int64(quota), getUserQuotaDB(t, userID), "拒绝补扣时余额不变")
	assert.GreaterOrEqual(t, getUserQuotaDB(t, userID), int64(0), "余额永不为负数")
	assert.Equal(t, int64(preConsumed), task.Quota, "守卫失败保留原额度（内存不被污染）")
}

func TestGuardedSettle_UserNotFound(t *testing.T) {
	truncateTables(t)
	// 用户不存在：返回 UserNotFound（与余额不足可区分），不扣任何东西
	task := &Task{
		TaskID: "task-guard-nouser", UserId: 999999, Quota: 1000, Status: TaskStatusInProgress,
		Data: []byte(`{}`), CreatedAt: time.Now().Unix(), UpdatedAt: time.Now().Unix(),
	}
	require.NoError(t, DB.Create(task).Error)

	res := ApplyTaskQuotaDeltaGuarded(task, 500, false, TaskQuotaDeltaOptions{GuardPositiveDelta: true})
	require.Equal(t, TaskQuotaDeltaUserNotFound, res)
	assert.Equal(t, int64(1000), task.Quota)
}

func TestGuardedSettle_RefundUnaffected(t *testing.T) {
	truncateTables(t)
	// 退款方向（delta<0）不受守卫影响，照常加回
	const userID, quota, preConsumed, actual = 1003, 1000, 1500, 500
	task := seedGuardTask(t, "task-guard-refund", userID, quota, preConsumed)
	seedUsedQuotaForUser(t, userID, preConsumed)

	res := ApplyTaskQuotaDeltaGuarded(task, actual-preConsumed, false, TaskQuotaDeltaOptions{GuardPositiveDelta: true})
	require.Equal(t, TaskQuotaDeltaSuccess, res)
	assert.Equal(t, int64(quota+(preConsumed-actual)), getUserQuotaDB(t, userID), "多预扣部分退款")
}

func TestGuardedSettle_SubscriptionExceeded(t *testing.T) {
	truncateTables(t)
	// 订阅超额：返回 SubscriptionExceeded（现有 AmountTotal 上限守卫）
	const userID, subID, preConsumed, actual = 1004, 1, 1000, 1500
	u := &User{Id: userID, Username: "guard_sub", Quota: 0, Status: common.UserStatusEnabled, Role: common.RoleCommonUser, AffCode: "aff-guard-sub"}
	require.NoError(t, DB.Create(u).Error)
	sub := &UserSubscription{
		Id: subID, UserId: userID, AmountTotal: 1200, AmountUsed: 1000,
		Status: "active", StartTime: time.Now().Unix(), EndTime: time.Now().Add(24 * time.Hour).Unix(),
	}
	require.NoError(t, DB.Create(sub).Error)
	task := &Task{
		TaskID: "task-guard-sub", UserId: userID, Quota: preConsumed, Status: TaskStatusInProgress,
		Data: []byte(`{}`), CreatedAt: time.Now().Unix(), UpdatedAt: time.Now().Unix(),
		PrivateData: TaskPrivateData{SubscriptionId: subID, BillingSource: "subscription"},
	}
	require.NoError(t, DB.Create(task).Error)

	res := ApplyTaskQuotaDeltaGuarded(task, actual-preConsumed, true, TaskQuotaDeltaOptions{GuardPositiveDelta: true})
	require.Equal(t, TaskQuotaDeltaSubscriptionExceeded, res)
	var reloaded UserSubscription
	require.NoError(t, DB.First(&reloaded, subID).Error)
	assert.Equal(t, int64(1000), reloaded.AmountUsed, "订阅超限时已用量不变")
}

func TestGuardedSettle_DefaultUnconditionalKeepsCompat(t *testing.T) {
	truncateTables(t)
	// 默认（GuardPositiveDelta=false）保持通用"允许欠费"语义：无条件扣减，
	// 余额可扣成负数（分层计费/组升级路径的兼容行为，Seedance 不经过此路径）
	const userID, quota, preConsumed, actual = 1005, 100, 1000, 5000
	task := seedGuardTask(t, "task-guard-compat", userID, quota, preConsumed)

	res := ApplyTaskQuotaDeltaGuarded(task, actual-preConsumed, false, TaskQuotaDeltaOptions{})
	require.Equal(t, TaskQuotaDeltaSuccess, res)
	assert.Equal(t, int64(quota-(actual-preConsumed)), getUserQuotaDB(t, userID), "默认语义无条件扣减（兼容分层计费）")
}

// ===========================================================================
// 并发回归：同一用户多个任务并发结算，任意交错下余额 >= 0 且不超扣
// ===========================================================================

func TestGuardedSettle_ConcurrentNeverNegative(t *testing.T) {
	truncateTables(t)
	const userID, balance = 2001, 3000
	u := &User{Id: userID, Username: "guard_conc", Quota: balance, Status: common.UserStatusEnabled, Role: common.RoleCommonUser, AffCode: "aff-guard-conc"}
	require.NoError(t, DB.Create(u).Error)

	// 5 个任务，每个预扣 1000，实际 1800（差额 800）；余额 3000 只够 3 个差额
	const numTasks = 5
	const preConsumed, delta = 1000, 800
	tasks := make([]*Task, numTasks)
	for i := 0; i < numTasks; i++ {
		task := &Task{
			TaskID: fmt.Sprintf("task-conc-%d", i), UserId: userID, Quota: preConsumed, Status: TaskStatusInProgress,
			Data: []byte(`{}`), CreatedAt: time.Now().Unix(), UpdatedAt: time.Now().Unix(),
		}
		require.NoError(t, DB.Create(task).Error)
		tasks[i] = task
	}

	done := make(chan struct{})
	timeout := time.After(60 * time.Second)
	var wg sync.WaitGroup
	for i := 0; i < numTasks; i++ {
		wg.Add(1)
		go func(task *Task) {
			defer wg.Done()
			ApplyTaskQuotaDeltaGuarded(task, delta, false, TaskQuotaDeltaOptions{GuardPositiveDelta: true})
		}(tasks[i])
	}
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-timeout:
		t.Fatal("并发结算超时")
	}

	quota := getUserQuotaDB(t, userID)
	assert.GreaterOrEqual(t, int64(quota), int64(0), "并发结算任意交错下余额必须 >= 0")
	assert.Equal(t, int64(balance-3*delta), quota, "成功收款总额 = 3 个差额（3000 余额最多收 3 个 800）")

	// 被拒绝的两个任务额度不变（未污染）
	rejected := 0
	for _, task := range tasks {
		var reloaded Task
		require.NoError(t, DB.First(&reloaded, task.ID).Error)
		if reloaded.Quota == preConsumed {
			rejected++
		} else {
			assert.Equal(t, int64(preConsumed+delta), reloaded.Quota, "成功任务额度更新到实际")
		}
	}
	assert.Equal(t, 2, rejected, "余额不足的任务必须被守卫拒绝并保持原额度")
}

func TestGuardedSettle_ConcurrentTopupAndSettle(t *testing.T) {
	truncateTables(t)
	const userID, initial = 2002, 1000
	u := &User{Id: userID, Username: "guard_topup", Quota: initial, Status: common.UserStatusEnabled, Role: common.RoleCommonUser, AffCode: "aff-guard-topup"}
	require.NoError(t, DB.Create(u).Error)

	const numOps = 20
	const delta, topup = 700, 1000
	tasks := make([]*Task, numOps)
	for i := 0; i < numOps; i++ {
		task := &Task{
			TaskID: fmt.Sprintf("task-topup-%d", i), UserId: userID, Quota: 1000, Status: TaskStatusInProgress,
			Data: []byte(`{}`), CreatedAt: time.Now().Unix(), UpdatedAt: time.Now().Unix(),
		}
		require.NoError(t, DB.Create(task).Error)
		tasks[i] = task
	}

	done := make(chan struct{})
	timeout := time.After(60 * time.Second)
	var wg sync.WaitGroup
	for i := 0; i < numOps; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if i%2 == 0 {
				// 充值：IncreaseUserQuota 无条件加回
				_ = IncreaseUserQuota(userID, topup, true)
			} else {
				ApplyTaskQuotaDeltaGuarded(tasks[i], delta, false, TaskQuotaDeltaOptions{GuardPositiveDelta: true})
			}
		}(i)
	}
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-timeout:
		t.Fatal("并发充值与结算超时")
	}

	quota := getUserQuotaDB(t, userID)
	assert.GreaterOrEqual(t, int64(quota), int64(0), "并发充值+结算任意交错下余额必须 >= 0")
	// 充值总额 = numOps/2 * topup = 10 * 1000；最多收款数受余额约束，但绝不能超扣
	maxCollect := (initial + numOps/2*topup) / delta
	assert.LessOrEqual(t, int64(quota), int64(initial+numOps/2*topup), "余额不超总入账")
	_ = maxCollect
}

// ===========================================================================
// 欠款/冻结/清偿闭环
// ===========================================================================

func TestCreateDebtAndFreeze_RecordsDebtAndFreezesUser(t *testing.T) {
	truncateTables(t)
	const userID = 3001
	u := &User{Id: userID, Username: "debt_u", Quota: 100, Status: common.UserStatusEnabled, Role: common.RoleCommonUser, AffCode: "aff-debt-u"}
	require.NoError(t, DB.Create(u).Error)

	created, frozen, isAdmin, err := CreateDebtAndFreeze(DebtInput{
		UserId: userID, TaskId: "task-debt-1", UpstreamTaskId: "cgt-1", ModelName: "doubao-seedance-2-0-260128",
		ChannelId: 1, PreConsumedQuota: 1000, ActualQuota: 1800, DeltaQuota: 800, Reason: "余额不足",
	})
	require.NoError(t, err)
	assert.True(t, created)
	assert.True(t, frozen, "普通用户欠款必须冻结")
	assert.False(t, isAdmin)

	var user User
	require.NoError(t, DB.First(&user, userID).Error)
	assert.True(t, user.DebtFrozen, "欠款冻结标记必须落库")
	assert.Equal(t, common.UserStatusEnabled, user.Status, "欠款冻结不得改变管理员手工禁用状态")

	debt, err := GetTaskBillingDebtByTaskId("task-debt-1")
	require.NoError(t, err)
	assert.Equal(t, DebtStatusPending, debt.Status)
	assert.Equal(t, int64(800), debt.DeltaQuota)
	assert.Equal(t, int64(1000), debt.PreConsumedQuota)
	assert.Equal(t, int64(1800), debt.ActualQuota)
	assert.False(t, debt.AlertSent, "告警发送前保持 false（可重试）")
}

func TestCreateDebtAndFreeze_IdempotentPerTask(t *testing.T) {
	truncateTables(t)
	const userID = 3002
	u := &User{Id: userID, Username: "debt_idem", Quota: 100, Status: common.UserStatusEnabled, Role: common.RoleCommonUser, AffCode: "aff-debt-idem"}
	require.NoError(t, DB.Create(u).Error)

	input := DebtInput{UserId: userID, TaskId: "task-debt-idem", PreConsumedQuota: 1000, ActualQuota: 2000, DeltaQuota: 1000}
	created, _, _, err := CreateDebtAndFreeze(input)
	require.NoError(t, err)
	assert.True(t, created)

	// 重复结算/重复恢复/重复轮询：同一任务只能一笔欠款，不重复冻结
	created2, frozen2, _, err := CreateDebtAndFreeze(input)
	require.NoError(t, err)
	assert.False(t, created2, "同一任务重复创建欠款必须幂等 no-op")
	assert.False(t, frozen2, "重复创建不得重复冻结")

	var count int64
	DB.Model(&TaskBillingDebt{}).Where("task_id = ?", "task-debt-idem").Count(&count)
	assert.EqualValues(t, 1, count, "同一任务只能生成一笔欠款")
}

func TestCreateDebtAndFreeze_SkipsAdmin(t *testing.T) {
	truncateTables(t)
	const adminID = 3003
	u := &User{Id: adminID, Username: "debt_admin", Quota: 100, Status: common.UserStatusEnabled, Role: common.RoleAdminUser, AffCode: "aff-debt-admin"}
	require.NoError(t, DB.Create(u).Error)

	created, frozen, isAdmin, err := CreateDebtAndFreeze(DebtInput{
		UserId: adminID, TaskId: "task-debt-admin", PreConsumedQuota: 1000, ActualQuota: 2500, DeltaQuota: 1500,
	})
	require.NoError(t, err)
	assert.True(t, created, "管理员欠款记录必须照建")
	assert.False(t, frozen, "管理员不得被欠款冻结（避免误冻结运维账号）")
	assert.True(t, isAdmin, "必须标记管理员身份供最高级别告警")

	var user User
	require.NoError(t, DB.First(&user, adminID).Error)
	assert.False(t, user.DebtFrozen)
}

func TestRepayTaskBillingDebt_CollectsAndUnfreezes(t *testing.T) {
	truncateTables(t)
	const userID = 3004
	u := &User{Id: userID, Username: "debt_repay", Quota: 2000, Status: common.UserStatusEnabled, Role: common.RoleCommonUser, AffCode: "aff-debt-repay"}
	require.NoError(t, DB.Create(u).Error)
	seedDebtTask(t, userID, "task-debt-repay", 1000)

	_, _, _, err := CreateDebtAndFreeze(DebtInput{UserId: userID, TaskId: "task-debt-repay", PreConsumedQuota: 1000, ActualQuota: 1800, DeltaQuota: 800})
	require.NoError(t, err)
	debt, err := GetTaskBillingDebtByTaskId("task-debt-repay")
	require.NoError(t, err)

	// 清偿（余额 2000 够 800）
	require.NoError(t, RepayTaskBillingDebt(userID, debt.ID, RepayDebtOptions{}, 0))
	assert.Equal(t, int64(2000-800), getUserQuotaDB(t, userID), "收款后余额扣减差额")

	var user User
	require.NoError(t, DB.First(&user, userID).Error)
	assert.False(t, user.DebtFrozen, "全部欠款清偿后解除欠款冻结")

	var reloaded TaskBillingDebt
	require.NoError(t, DB.First(&reloaded, debt.ID).Error)
	assert.Equal(t, DebtStatusPaid, reloaded.Status)
	assert.NotZero(t, reloaded.CollectedAt, "收款时间必须记录")
	assert.NotZero(t, reloaded.ReleasedAt, "解除时间必须记录")
}

func TestRepayTaskBillingDebt_GuardAndIdempotency(t *testing.T) {
	truncateTables(t)
	const userID = 3005
	u := &User{Id: userID, Username: "debt_repay2", Quota: 500, Status: common.UserStatusEnabled, Role: common.RoleCommonUser, AffCode: "aff-debt-repay2"}
	require.NoError(t, DB.Create(u).Error)
	seedDebtTask(t, userID, "task-debt-repay2", 1000)

	_, _, _, err := CreateDebtAndFreeze(DebtInput{UserId: userID, TaskId: "task-debt-repay2", PreConsumedQuota: 1000, ActualQuota: 2000, DeltaQuota: 1000})
	require.NoError(t, err)
	debt, err := GetTaskBillingDebtByTaskId("task-debt-repay2")
	require.NoError(t, err)

	// 余额 500 < 差额 1000：清偿被守卫拒绝，余额不变
	err = RepayTaskBillingDebt(userID, debt.ID, RepayDebtOptions{}, 0)
	require.ErrorIs(t, err, ErrDebtInsufficientBalance, "余额不足必须返回可识别的 InsufficientBalance")
	assert.Equal(t, int64(500), getUserQuotaDB(t, userID), "守卫拒绝时余额不变")

	// 充值后清偿成功
	require.NoError(t, IncreaseUserQuota(userID, 600, true))
	require.NoError(t, RepayTaskBillingDebt(userID, debt.ID, RepayDebtOptions{}, 0))
	assert.Equal(t, int64(100), getUserQuotaDB(t, userID))

	// 幂等：重复清偿同一笔 → ErrDebtNotFound（已 paid）
	err = RepayTaskBillingDebt(userID, debt.ID, RepayDebtOptions{}, 0)
	require.ErrorIs(t, err, ErrDebtNotFound, "重复清偿必须幂等拒绝")
	assert.Equal(t, int64(100), getUserQuotaDB(t, userID), "重复清偿不得再次扣款")
}

func TestRepayTaskBillingDebt_OtherDebtsKeepFrozen(t *testing.T) {
	truncateTables(t)
	const userID = 3006
	u := &User{Id: userID, Username: "debt_multi", Quota: 10000, Status: common.UserStatusEnabled, Role: common.RoleCommonUser, AffCode: "aff-debt-multi"}
	require.NoError(t, DB.Create(u).Error)
	seedDebtTask(t, userID, "task-debt-a", 1000)
	seedDebtTask(t, userID, "task-debt-b", 1000)

	_, _, _, err := CreateDebtAndFreeze(DebtInput{UserId: userID, TaskId: "task-debt-a", PreConsumedQuota: 1000, ActualQuota: 1500, DeltaQuota: 500})
	require.NoError(t, err)
	_, _, _, err = CreateDebtAndFreeze(DebtInput{UserId: userID, TaskId: "task-debt-b", PreConsumedQuota: 1000, ActualQuota: 1500, DeltaQuota: 500})
	require.NoError(t, err)

	debtA, err := GetTaskBillingDebtByTaskId("task-debt-a")
	require.NoError(t, err)

	// 清偿一笔后仍有另一笔 → 不解冻
	require.NoError(t, RepayTaskBillingDebt(userID, debtA.ID, RepayDebtOptions{}, 0))
	var user User
	require.NoError(t, DB.First(&user, userID).Error)
	assert.True(t, user.DebtFrozen, "仍有其他未清欠款时不得解除欠款冻结")

	// 全部清偿后解除
	debtB, err := GetTaskBillingDebtByTaskId("task-debt-b")
	require.NoError(t, err)
	require.NoError(t, RepayTaskBillingDebt(userID, debtB.ID, RepayDebtOptions{}, 0))
	require.NoError(t, DB.First(&user, userID).Error)
	assert.False(t, user.DebtFrozen, "全部清偿后才解除欠款冻结")
}

func TestUnfreezeNeverTouchesAdminDisabledStatus(t *testing.T) {
	truncateTables(t)
	const userID = 3007
	u := &User{Id: userID, Username: "debt_disabled", Quota: 1000, Status: common.UserStatusDisabled, Role: common.RoleCommonUser, AffCode: "aff-debt-disabled", DebtFrozen: true}
	require.NoError(t, DB.Create(u).Error)

	require.NoError(t, UnfreezeUserDebt(userID))
	var user User
	require.NoError(t, DB.First(&user, userID).Error)
	assert.False(t, user.DebtFrozen, "欠款冻结解除")
	assert.Equal(t, common.UserStatusDisabled, user.Status, "管理员手工禁用状态绝不能被清偿流程误解除")
}

func getUserQuotaDB(t *testing.T, id int) int64 {
	t.Helper()
	var user User
	require.NoError(t, DB.Select("quota").Where("id = ?", id).First(&user).Error)
	return user.Quota
}

func seedUsedQuotaForUser(t *testing.T, userID int, amount int64) {
	t.Helper()
	require.NoError(t, DB.Model(&User{}).Where("id = ?", userID).Update("used_quota", amount).Error)
}

// seedDebtTask 创建欠款清偿所需的关联任务行（新清偿语义要求任务存在）。
func seedDebtTask(t *testing.T, userID int, taskID string, preConsumed int64) {
	t.Helper()
	task := &Task{
		TaskID:    taskID,
		UserId:    userID,
		Quota:     preConsumed,
		Status:    TaskStatusInProgress,
		Group:     "default",
		Data:      []byte(`{}`),
		CreatedAt: time.Now().Unix(),
		UpdatedAt: time.Now().Unix(),
		PrivateData: TaskPrivateData{
			BillingSource:      "wallet",
			ConsumeLogRecorded: true,
		},
	}
	require.NoError(t, DB.Create(task).Error)
}

// TestGuardedSettle_TransactionRollback 事务中途失败时资金/任务/欠款/冻结全部回滚。
// 通过 SQLite 触发器注入"任务额度 UPDATE 失败"，验证守卫结算整体回滚。
func TestGuardedSettle_TransactionRollback(t *testing.T) {
	truncateTables(t)
	const userID, quota, preConsumed, actual = 3008, 10000, 1000, 1500
	task := seedGuardTask(t, "task-guard-rollback", userID, quota, preConsumed)

	require.NoError(t, DB.Exec(`CREATE TRIGGER fail_guard_task_quota
		BEFORE UPDATE OF quota ON tasks
		BEGIN
			SELECT RAISE(ABORT, 'injected guarded quota update failure');
		END`).Error)
	t.Cleanup(func() { _ = DB.Exec("DROP TRIGGER IF EXISTS fail_guard_task_quota").Error })

	res := ApplyTaskQuotaDeltaGuarded(task, actual-preConsumed, false, TaskQuotaDeltaOptions{GuardPositiveDelta: true})
	require.Equal(t, TaskQuotaDeltaDBError, res)
	assert.Equal(t, int64(quota), getUserQuotaDB(t, userID), "事务回滚：资金不变")
	assert.Equal(t, int64(preConsumed), task.Quota, "事务回滚：内存任务额度不被污染")
	var reloaded Task
	require.NoError(t, DB.First(&reloaded, task.ID).Error)
	assert.Equal(t, int64(preConsumed), reloaded.Quota, "事务回滚：任务额度不变")

	// 移除注入后可重试成功
	require.NoError(t, DB.Exec("DROP TRIGGER IF EXISTS fail_guard_task_quota").Error)
	res = ApplyTaskQuotaDeltaGuarded(task, actual-preConsumed, false, TaskQuotaDeltaOptions{GuardPositiveDelta: true})
	require.Equal(t, TaskQuotaDeltaSuccess, res)
	assert.Equal(t, int64(quota-(actual-preConsumed)), getUserQuotaDB(t, userID))
}

// TestCreateDebtAndFreeze_Rollback 欠款+冻结在同一事务：任一步失败整体回滚。
func TestCreateDebtAndFreeze_Rollback(t *testing.T) {
	truncateTables(t)
	const userID = 3009
	u := &User{Id: userID, Username: "debt_rollback", Quota: 100, Status: common.UserStatusEnabled, Role: common.RoleCommonUser, AffCode: "aff-debt-rollback"}
	require.NoError(t, DB.Create(u).Error)

	// 注入：冻结用户的 UPDATE 强制失败（模拟 DB 错误）
	require.NoError(t, DB.Exec(`CREATE TRIGGER fail_debt_freeze
		BEFORE UPDATE OF debt_frozen ON users
		BEGIN
			SELECT RAISE(ABORT, 'injected freeze failure');
		END`).Error)
	t.Cleanup(func() { _ = DB.Exec("DROP TRIGGER IF EXISTS fail_debt_freeze").Error })

	created, _, _, err := CreateDebtAndFreeze(DebtInput{
		UserId: userID, TaskId: "task-debt-rollback", PreConsumedQuota: 1000, ActualQuota: 1800, DeltaQuota: 800,
	})
	require.Error(t, err, "冻结失败必须整体回滚")
	assert.False(t, created)

	var count int64
	DB.Model(&TaskBillingDebt{}).Where("task_id = ?", "task-debt-rollback").Count(&count)
	assert.EqualValues(t, 0, count, "事务回滚：欠款记录不落库")
	var user User
	require.NoError(t, DB.First(&user, userID).Error)
	assert.False(t, user.DebtFrozen, "事务回滚：冻结标记不落库")
}

// ===========================================================================
// 告警投递 claim（多实例去重 + 租约回收 + 幂等标记）
// ===========================================================================

// seedPendingDebtWithAlert 创建一条未清且告警未发送的欠款记录。
func seedPendingDebtWithAlert(t *testing.T, userID int, taskID string, delta int64) *TaskBillingDebt {
	t.Helper()
	debt := &TaskBillingDebt{
		UserId:           userID,
		TaskId:           taskID,
		UpstreamTaskId:   "cgt-alert-" + taskID,
		PreConsumedQuota: 1000,
		ActualQuota:      1000 + delta,
		DeltaQuota:       delta,
		Reason:           "余额不足",
		Status:           DebtStatusPending,
		AlertSent:        false,
		CreatedAt:        time.Now().Unix(),
		UpdatedAt:        time.Now().Unix(),
	}
	require.NoError(t, DB.Create(debt).Error)
	return debt
}

func TestClaimDebtAlert_ConcurrentOnlyOneWins(t *testing.T) {
	truncateTables(t)
	const userID = 5001
	seedUserForDebtAlert(t, userID)
	debt := seedPendingDebtWithAlert(t, userID, "task-alert-conc", 800)

	// 两个实例并发 claim 同一欠款 → 只有一个成功（RowsAffected==1）
	var wg sync.WaitGroup
	results := make([]bool, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			claimed, err := ClaimDebtAlert(debt.ID, 120)
			require.NoError(t, err)
			results[i] = claimed
		}(i)
	}
	wg.Wait()

	successes := 0
	for _, ok := range results {
		if ok {
			successes++
		}
	}
	assert.Equal(t, 1, successes, "并发 claim 同一告警只允许一个实例胜出")

	var reloaded TaskBillingDebt
	require.NoError(t, DB.First(&reloaded, debt.ID).Error)
	assert.NotZero(t, reloaded.AlertClaimAt, "claim 成功必须写入时间戳")
	assert.Equal(t, 1, reloaded.AlertAttempts, "attempt 自增一次")
}

func TestClaimDebtAlert_LeaseExpiryAllowsReclaim(t *testing.T) {
	truncateTables(t)
	const userID = 5002
	seedUserForDebtAlert(t, userID)
	debt := seedPendingDebtWithAlert(t, userID, "task-alert-lease", 800)

	// 实例 A claim（租约 120s）
	claimed, err := ClaimDebtAlert(debt.ID, 120)
	require.NoError(t, err)
	require.True(t, claimed)

	// 租约未到期：实例 B claim 失败（不重复发送）
	claimed, err = ClaimDebtAlert(debt.ID, 120)
	require.NoError(t, err)
	assert.False(t, claimed, "租约未到期不得被其他实例回收")

	// 进程崩溃兜底：直接推进 claim_at 到租约外，模拟超时回收
	require.NoError(t, DB.Model(&TaskBillingDebt{}).Where("id = ?", debt.ID).
		Update("alert_claim_at", time.Now().Unix()-300).Error)
	claimed, err = ClaimDebtAlert(debt.ID, 120)
	require.NoError(t, err)
	assert.True(t, claimed, "claim 超时后必须允许其他实例回收重试")
	assert.Equal(t, 2, func() int {
		var d TaskBillingDebt
		require.NoError(t, DB.First(&d, debt.ID).Error)
		return d.AlertAttempts
	}(), "回收重试会继续累计 attempt")
}

func TestClaimDebtAlert_ReleaseThenReclaim(t *testing.T) {
	truncateTables(t)
	const userID = 5003
	seedUserForDebtAlert(t, userID)
	debt := seedPendingDebtWithAlert(t, userID, "task-alert-release", 800)

	claimed, err := ClaimDebtAlert(debt.ID, 120)
	require.NoError(t, err)
	require.True(t, claimed)

	// 发送失败 → 释放 claim（AlertSent 保持 false）→ 下轮可重新 claim
	require.NoError(t, ReleaseDebtAlert(debt.ID))
	var reloaded TaskBillingDebt
	require.NoError(t, DB.First(&reloaded, debt.ID).Error)
	assert.False(t, reloaded.AlertSent, "发送失败必须保留 AlertSent=false 供重试")
	assert.Zero(t, reloaded.AlertClaimAt, "释放后 claim 时间戳归零")

	claimed, err = ClaimDebtAlert(debt.ID, 120)
	require.NoError(t, err)
	assert.True(t, claimed, "释放后可重新 claim")
}

func TestMarkTaskBillingDebtAlertSent_Idempotent(t *testing.T) {
	truncateTables(t)
	const userID = 5004
	seedUserForDebtAlert(t, userID)
	debt := seedPendingDebtWithAlert(t, userID, "task-alert-sent", 800)

	ok, err := MarkTaskBillingDebtAlertSent(debt.ID)
	require.NoError(t, err)
	assert.True(t, ok, "首次标记返回 true（false→true）")
	assert.False(t, HasPendingDebtAlerts(), "标记后不再有待发送告警")

	// 幂等：已标记的记录 no-op
	ok, err = MarkTaskBillingDebtAlertSent(debt.ID)
	require.NoError(t, err)
	assert.False(t, ok, "重复标记为 no-op")
}

func seedUserForDebtAlert(t *testing.T, id int) {
	t.Helper()
	u := &User{Id: id, Username: fmt.Sprintf("alert_u_%d", id), Quota: 10000, Status: common.UserStatusEnabled, Role: common.RoleCommonUser, AffCode: fmt.Sprintf("aff-alert-%d", id)}
	require.NoError(t, DB.Create(u).Error)
}

// ===========================================================================
// 订阅结算不得触碰钱包 Redis 缓存（缺陷七定向修复）
// ===========================================================================

// TestSettleSubscriptionDoesNotTouchWalletCache 验证钱包缓存同步只发生在钱包
// 资金来源：订阅结算（ApplySeedanceSettle/ApplyTaskQuotaDeltaGuarded 的
// isSubscription=true 分支）绝不调用 syncWalletQuotaCacheAfterCommit。
func TestSettleSubscriptionDoesNotTouchWalletCache(t *testing.T) {
	truncateTables(t)
	walletQuotaCacheSyncAttempts.Store(0)

	const userID, subID, preConsumed, delta = 5005, 5, 1000, 500
	u := &User{Id: userID, Username: "sub_cache", Quota: 500, Status: common.UserStatusEnabled, Role: common.RoleCommonUser, AffCode: "aff-sub-cache"}
	require.NoError(t, DB.Create(u).Error)
	sub := &UserSubscription{
		Id: subID, UserId: userID, AmountTotal: 2000, AmountUsed: 500,
		Status: "active", StartTime: time.Now().Unix(), EndTime: time.Now().Add(24 * time.Hour).Unix(),
	}
	require.NoError(t, DB.Create(sub).Error)
	task := &Task{
		TaskID: "task-sub-cache", UserId: userID, Quota: preConsumed, Status: TaskStatusInProgress,
		Data: []byte(`{}`), CreatedAt: time.Now().Unix(), UpdatedAt: time.Now().Unix(),
		PrivateData: TaskPrivateData{SubscriptionId: subID, BillingSource: "subscription"},
	}
	require.NoError(t, DB.Create(task).Error)

	// 订阅补扣：成功但绝不触发钱包缓存同步
	res, _ := ApplySeedanceSettle(task, delta, true, TaskQuotaDeltaOptions{GuardPositiveDelta: true})
	require.Equal(t, TaskQuotaDeltaSuccess, res)
	assert.Equal(t, int64(0), walletQuotaCacheSyncAttempts.Load(), "订阅结算不得同步钱包 Redis 缓存")

	// 钱包补扣：必须触发一次钱包缓存同步
	res, _ = ApplySeedanceSettle(task, delta, false, TaskQuotaDeltaOptions{GuardPositiveDelta: true})
	require.Equal(t, TaskQuotaDeltaSuccess, res)
	assert.Equal(t, int64(1), walletQuotaCacheSyncAttempts.Load(), "钱包结算必须同步钱包 Redis 缓存")
}

// TestGuardedSettleSubscriptionDoesNotTouchWalletCache 对通用守卫路径做同断言。
func TestGuardedSettleSubscriptionDoesNotTouchWalletCache(t *testing.T) {
	truncateTables(t)
	walletQuotaCacheSyncAttempts.Store(0)

	const userID, subID, preConsumed, delta = 5006, 6, 1000, 500
	u := &User{Id: userID, Username: "sub_cache2", Quota: 0, Status: common.UserStatusEnabled, Role: common.RoleCommonUser, AffCode: "aff-sub-cache2"}
	require.NoError(t, DB.Create(u).Error)
	sub := &UserSubscription{
		Id: subID, UserId: userID, AmountTotal: 2000, AmountUsed: 500,
		Status: "active", StartTime: time.Now().Unix(), EndTime: time.Now().Add(24 * time.Hour).Unix(),
	}
	require.NoError(t, DB.Create(sub).Error)
	task := &Task{
		TaskID: "task-sub-cache2", UserId: userID, Quota: preConsumed, Status: TaskStatusInProgress,
		Data: []byte(`{}`), CreatedAt: time.Now().Unix(), UpdatedAt: time.Now().Unix(),
		PrivateData: TaskPrivateData{SubscriptionId: subID, BillingSource: "subscription"},
	}
	require.NoError(t, DB.Create(task).Error)

	res := ApplyTaskQuotaDeltaGuarded(task, delta, true, TaskQuotaDeltaOptions{GuardPositiveDelta: true})
	require.Equal(t, TaskQuotaDeltaSuccess, res)
	assert.Equal(t, int64(0), walletQuotaCacheSyncAttempts.Load(), "订阅结算（通用守卫路径）不得同步钱包 Redis 缓存")
}

// ===========================================================================
// 多字节原因截断（truncateDebtReason 字节安全，缺陷七定向修复）
// ===========================================================================

func TestTruncateDebtReason_MultiByteSafe(t *testing.T) {
	// 255 个中文字符（每字 3 字节，共 765 字节）→ 截断到 255 字节且不 panic、
	// 不切断多字节字符
	long := strings.Repeat("欠", 300)
	got := truncateDebtReason(long)
	require.LessOrEqual(t, len(got), 255, "截断后字节数必须 ≤ 255")
	// 截断结果必须是合法 UTF-8（无半个多字节字符）
	require.True(t, utf8.ValidString(got), "截断结果必须是合法 UTF-8")
	// 短字符串原样返回
	assert.Equal(t, "ok", truncateDebtReason("ok"))
	// 混合中英文
	mixed := strings.Repeat("a", 200) + strings.Repeat("欠", 100)
	got = truncateDebtReason(mixed)
	require.LessOrEqual(t, len(got), 255)
	require.True(t, utf8.ValidString(got))
}

var _ = errors.Is // keep errors import if unused paths change
