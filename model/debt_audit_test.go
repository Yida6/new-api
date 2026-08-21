package model

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ===========================================================================
// 欠款核销、冻结解除与审计闭环（问题七）
// ===========================================================================

func seedDebtAuditUser(t *testing.T, id int, quota int64) {
	t.Helper()
	u := &User{Id: id, Username: fmt.Sprintf("debt_audit_%d", id), Quota: quota, Status: common.UserStatusEnabled, Role: common.RoleCommonUser, AffCode: fmt.Sprintf("aff-debt-audit-%d", id)}
	require.NoError(t, DB.Create(u).Error)
}

func countDebtAudits(t *testing.T, debtID int64) int64 {
	t.Helper()
	var n int64
	require.NoError(t, DB.Model(&TaskBillingDebtAudit{}).Where("debt_id = ?", debtID).Count(&n).Error)
	return n
}

// 核销（voided）后用户不存在其他 pending 欠款 → 自动解除 debt_frozen；
// 核销与自动解冻分别落审计；绝不动 users.status。
func TestVoidTaskBillingDebt_AutoUnfreezesWhenNoOtherPending(t *testing.T) {
	truncateTables(t)
	const userID = 8001
	seedDebtAuditUser(t, userID, 100)
	seedDebtTask(t, userID, "task-void-unfreeze", 1000)

	_, _, _, err := CreateDebtAndFreeze(DebtInput{UserId: userID, TaskId: "task-void-unfreeze", PreConsumedQuota: 1000, ActualQuota: 1800, DeltaQuota: 800})
	require.NoError(t, err)
	debt, err := GetTaskBillingDebtByTaskId("task-void-unfreeze")
	require.NoError(t, err)
	require.True(t, mustUserDebtFrozen(t, userID), "前置：欠款已冻结")

	// 管理员核销（带原因）
	require.NoError(t, VoidTaskBillingDebt(debt.ID, 9001, "上游重复计费，人工核销"))
	reloaded, err := GetTaskBillingDebtByID(debt.ID)
	require.NoError(t, err)
	assert.Equal(t, DebtStatusVoided, reloaded.Status)

	// 无其他 pending → 自动解除欠款冻结（只动 debt_frozen）
	assert.False(t, mustUserDebtFrozen(t, userID), "核销后无其他未清欠款必须自动解除欠款冻结")
	var user User
	require.NoError(t, DB.First(&user, userID).Error)
	assert.Equal(t, common.UserStatusEnabled, user.Status, "核销绝不动 users.status")

	// 审计：void + auto_unfreeze 各一条，字段完整（管理员 ID、原因、时间、债务 ID、用户 ID、额度）
	var audits []TaskBillingDebtAudit
	require.NoError(t, DB.Where("debt_id = ?", debt.ID).Order("id").Find(&audits).Error)
	require.Len(t, audits, 2, "核销审计 + 自动解冻审计")
	assert.Equal(t, "void", audits[0].Action)
	assert.Equal(t, 9001, audits[0].AdminId, "审计必须记录管理员 ID")
	assert.Equal(t, userID, audits[0].UserId, "审计必须记录用户 ID")
	assert.Equal(t, int64(800), audits[0].DeltaQuota, "审计必须记录额度")
	assert.NotZero(t, audits[0].CreatedAt, "审计必须记录时间")
	assert.Contains(t, audits[0].Reason, "人工核销", "审计必须记录原因")
	assert.Equal(t, "auto_unfreeze", audits[1].Action)
	assert.Equal(t, 9001, audits[1].AdminId)
	assert.Equal(t, userID, audits[1].UserId)

	// 幂等：重复核销已 voided 的记录 → ErrDebtNotFound，不重复写审计
	err = VoidTaskBillingDebt(debt.ID, 9001, "再核销一次")
	require.ErrorIs(t, err, ErrDebtNotFound)
	assert.Equal(t, int64(2), countDebtAudits(t, debt.ID), "重复核销不得重复写审计")
}

// 有其他 pending 欠款时，核销**不**解除冻结（等待其他欠款闭环）。
func TestVoidTaskBillingDebt_KeepsFrozenWhileOtherPending(t *testing.T) {
	truncateTables(t)
	const userID = 8002
	seedDebtAuditUser(t, userID, 100)
	seedDebtTask(t, userID, "task-void-a", 1000)
	seedDebtTask(t, userID, "task-void-b", 1000)

	_, _, _, err := CreateDebtAndFreeze(DebtInput{UserId: userID, TaskId: "task-void-a", PreConsumedQuota: 1000, ActualQuota: 1500, DeltaQuota: 500})
	require.NoError(t, err)
	_, _, _, err = CreateDebtAndFreeze(DebtInput{UserId: userID, TaskId: "task-void-b", PreConsumedQuota: 1000, ActualQuota: 1500, DeltaQuota: 500})
	require.NoError(t, err)
	debtA, err := GetTaskBillingDebtByTaskId("task-void-a")
	require.NoError(t, err)

	require.NoError(t, VoidTaskBillingDebt(debtA.ID, 9001, "核销 A"))
	assert.True(t, mustUserDebtFrozen(t, userID), "仍有其他未清欠款时核销不得解除冻结")
	// 只写 void 审计（无 auto_unfreeze）
	assert.Equal(t, int64(1), countDebtAudits(t, debtA.ID))
}

// 显式人工解冻：带理由与审计；仍有未清欠款时拒绝；幂等（已解冻 no-op）。
func TestUnfreezeUserDebtAudited_WithAuditAndGuards(t *testing.T) {
	truncateTables(t)
	const userID = 8003
	seedDebtAuditUser(t, userID, 100)
	seedDebtTask(t, userID, "task-unfreeze-a", 1000)

	_, _, _, err := CreateDebtAndFreeze(DebtInput{UserId: userID, TaskId: "task-unfreeze-a", PreConsumedQuota: 1000, ActualQuota: 1500, DeltaQuota: 500})
	require.NoError(t, err)
	debtA, err := GetTaskBillingDebtByTaskId("task-unfreeze-a")
	require.NoError(t, err)
	require.True(t, mustUserDebtFrozen(t, userID))

	// 仍有 pending 欠款 → 拒绝解冻（ErrDebtPendingRemaining，不得绕过清偿闭环）
	err = UnfreezeUserDebtAudited(userID, debtA.ID, 9001, "误冻结，先解冻")
	require.ErrorIs(t, err, ErrDebtPendingRemaining)
	assert.True(t, mustUserDebtFrozen(t, userID), "拒绝后保持冻结")
	assert.Equal(t, int64(0), countDebtAudits(t, debtA.ID), "拒绝解冻不写审计")

	// 核销该欠款（自动解冻）后，显式解冻为幂等 no-op（不再重复写审计）
	require.NoError(t, VoidTaskBillingDebt(debtA.ID, 9001, "核销"))
	assert.False(t, mustUserDebtFrozen(t, userID))
	before := countDebtAudits(t, debtA.ID) // void + auto_unfreeze = 2
	err = UnfreezeUserDebtAudited(userID, debtA.ID, 9001, "再解冻一次")
	require.NoError(t, err)
	assert.Equal(t, before, countDebtAudits(t, debtA.ID), "已解冻的幂等 no-op 不重复写审计")
	var user User
	require.NoError(t, DB.First(&user, userID).Error)
	assert.Equal(t, common.UserStatusEnabled, user.Status, "人工解冻绝不动 users.status")
}

// 显式人工解冻成功路径：单独构造"已核销且已自动解冻失败"的场景不可行
// （自动解冻与核销同事务），因此验证：解冻审计在真正发生状态迁移时写入。
func TestUnfreezeUserDebtAudited_RecordsAuditOnMigration(t *testing.T) {
	truncateTables(t)
	const userID = 8004
	seedDebtAuditUser(t, userID, 100)
	// 直接冻结用户 + 一条已核销欠款（模拟历史遗留：欠款已核销但冻结未解除）
	require.NoError(t, DB.Model(&User{}).Where("id = ?", userID).Updates(map[string]any{
		"debt_frozen":        true,
		"debt_frozen_at":     time.Now().Unix(),
		"debt_frozen_reason": "历史遗留",
	}).Error)
	debt := &TaskBillingDebt{
		UserId: userID, TaskId: "task-unfreeze-hist", Status: DebtStatusVoided,
		PreConsumedQuota: 1000, ActualQuota: 1500, DeltaQuota: 500,
		Reason: "历史核销", CreatedAt: time.Now().Unix(), UpdatedAt: time.Now().Unix(),
	}
	require.NoError(t, DB.Create(debt).Error)

	require.NoError(t, UnfreezeUserDebtAudited(userID, debt.ID, 9001, "历史遗留解冻"))
	assert.False(t, mustUserDebtFrozen(t, userID))
	var audits []TaskBillingDebtAudit
	require.NoError(t, DB.Where("debt_id = ?", debt.ID).Order("id").Find(&audits).Error)
	require.Len(t, audits, 1, "解冻迁移必须写一条审计")
	assert.Equal(t, "unfreeze", audits[0].Action)
	assert.Equal(t, 9001, audits[0].AdminId)
	assert.Equal(t, userID, audits[0].UserId)
	assert.Equal(t, int64(500), audits[0].DeltaQuota)
	assert.Contains(t, audits[0].Reason, "历史遗留解冻")
}

// 清偿审计：清偿 + 自动解冻各写一条，记录管理员 ID/原因/时间/债务/用户/额度。
func TestRepayTaskBillingDebt_RecordsAudit(t *testing.T) {
	truncateTables(t)
	const userID = 8005
	seedDebtAuditUser(t, userID, 2000)
	seedDebtTask(t, userID, "task-repay-audit", 1000)

	_, _, _, err := CreateDebtAndFreeze(DebtInput{UserId: userID, TaskId: "task-repay-audit", PreConsumedQuota: 1000, ActualQuota: 1500, DeltaQuota: 500})
	require.NoError(t, err)
	debt, err := GetTaskBillingDebtByTaskId("task-repay-audit")
	require.NoError(t, err)

	require.NoError(t, RepayTaskBillingDebt(userID, debt.ID, RepayDebtOptions{}, 10086))
	var audits []TaskBillingDebtAudit
	require.NoError(t, DB.Where("debt_id = ?", debt.ID).Order("id").Find(&audits).Error)
	require.Len(t, audits, 2, "清偿审计 + 自动解冻审计")
	assert.Equal(t, "auto_unfreeze", audits[0].Action)
	assert.Equal(t, "repay", audits[1].Action)
	assert.Equal(t, 10086, audits[1].AdminId, "清偿审计必须记录管理员 ID")
	assert.Equal(t, userID, audits[1].UserId)
	assert.Equal(t, int64(500), audits[1].DeltaQuota)
	assert.NotZero(t, audits[1].CreatedAt)
	assert.Contains(t, audits[1].Reason, "欠款清偿")
}

// ===========================================================================
// 并发清偿/核销/解冻：只能有一个状态迁移成功（问题八 item 10）
// ===========================================================================

// 并发清偿 + 核销同一笔欠款：恰好一个成功，其余幂等拒绝；审计恰有一条迁移。
func TestDebtVoidRepay_ConcurrentSingleMigrationWins(t *testing.T) {
	truncateTables(t)
	const userID = 8006
	seedDebtAuditUser(t, userID, 2000)
	seedDebtTask(t, userID, "task-debt-race", 1000)
	_, _, _, err := CreateDebtAndFreeze(DebtInput{UserId: userID, TaskId: "task-debt-race", PreConsumedQuota: 1000, ActualQuota: 1500, DeltaQuota: 500})
	require.NoError(t, err)
	debt, err := GetTaskBillingDebtByTaskId("task-debt-race")
	require.NoError(t, err)

	const workers = 4 // 2 清偿 + 2 核销
	var wg sync.WaitGroup
	errs := make([]error, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if i%2 == 0 {
				errs[i] = RepayTaskBillingDebt(userID, debt.ID, RepayDebtOptions{}, 100)
			} else {
				errs[i] = VoidTaskBillingDebt(debt.ID, 100, "race-void")
			}
		}(i)
	}
	wg.Wait()

	success := 0
	for _, e := range errs {
		if e == nil {
			success++
		} else {
			require.ErrorIs(t, e, ErrDebtNotFound, "败者必须幂等拒绝")
		}
	}
	assert.Equal(t, 1, success, "并发清偿/核销只能有一个状态迁移成功")

	reloaded, err := GetTaskBillingDebtByID(debt.ID)
	require.NoError(t, err)
	switch reloaded.Status {
	case DebtStatusPaid:
		assert.Equal(t, 2000-500, getUserQuotaDB(t, userID), "清偿收款一次")
		assert.False(t, mustUserDebtFrozen(t, userID), "清偿后自动解冻")
	case DebtStatusVoided:
		assert.Equal(t, int64(2000), getUserQuotaDB(t, userID), "核销不收款")
		assert.False(t, mustUserDebtFrozen(t, userID), "核销后自动解冻")
	default:
		t.Fatalf("unexpected debt status: %s", reloaded.Status)
	}
	// 审计：一条迁移审计 + 一条自动解冻审计
	assert.Equal(t, int64(2), countDebtAudits(t, debt.ID), "恰好一条迁移 + 一条自动解冻")
}

// 并发显式解冻：只有一个状态迁移成功（已解冻的 no-op 不重复写审计）。
func TestDebtUnfreeze_ConcurrentSingleMigrationWins(t *testing.T) {
	truncateTables(t)
	const userID = 8007
	seedDebtAuditUser(t, userID, 100)
	require.NoError(t, DB.Model(&User{}).Where("id = ?", userID).Updates(map[string]any{
		"debt_frozen":        true,
		"debt_frozen_at":     time.Now().Unix(),
		"debt_frozen_reason": "测试冻结",
	}).Error)
	debt := &TaskBillingDebt{
		UserId: userID, TaskId: "task-unfreeze-race", Status: DebtStatusVoided,
		PreConsumedQuota: 1000, ActualQuota: 1500, DeltaQuota: 500,
		Reason: "历史核销", CreatedAt: time.Now().Unix(), UpdatedAt: time.Now().Unix(),
	}
	require.NoError(t, DB.Create(debt).Error)

	var wg sync.WaitGroup
	errs := make([]error, 4)
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = UnfreezeUserDebtAudited(userID, debt.ID, 100, "并发解冻")
		}(i)
	}
	wg.Wait()

	for _, e := range errs {
		require.NoError(t, e, "并发解冻幂等：已解冻的 no-op 成功")
	}
	assert.False(t, mustUserDebtFrozen(t, userID))
	assert.Equal(t, int64(1), countDebtAudits(t, debt.ID), "状态迁移只发生一次，审计只写一次")
}

func mustUserDebtFrozen(t *testing.T, userID int) bool {
	t.Helper()
	frozen, err := GetUserDebtFrozen(userID)
	require.NoError(t, err)
	return frozen
}
