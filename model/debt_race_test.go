package model

import (
	"sync"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ===========================================================================
// 问题二：欠款创建与解冻之间的竞态回归
//
// 修复前缺陷：RepayTaskBillingDebt / VoidTaskBillingDebt / UnfreezeUserDebtAudited
// 各自"查询 pending 数量 → 判断为 0 → 更新 debt_frozen=false"三步之间没有与
// CreateDebtAndFreeze 建立串行化边界：清偿/核销/解冻事务可在"查到无 pending"
// 与"写入 debt_frozen=false"之间被并发创建的新欠款插入，最终出现
// "存在 pending 欠款但用户未冻结"的不一致。
//
// 修复后：所有欠款流程在事务内先 lockForUpdate 用户行（统一串行化边界，
// 锁顺序一致：用户行 → 债务行），pending 检查与 debt_frozen 迁移都在该锁
// 保护下执行。SQLite 测试环境无 FOR UPDATE（见 lockForUpdate），由单连接
// 串行化保证等价语义，并发断言仍验证最终不变量：
//   存在 pending  → debt_frozen 必须为 true；
//   不存在 pending → 才允许 debt_frozen=false。
// ===========================================================================

func isDebtFrozenDB(t *testing.T, userID int) bool {
	t.Helper()
	var user User
	require.NoError(t, DB.Select("debt_frozen").Where("id = ?", userID).First(&user).Error)
	return user.DebtFrozen
}

// assertDebtFrozenConsistent 断言欠款冻结不变量：存在 pending → 冻结；无 pending
// → 才允许解冻。这是并发测试的最终状态约束，与交错顺序无关。
func assertDebtFrozenConsistent(t *testing.T, userID int) {
	t.Helper()
	var open int64
	require.NoError(t, DB.Model(&TaskBillingDebt{}).
		Where("user_id = ? AND status = ?", userID, DebtStatusPending).Count(&open).Error)
	frozen := isDebtFrozenDB(t, userID)
	if open > 0 {
		assert.True(t, frozen, "存在 pending 欠款时用户必须保持冻结")
	} else {
		assert.False(t, frozen, "不存在 pending 欠款时才允许 debt_frozen=false")
	}
}

func seedRaceUser(t *testing.T, userID int, quota int) {
	t.Helper()
	u := &User{Id: userID, Username: "race_user", Quota: quota, Status: common.UserStatusEnabled, Role: common.RoleCommonUser, AffCode: "aff-race"}
	require.NoError(t, DB.Create(u).Error)
}

// 并发 1：CreateDebtAndFreeze 与 RepayTaskBillingDebt（自动解冻）并发。
// debt1 已存在（用户已冻结）；并发清偿 debt1 + 创建 debt2。
// 无论交错顺序，debt2 必为 pending → 用户必须保持冻结。
func TestDebtRace_CreateVsRepayUnfreeze(t *testing.T) {
	truncateTables(t)
	const userID = 6001
	seedRaceUser(t, userID, 10000)
	seedDebtTask(t, userID, "task-race-repay-1", 1000)
	seedDebtTask(t, userID, "task-race-repay-2", 1000)

	_, _, _, err := CreateDebtAndFreeze(DebtInput{UserId: userID, TaskId: "task-race-repay-1", PreConsumedQuota: 1000, ActualQuota: 1500, DeltaQuota: 500})
	require.NoError(t, err)
	debt1, err := GetTaskBillingDebtByTaskId("task-race-repay-1")
	require.NoError(t, err)
	require.True(t, isDebtFrozenDB(t, userID), "前置：debt1 已冻结用户")

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		// 清偿 debt1（余额充足 → 自动解冻判定）；若 debt2 已创建则不解冻
		_ = RepayTaskBillingDebt(userID, debt1.ID, RepayDebtOptions{}, 0)
	}()
	go func() {
		defer wg.Done()
		// 并发创建 debt2（同一用户，锁定同一用户行）
		_, _, _, _ = CreateDebtAndFreeze(DebtInput{UserId: userID, TaskId: "task-race-repay-2", PreConsumedQuota: 1000, ActualQuota: 2000, DeltaQuota: 1000})
	}()
	wg.Wait()

	assertDebtFrozenConsistent(t, userID)
	assert.True(t, isDebtFrozenDB(t, userID), "debt2 必为 pending：用户必须保持冻结（竞态已被用户锁阻断）")

	// debt1 无论被清偿或保持 pending，都不会出现"有 pending 且未冻结"
	var open int64
	require.NoError(t, DB.Model(&TaskBillingDebt{}).
		Where("user_id = ? AND status = ?", userID, DebtStatusPending).Count(&open).Error)
	require.GreaterOrEqual(t, open, int64(1), "至少 debt2 保持 pending")
}

// 并发 2：CreateDebtAndFreeze 与 VoidTaskBillingDebt（自动解冻）并发。
// debt1 已存在（用户已冻结）；并发核销 debt1 + 创建 debt2。
// 无论交错顺序，debt2 必为 pending → 用户必须保持冻结。
func TestDebtRace_CreateVsVoidUnfreeze(t *testing.T) {
	truncateTables(t)
	const userID = 6002
	seedRaceUser(t, userID, 10000)
	seedDebtTask(t, userID, "task-race-void-1", 1000)
	seedDebtTask(t, userID, "task-race-void-2", 1000)

	_, _, _, err := CreateDebtAndFreeze(DebtInput{UserId: userID, TaskId: "task-race-void-1", PreConsumedQuota: 1000, ActualQuota: 1500, DeltaQuota: 500})
	require.NoError(t, err)
	debt1, err := GetTaskBillingDebtByTaskId("task-race-void-1")
	require.NoError(t, err)
	require.True(t, isDebtFrozenDB(t, userID))

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		// 核销 debt1（若 debt2 已创建则不解冻）
		_ = VoidTaskBillingDebt(debt1.ID, 6102, "race-void")
	}()
	go func() {
		defer wg.Done()
		_, _, _, _ = CreateDebtAndFreeze(DebtInput{UserId: userID, TaskId: "task-race-void-2", PreConsumedQuota: 1000, ActualQuota: 2000, DeltaQuota: 1000})
	}()
	wg.Wait()

	assertDebtFrozenConsistent(t, userID)
	assert.True(t, isDebtFrozenDB(t, userID), "debt2 必为 pending：用户必须保持冻结")
}

// 并发 3：CreateDebtAndFreeze 与 UnfreezeUserDebtAudited 并发。
// 用户已冻结但无 pending（debt1 已清偿，仅作为解冻参数）；
// 并发显式解冻 + 创建 debt2。
// 无论交错顺序：若解冻先完成，创建会重新冻结；若创建先完成，解冻判定
// 看到 pending → ErrDebtPendingRemaining 拒绝。最终 debt2 pending → 冻结。
func TestDebtRace_CreateVsUnfreezeAudited(t *testing.T) {
	truncateTables(t)
	const userID, adminID = 6003, 6103
	seedRaceUser(t, userID, 10000)
	seedDebtTask(t, userID, "task-race-unf-1", 1000)

	// 前置：debt1 创建并清偿（paid），作为解冻参数；随后手动重新冻结，
	// 模拟"已冻结但当前无 pending"的管理场景。
	_, _, _, err := CreateDebtAndFreeze(DebtInput{UserId: userID, TaskId: "task-race-unf-1", PreConsumedQuota: 1000, ActualQuota: 1500, DeltaQuota: 500})
	require.NoError(t, err)
	debt1, err := GetTaskBillingDebtByTaskId("task-race-unf-1")
	require.NoError(t, err)
	require.NoError(t, RepayTaskBillingDebt(userID, debt1.ID, RepayDebtOptions{}, 0))
	require.NoError(t, DB.Model(&User{}).Where("id = ?", userID).Updates(map[string]any{
		"debt_frozen":        true,
		"debt_frozen_at":     time.Now().Unix(),
		"debt_frozen_reason": "race-fixture",
	}).Error)
	require.True(t, isDebtFrozenDB(t, userID))

	seedDebtTask(t, userID, "task-race-unf-2", 1000)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_ = UnfreezeUserDebtAudited(userID, debt1.ID, adminID, "race-unfreeze")
	}()
	go func() {
		defer wg.Done()
		_, _, _, _ = CreateDebtAndFreeze(DebtInput{UserId: userID, TaskId: "task-race-unf-2", PreConsumedQuota: 1000, ActualQuota: 2000, DeltaQuota: 1000})
	}()
	wg.Wait()

	assertDebtFrozenConsistent(t, userID)
	assert.True(t, isDebtFrozenDB(t, userID), "debt2 必为 pending：用户必须保持冻结")

	// 清理后（清偿 debt2）允许解冻——验证"无 pending 才允许 debt_frozen=false"
	debt2, err := GetTaskBillingDebtByTaskId("task-race-unf-2")
	require.NoError(t, err)
	require.NoError(t, RepayTaskBillingDebt(userID, debt2.ID, RepayDebtOptions{}, 0))
	assertDebtFrozenConsistent(t, userID)
	assert.False(t, isDebtFrozenDB(t, userID), "全部清偿后允许解除欠款冻结")
}
