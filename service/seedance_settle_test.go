package service

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relaykit/dto"
	relaykittypes "github.com/QuantumNous/new-api/relaykit/types"
	"github.com/QuantumNous/new-api/setting/ratio_setting"
	"github.com/QuantumNous/new-api/types"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// ---------------------------------------------------------------------------
// Seedance 专用结算闭环（SettleSeedanceTaskBilling）回归测试
// 计费语义：预扣 P，实际 A；A>P 走带守卫补扣，余额不足 → 欠款+冻结+告警闭环。
// ---------------------------------------------------------------------------

var seedanceTaskSeq int64

// seedSeedanceTask 构造一个 Seedance 任务（doubao 平台，钱包计费，已预扣 quota）。
// TaskID 使用原子递增序号，避免并发下 Windows 时钟精度导致碰撞（碰撞会让
// 不同任务共用 task_id，破坏欠款按任务幂等的断言）。
func seedSeedanceTask(t *testing.T, userID, channelID, tokenID int, preConsumed int64) *model.Task {
	t.Helper()
	seq := atomic.AddInt64(&seedanceTaskSeq, 1)
	task := &model.Task{
		TaskID:    fmt.Sprintf("task_seedance_%d_%d", userID, seq),
		UserId:    userID,
		ChannelId: channelID,
		Quota:     preConsumed,
		Status:    model.TaskStatus(model.TaskStatusInProgress),
		Group:     "default",
		Platform:  constant.TaskPlatform("54"), // doubao video
		Data:      json.RawMessage(`{}`),
		CreatedAt: time.Now().Unix(),
		UpdatedAt: time.Now().Unix(),
		Properties: model.Properties{
			OriginModelName: "doubao-seedance-2-0-260128",
		},
		PrivateData: model.TaskPrivateData{
			UpstreamTaskID:       "cgt-seedance-" + fmt.Sprint(time.Now().UnixNano()),
			BillingSource:       "wallet",
			TokenId:             tokenID,
			ConsumeLogRecorded:  true,
			BillingContext: &model.TaskBillingContext{
				ModelPrice:      0.02,
				GroupRatio:      1.0,
				ModelRatio:      3.15,
				OriginModelName: "doubao-seedance-2-0-260128",
			},
		},
	}
	require.NoError(t, model.DB.Create(task).Error)
	// 模拟提交时的 Token 预扣（生产路径由 PreConsumeTokenQuota 在提交时完成：
	// remain -= preConsumed、used += preConsumed）。没有这一步，退款方向的
	// used_quota >= abs(delta) 守卫会在测试中错误失败——生产路径下 Token 的
	// used_quota 必然 ≥ 已预扣额。
	if tokenID > 0 && preConsumed > 0 {
		res := model.DB.Model(&model.Token{}).Where("id = ?", tokenID).
			Updates(map[string]interface{}{
				"remain_quota": gorm.Expr("remain_quota - ?", preConsumed),
				"used_quota":   gorm.Expr("used_quota + ?", preConsumed),
			})
		require.NoError(t, res.Error)
		require.Equal(t, int64(1), res.RowsAffected, "fixture: token 必须存在")
	}
	return task
}

func seedSeedanceUser(t *testing.T, id int, quota int64, role int) {
	t.Helper()
	user := &model.User{Id: id, Username: fmt.Sprintf("seedance_%d", id), Quota: quota, Status: common.UserStatusEnabled, Role: role, AffCode: fmt.Sprintf("aff-seed-%d", id)}
	require.NoError(t, model.DB.Create(user).Error)
}

func getDebtCount(t *testing.T, taskID string) int64 {
	t.Helper()
	var count int64
	model.DB.Model(&model.TaskBillingDebt{}).Where("task_id = ?", taskID).Count(&count)
	return count
}

func isUserDebtFrozen(t *testing.T, userID int) bool {
	t.Helper()
	frozen, err := model.GetUserDebtFrozen(userID)
	require.NoError(t, err)
	return frozen
}

// 场景1：保守预扣大于实际费用 → 正确退款（多预扣部分退回）。
func TestSeedanceSettle_OverchargedRefund(t *testing.T) {
	truncate(t)
	ctx := context.Background()
	const userID, tokenID, channelID = 4001, 4001, 4001
	const initQuota, preConsumed, actual = 100000, 5000, 1000
	seedSeedanceUser(t, userID, initQuota, common.RoleCommonUser)
	seedToken(t, tokenID, userID, "sk-seed-refund", 100000)
	seedChannel(t, channelID)
	seedUsedQuota(t, userID, channelID, preConsumed)
	task := seedSeedanceTask(t, userID, channelID, tokenID, preConsumed)

	adaptor := &mockAdaptor{adjustReturn: actual}
	outcome := SettleSeedanceTaskBilling(ctx, adaptor, task, &relaycommon.TaskInfo{Status: model.TaskStatusSuccess})
	require.Equal(t, SeedanceSettleSuccess, outcome)

	// 问题一要求的全量断言：钱包、Token remain/used、任务额度、累计消费
	assert.Equal(t, int64(initQuota+(preConsumed-actual)), getUserQuota(t, userID), "多预扣部分退款（钱包）")
	// Token：fixture 预扣 5000 后 remain=95000/used=5000；退款 4000 → remain=99000/used=1000
	assert.Equal(t, int64(100000-preConsumed+(preConsumed-actual)), getTokenRemainQuota(t, tokenID), "Token remain 加回退款额")
	assert.Equal(t, int64(preConsumed-(preConsumed-actual)), getTokenUsedQuota(t, tokenID), "Token used 冲减退款额")
	assert.Equal(t, int64(100000), getTokenRemainQuota(t, tokenID)+getTokenUsedQuota(t, tokenID), "remain+used 资金不变量")
	assert.Equal(t, int64(actual), task.Quota, "任务额度收敛到实际费用")
	assert.Equal(t, int64(actual), getUserUsedQuota(t, userID), "用户累计消耗冲减到实际费用")
	assert.EqualValues(t, actual, getChannelUsedQuota(t, channelID), "渠道累计消耗冲减到实际费用")
	assert.Equal(t, int64(0), getDebtCount(t, task.TaskID), "退款场景不得产生欠款")
	assert.False(t, isUserDebtFrozen(t, userID), "退款场景不得冻结")
}

// 场景1b：订阅资金来源的 Token 退款——订阅已用量、Token 与任务额度同方向收敛。
func TestSeedanceSettle_SubscriptionRefundTokenInvariant(t *testing.T) {
	truncate(t)
	ctx := context.Background()
	const userID, tokenID, channelID, subID = 40011, 40011, 40011, 40011
	seedSeedanceUser(t, userID, 0, common.RoleCommonUser)
	seedToken(t, tokenID, userID, "sk-seed-subrefund", 100000)
	seedChannel(t, channelID)
	seedSubscription(t, subID, userID, 10000, 5000) // 已用 5000
	seedUsedQuota(t, userID, channelID, 3000)
	task := seedSeedanceTask(t, userID, channelID, tokenID, 3000)
	task.PrivateData.SubscriptionId = subID
	task.PrivateData.BillingSource = BillingSourceSubscription
	require.NoError(t, task.Update())

	// 实际 1000 < 预扣 3000 → 退款 2000：订阅已用量、Token、累计消耗同方向冲减
	adaptor := &mockAdaptor{adjustReturn: 1000}
	require.Equal(t, SeedanceSettleSuccess, SettleSeedanceTaskBilling(ctx, adaptor, task, &relaycommon.TaskInfo{Status: model.TaskStatusSuccess}))
	assert.Equal(t, int64(5000-2000), getSubscriptionUsed(t, subID), "订阅已用量冲减退款额")
	assert.Equal(t, int64(100000-3000+2000), getTokenRemainQuota(t, tokenID), "Token remain 加回")
	assert.Equal(t, int64(3000-2000), getTokenUsedQuota(t, tokenID), "Token used 冲减")
	assert.Equal(t, int64(100000), getTokenRemainQuota(t, tokenID)+getTokenUsedQuota(t, tokenID), "remain+used 不变量")
	assert.Equal(t, int64(1000), task.Quota, "任务额度收敛")
	assert.Equal(t, int64(1000), getUserUsedQuota(t, userID), "用户累计消耗冲减")
	assert.Equal(t, int64(0), getDebtCount(t, task.TaskID))
}

// 场景2：余额恰好够差额 → 成功结算且余额为 0。
func TestSeedanceSettle_ExactBalance(t *testing.T) {
	truncate(t)
	ctx := context.Background()
	const userID, tokenID, channelID = 4002, 4002, 4002
	const balance, preConsumed, actual = 500, 1000, 1500 // 差额 500
	seedSeedanceUser(t, userID, balance, common.RoleCommonUser)
	seedToken(t, tokenID, userID, "sk-seed-exact", 100000)
	seedChannel(t, channelID)
	seedUsedQuota(t, userID, channelID, preConsumed)
	task := seedSeedanceTask(t, userID, channelID, tokenID, preConsumed)

	adaptor := &mockAdaptor{adjustReturn: actual}
	outcome := SettleSeedanceTaskBilling(ctx, adaptor, task, &relaycommon.TaskInfo{Status: model.TaskStatusSuccess})
	require.Equal(t, SeedanceSettleSuccess, outcome)
	assert.Equal(t, int64(0), getUserQuota(t, userID), "余额恰好够差额时余额为 0")
	assert.Equal(t, int64(actual), task.Quota)
	assert.Equal(t, int64(0), getDebtCount(t, task.TaskID))
}

// 场景3：余额少 1 → 拒绝补扣，余额不变且不为负；生成欠款、冻结用户。
func TestSeedanceSettle_ShortByOneCreatesDebtAndFreeze(t *testing.T) {
	truncate(t)
	ctx := context.Background()
	const userID, tokenID, channelID = 4003, 4003, 4003
	const balance, preConsumed, actual = 499, 1000, 1500 // 差额 500，余额少 1
	seedSeedanceUser(t, userID, balance, common.RoleCommonUser)
	seedToken(t, tokenID, userID, "sk-seed-short", 100000)
	seedChannel(t, channelID)
	task := seedSeedanceTask(t, userID, channelID, tokenID, preConsumed)

	adaptor := &mockAdaptor{adjustReturn: actual}
	outcome := SettleSeedanceTaskBilling(ctx, adaptor, task, &relaycommon.TaskInfo{Status: model.TaskStatusSuccess})
	require.Equal(t, SeedanceSettleDebtCreated, outcome, "余额不足必须进入欠款闭环")

	assert.Equal(t, int64(balance), getUserQuota(t, userID), "拒绝补扣时余额不变")
	assert.GreaterOrEqual(t, getUserQuota(t, userID), int64(0), "余额永不为负")
	assert.Equal(t, int64(preConsumed), task.Quota, "已预扣金额保留（不错误退款）")
	assert.Equal(t, int64(1), getDebtCount(t, task.TaskID), "只生成一条欠款")
	assert.True(t, isUserDebtFrozen(t, userID), "欠款用户必须冻结")

	debt, err := model.GetTaskBillingDebtByTaskId(task.TaskID)
	require.NoError(t, err)
	assert.Equal(t, model.DebtStatusPending, debt.Status)
	assert.Equal(t, int64(preConsumed), debt.PreConsumedQuota)
	assert.Equal(t, int64(actual), debt.ActualQuota)
	assert.Equal(t, int64(actual-preConsumed), debt.DeltaQuota)
	assert.Equal(t, task.GetUpstreamTaskID(), debt.UpstreamTaskId, "欠款记录包含上游任务 ID")
	assert.Equal(t, "doubao-seedance-2-0-260128", debt.ModelName)
	assert.False(t, debt.AlertSent, "告警发送成功前保持 false（可重试）")
}

// 场景4：重复结算/恢复/轮询 → 同一任务只能一笔欠款，不重复冻结/累计。
func TestSeedanceSettle_IdempotentDebtNoDuplicate(t *testing.T) {
	truncate(t)
	ctx := context.Background()
	const userID, tokenID, channelID = 4004, 4004, 4004
	seedSeedanceUser(t, userID, 100, common.RoleCommonUser)
	seedToken(t, tokenID, userID, "sk-seed-idem", 100000)
	seedChannel(t, channelID)
	task := seedSeedanceTask(t, userID, channelID, tokenID, 1000)

	adaptor := &mockAdaptor{adjustReturn: 2500}
	taskResult := &relaycommon.TaskInfo{Status: model.TaskStatusSuccess}
	require.Equal(t, SeedanceSettleDebtCreated, SettleSeedanceTaskBilling(ctx, adaptor, task, taskResult))
	assert.Equal(t, int64(1), getDebtCount(t, task.TaskID))
	assert.True(t, isUserDebtFrozen(t, userID))

	// 模拟重复轮询/恢复：同一任务再次结算 → 幂等命中，不重复创建/冻结
	require.Equal(t, SeedanceSettleDebtCreated, SettleSeedanceTaskBilling(ctx, adaptor, task, taskResult))
	assert.Equal(t, int64(1), getDebtCount(t, task.TaskID), "重复结算不得产生第二条欠款")
	assert.True(t, isUserDebtFrozen(t, userID), "重复结算不重复冻结（已冻结保持）")
	assert.Equal(t, int64(100), getUserQuota(t, userID), "重复结算不得重复扣费")
}

// 场景5：管理员账号欠款 → 记录欠款但绝不冻结，仍需最高级别告警标记。
func TestSeedanceSettle_AdminDebtNoFreeze(t *testing.T) {
	truncate(t)
	ctx := context.Background()
	const userID, tokenID, channelID = 4005, 4005, 4005
	seedSeedanceUser(t, userID, 50, common.RoleAdminUser)
	seedToken(t, tokenID, userID, "sk-seed-admin", 100000)
	seedChannel(t, channelID)
	task := seedSeedanceTask(t, userID, channelID, tokenID, 1000)

	adaptor := &mockAdaptor{adjustReturn: 3000}
	outcome := SettleSeedanceTaskBilling(ctx, adaptor, task, &relaycommon.TaskInfo{Status: model.TaskStatusSuccess})
	require.Equal(t, SeedanceSettleDebtCreated, outcome)
	assert.Equal(t, int64(1), getDebtCount(t, task.TaskID), "管理员欠款记录必须照建")
	assert.False(t, isUserDebtFrozen(t, userID), "管理员不得被欠款冻结")
	assert.Equal(t, common.RoleAdminUser, getUserRole(t, userID), "管理员角色不变")
}

func getUserRole(t *testing.T, id int) int {
	t.Helper()
	var user model.User
	require.NoError(t, model.DB.Select("role").Where("id = ?", id).First(&user).Error)
	return user.Role
}

// 场景6：数据库错误 → Retryable（调用方回退非终态重试），不落欠款。
func TestSeedanceSettle_DBErrorRetryable(t *testing.T) {
	truncate(t)
	ctx := context.Background()
	const userID, tokenID, channelID = 4006, 4006, 4006
	seedSeedanceUser(t, userID, 10000, common.RoleCommonUser)
	seedToken(t, tokenID, userID, "sk-seed-dberr", 100000)
	seedChannel(t, channelID)
	task := seedSeedanceTask(t, userID, channelID, tokenID, 1000)

	// 注入任务额度 UPDATE 失败
	require.NoError(t, model.DB.Exec(`CREATE TRIGGER fail_seedance_settle
		BEFORE UPDATE OF quota ON tasks
		BEGIN
			SELECT RAISE(ABORT, 'injected settle failure');
		END`).Error)
	t.Cleanup(func() { _ = model.DB.Exec("DROP TRIGGER IF EXISTS fail_seedance_settle").Error })

	adaptor := &mockAdaptor{adjustReturn: 2500}
	outcome := SettleSeedanceTaskBilling(ctx, adaptor, task, &relaycommon.TaskInfo{Status: model.TaskStatusSuccess})
	require.Equal(t, SeedanceSettleRetryable, outcome, "数据库错误必须返回 Retryable")
	assert.Equal(t, int64(0), getDebtCount(t, task.TaskID), "DB 错误不落欠款（避免伪造已处理）")
	assert.False(t, isUserDebtFrozen(t, userID), "DB 错误不冻结")
	assert.Equal(t, int64(1000), task.Quota, "内存任务额度不被污染")
}

// 场景7：欠款任务进入终态后并发名额只释放一次（MarkTaskSlotReleasedAndDecrement 幂等）。
func TestSeedanceSettle_DebtTaskSlotReleasedOnce(t *testing.T) {
	truncate(t)
	ctx := context.Background()
	const userID, tokenID, channelID = 4007, 4007, 4007
	seedSeedanceUser(t, userID, 100, common.RoleCommonUser)
	seedToken(t, tokenID, userID, "sk-seed-slot", 100000)
	seedChannel(t, channelID)

	// 预占并发名额
	limit := 3
	reserved, current, err := model.ReserveTaskConcurrencySlot(userID, limit)
	require.NoError(t, err)
	require.True(t, reserved)
	assert.Equal(t, 1, current)

	task := seedSeedanceTask(t, userID, channelID, tokenID, 1000)
	task.Status = model.TaskStatusSuccess // 欠款任务进入终态（上游生命周期结束）
	require.NoError(t, task.Update())

	adaptor := &mockAdaptor{adjustReturn: 2500}
	require.Equal(t, SeedanceSettleDebtCreated, SettleSeedanceTaskBilling(ctx, adaptor, task, &relaycommon.TaskInfo{Status: model.TaskStatusSuccess}))

	// 终态确立 + 欠款闭环：释放名额（调用方在 updateVideoSingleTask 中执行）
	ReleaseTaskSlotIfSeedance(task)
	count, err := model.GetRunningCountForUser(userID)
	require.NoError(t, err)
	assert.Equal(t, 0, count, "欠款任务进入终态后必须释放并发名额")

	// 重复释放（其他终态路径并发）→ 幂等 no-op，计数不变
	ReleaseTaskSlotIfSeedance(task)
	count, err = model.GetRunningCountForUser(userID)
	require.NoError(t, err)
	assert.Equal(t, 0, count, "并发名额只释放一次")
}

// 场景8：同一用户多个 Seedance 任务并发结算，任意交错顺序下余额 >= 0。
func TestSeedanceSettle_ConcurrentMultiTaskNeverNegative(t *testing.T) {
	truncate(t)
	ctx := context.Background()
	const userID, tokenID, channelID = 4008, 4008, 4008
	const balance, preConsumed, actual = 2400, 1000, 1800 // 差额 800，余额够 3 个
	seedSeedanceUser(t, userID, balance, common.RoleCommonUser)
	seedToken(t, tokenID, userID, "sk-seed-conc", 100000)
	seedChannel(t, channelID)

	const numTasks = 5
	tasks := make([]*model.Task, numTasks)
	for i := 0; i < numTasks; i++ {
		tasks[i] = seedSeedanceTask(t, userID, channelID, tokenID, preConsumed)
	}
	adaptor := &mockAdaptor{adjustReturn: actual}
	taskResult := &relaycommon.TaskInfo{Status: model.TaskStatusSuccess}

	done := make(chan struct{})
	timeout := time.After(60 * time.Second)
	var wg sync.WaitGroup
	for i := 0; i < numTasks; i++ {
		wg.Add(1)
		go func(task *model.Task) {
			defer wg.Done()
			SettleSeedanceTaskBilling(ctx, adaptor, task, taskResult)
		}(tasks[i])
	}
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-timeout:
		t.Fatal("并发结算超时")
	}

	quota := getUserQuota(t, userID)
	assert.GreaterOrEqual(t, quota, int64(0), "并发结算任意交错下余额必须 >= 0")
	assert.Equal(t, int64(balance-3*800), quota, "成功收款总额不超过可用余额（3 个差额）")
	assert.True(t, isUserDebtFrozen(t, userID), "存在未清欠款时用户保持冻结")

	// 欠款总数 = 5 - 成功收款数
	assert.Equal(t, int64(2), getTotalDebtCount(t, userID), "被拒绝的任务各产生一条欠款")
}

func getTotalDebtCount(t *testing.T, userID int) int64 {
	t.Helper()
	var count int64
	model.DB.Model(&model.TaskBillingDebt{}).Where("user_id = ? AND status = ?", userID, model.DebtStatusPending).Count(&count)
	return count
}

// 场景9：订阅资金来源：订阅超额 → 欠款闭环；订阅充足 → 正常结算。
func TestSeedanceSettle_SubscriptionFunding(t *testing.T) {
	truncate(t)
	ctx := context.Background()
	const userID, tokenID, channelID, subID = 4009, 4009, 4009, 9
	seedSeedanceUser(t, userID, 0, common.RoleCommonUser)
	seedToken(t, tokenID, userID, "sk-seed-sub", 100000)
	seedChannel(t, channelID)
	seedSubscription(t, subID, userID, 10000, 9500) // 只剩 500 额度
	task := seedSeedanceTask(t, userID, channelID, tokenID, 1000)
	task.PrivateData.SubscriptionId = subID
	task.PrivateData.BillingSource = BillingSourceSubscription
	require.NoError(t, task.Update())

	// 差额 500 恰好等于剩余额度 → 成功
	adaptor := &mockAdaptor{adjustReturn: 1500}
	require.Equal(t, SeedanceSettleSuccess, SettleSeedanceTaskBilling(ctx, adaptor, task, &relaycommon.TaskInfo{Status: model.TaskStatusSuccess}))
	assert.Equal(t, int64(10000), getSubscriptionUsed(t, subID), "订阅已用量达到总额度上限")

	// 差额超过剩余额度 → 欠款闭环（订阅守卫拒绝）
	task2 := seedSeedanceTask(t, userID, channelID, tokenID, 1000)
	task2.PrivateData.SubscriptionId = subID
	task2.PrivateData.BillingSource = BillingSourceSubscription
	require.NoError(t, task2.Update())
	adaptor2 := &mockAdaptor{adjustReturn: 2500} // 差额 1500 > 剩余 0
	require.Equal(t, SeedanceSettleDebtCreated, SettleSeedanceTaskBilling(ctx, adaptor2, task2, &relaycommon.TaskInfo{Status: model.TaskStatusSuccess}))
	assert.Equal(t, int64(1), getDebtCount(t, task2.TaskID), "订阅超额必须进入欠款闭环")
	assert.Equal(t, int64(10000), getSubscriptionUsed(t, subID), "订阅已用量不得超过总额度")
}

// 场景10：清偿一笔欠款但仍有其他欠款 → 不解冻；全部清偿后才解除。
func TestSeedanceSettle_RepayPartialKeepsFrozen(t *testing.T) {
	truncate(t)
	const userID = 4010
	seedSeedanceUser(t, userID, 5000, common.RoleCommonUser)
	seedDebtTaskForService(t, userID, "task-repay-a", 1000)
	seedDebtTaskForService(t, userID, "task-repay-b", 1000)

	_, _, _, err := model.CreateDebtAndFreeze(model.DebtInput{UserId: userID, TaskId: "task-repay-a", PreConsumedQuota: 1000, ActualQuota: 1500, DeltaQuota: 500})
	require.NoError(t, err)
	_, _, _, err = model.CreateDebtAndFreeze(model.DebtInput{UserId: userID, TaskId: "task-repay-b", PreConsumedQuota: 1000, ActualQuota: 1500, DeltaQuota: 500})
	require.NoError(t, err)

	debtA, err := model.GetTaskBillingDebtByTaskId("task-repay-a")
	require.NoError(t, err)
	require.NoError(t, model.RepayTaskBillingDebt(userID, debtA.ID, model.RepayDebtOptions{}, 0))
	assert.True(t, isUserDebtFrozen(t, userID), "仍有其他欠款时不得解除冻结")

	debtB, err := model.GetTaskBillingDebtByTaskId("task-repay-b")
	require.NoError(t, err)
	require.NoError(t, model.RepayTaskBillingDebt(userID, debtB.ID, model.RepayDebtOptions{}, 0))
	assert.False(t, isUserDebtFrozen(t, userID), "全部清偿后才解除欠款冻结")
}

// 场景11：管理员手工禁用状态不会被清偿流程误解除。
func TestSeedanceSettle_RepayNeverUnbansAdminDisabled(t *testing.T) {
	truncate(t)
	const userID = 4011
	// 用户被欠款冻结 + 管理员手工禁用
	u := &model.User{Id: userID, Username: "seed_disabled", Quota: 2000, Status: common.UserStatusDisabled, Role: common.RoleCommonUser, AffCode: "aff-seed-disabled", DebtFrozen: true}
	require.NoError(t, model.DB.Create(u).Error)
	seedDebtTaskForService(t, userID, "task-repay-disabled", 1000)

	_, _, _, err := model.CreateDebtAndFreeze(model.DebtInput{UserId: userID, TaskId: "task-repay-disabled", PreConsumedQuota: 1000, ActualQuota: 1500, DeltaQuota: 500})
	require.NoError(t, err)
	debt, err := model.GetTaskBillingDebtByTaskId("task-repay-disabled")
	require.NoError(t, err)
	require.NoError(t, model.RepayTaskBillingDebt(userID, debt.ID, model.RepayDebtOptions{}, 0))

	var user model.User
	require.NoError(t, model.DB.First(&user, userID).Error)
	assert.False(t, user.DebtFrozen, "欠款冻结解除")
	assert.Equal(t, common.UserStatusDisabled, user.Status, "管理员手工禁用状态绝不能被清偿流程误解除")
}

// seedDebtTaskForService 创建欠款清偿所需的关联任务行。
func seedDebtTaskForService(t *testing.T, userID int, taskID string, preConsumed int64) {
	t.Helper()
	task := &model.Task{
		TaskID:    taskID,
		UserId:    userID,
		Quota:     preConsumed,
		Status:    model.TaskStatusInProgress,
		Group:     "default",
		Data:      json.RawMessage(`{}`),
		CreatedAt: time.Now().Unix(),
		UpdatedAt: time.Now().Unix(),
		PrivateData: model.TaskPrivateData{
			BillingSource:      "wallet",
			ConsumeLogRecorded: true,
		},
	}
	require.NoError(t, model.DB.Create(task).Error)
}

// 场景12：非 Seedance 的既有欠费兼容路径不受影响（默认无守卫语义）。
func TestSeedanceSettle_NonSeedanceCompatUnaffected(t *testing.T) {
	truncate(t)
	ctx := context.Background()
	const userID, tokenID, channelID = 4012, 4012, 4012
	seedSeedanceUser(t, userID, 100, common.RoleCommonUser)
	seedToken(t, tokenID, userID, "sk-non-seedance", 100000)
	seedChannel(t, channelID)

	// 普通任务（非 Seedance 平台）：差额结算仍走通用 RecalculateTaskQuota（允许欠费语义）
	task := &model.Task{
		TaskID:    "task-non-seedance",
		UserId:    userID,
		ChannelId: channelID,
		Quota:     1000,
		Status:    model.TaskStatusInProgress,
		Group:     "default",
		Platform:  constant.TaskPlatform("1"), // 非 Seedance
		Data:      json.RawMessage(`{}`),
		CreatedAt: time.Now().Unix(),
		UpdatedAt: time.Now().Unix(),
		PrivateData: model.TaskPrivateData{
			BillingSource:      "wallet",
			TokenId:            tokenID,
			ConsumeLogRecorded: true,
		},
	}
	require.NoError(t, model.DB.Create(task).Error)

	// 通用路径差额补扣：余额 100 < 差额 500 → 仍按旧语义扣成 -400（兼容分层计费）
	// 注意：这是通用 RecalculateTaskQuota 的既有行为，不是 Seedance 路径。
	assert.True(t, RecalculateTaskQuota(ctx, task, 1500, "adaptor计费调整", 0))
	assert.Equal(t, int64(100-500), getUserQuota(t, userID), "非 Seedance 通用路径保持既有欠费兼容语义")
	assert.Equal(t, int64(0), getDebtCount(t, task.TaskID), "非 Seedance 路径不产生欠款记录")
}

// 场景13：钱包与订阅两种资金来源的预扣守卫在 PreConsumeBilling 冻结检查可用。
func TestPreConsumeBilling_DebtFrozenUserRejected(t *testing.T) {
	truncate(t)
	const userID = 4013
	u := &model.User{Id: userID, Username: "frozen_user", Quota: 10000, Status: common.UserStatusEnabled, Role: common.RoleCommonUser, AffCode: "aff-frozen", DebtFrozen: true}
	require.NoError(t, model.DB.Create(u).Error)

	info := &relaycommon.RelayInfo{
		UserId:      userID,
		UsingGroup:  "default",
		UserSetting: dto.UserSetting{},
	}
	c := testGinContext("frozen_user")
	apiErr := PreConsumeBilling(c, 1000, info)
	require.NotNil(t, apiErr, "欠款冻结用户必须被预扣入口拒绝")
	assert.Equal(t, relaykittypes.ErrorCodeDebtFrozen, apiErr.GetErrorCode(), "必须返回可识别的欠款冻结错误码")
	assert.Nil(t, info.Billing, "冻结用户不得创建计费会话")
}

// 场景14：事务中途失败时资金/任务/欠款/冻结全部回滚（service 层整体视角）。
func TestSeedanceSettle_TransactionAllRolledBack(t *testing.T) {
	truncate(t)
	ctx := context.Background()
	const userID, tokenID, channelID = 4014, 4014, 4014
	seedSeedanceUser(t, userID, 100, common.RoleCommonUser)
	seedToken(t, tokenID, userID, "sk-seed-rollback-all", 100000)
	seedChannel(t, channelID)
	task := seedSeedanceTask(t, userID, channelID, tokenID, 1000)

	// 注入欠款创建失败（冻结 UPDATE 触发器）→ 结算整体 Retryable
	require.NoError(t, model.DB.Exec(`CREATE TRIGGER fail_debt_freezing
		BEFORE UPDATE OF debt_frozen ON users
		BEGIN
			SELECT RAISE(ABORT, 'injected freeze failure');
		END`).Error)
	t.Cleanup(func() { _ = model.DB.Exec("DROP TRIGGER IF EXISTS fail_debt_freezing").Error })

	adaptor := &mockAdaptor{adjustReturn: 2500}
	outcome := SettleSeedanceTaskBilling(ctx, adaptor, task, &relaycommon.TaskInfo{Status: model.TaskStatusSuccess})
	require.Equal(t, SeedanceSettleRetryable, outcome, "欠款/冻结任一步失败必须整体回滚为可重试")
	assert.Equal(t, int64(0), getDebtCount(t, task.TaskID), "回滚：欠款不落库")
	assert.False(t, isUserDebtFrozen(t, userID), "回滚：冻结不落库")
	assert.Equal(t, int64(100), getUserQuota(t, userID), "回滚：资金不变")
	assert.Equal(t, int64(1000), task.Quota, "回滚：任务额度不变")
}

// 场景15：保守预扣(高) → 完成按真实 usage 结算多退少补（端到端：提交估算 ratios
// 写入 BillingContext，轮询 token 重算使用同一 otherMultiplier，两者乘积 >= 1）。
func TestSeedanceSettle_ConservativePreConsumeThenRefund(t *testing.T) {
	truncate(t)
	ctx := context.Background()
	const userID, tokenID, channelID = 4015, 4015, 4015
	seedSeedanceUser(t, userID, 100000, common.RoleCommonUser)
	seedToken(t, tokenID, userID, "sk-seed-conservative", 100000)
	seedChannel(t, channelID)

	// 模拟提交：10 秒 1080p 任务 → 保守预扣 = 基础 × seconds(2) × size(51/46)
	priceData := types.PriceData{}
	priceData.AddOtherRatio("seconds", 2.0)
	priceData.AddOtherRatio("size", 51.0/46.0)
	priceData.AddOtherRatio("video_input", 1.0)
	preConsumed, clamp := common.QuotaFromFloatChecked(10000 * priceData.OtherRatioMultiplier())
	require.Nil(t, clamp)
	require.Greater(t, preConsumed, int64(10000), "保守预扣必须大于基础价")

	task := seedSeedanceTask(t, userID, channelID, tokenID, preConsumed)
	task.PrivateData.BillingContext.OtherRatios = priceData.OtherRatios()
	require.NoError(t, task.Update())
	seedUsedQuota(t, userID, channelID, preConsumed)

	// 实际费用低于预扣（真实 usage 结算）→ 多退少补
	actual := int64(8000)
	adaptor := &mockAdaptor{adjustReturn: actual}
	require.Equal(t, SeedanceSettleSuccess, SettleSeedanceTaskBilling(ctx, adaptor, task, &relaycommon.TaskInfo{Status: model.TaskStatusSuccess}))
	// seedSeedanceUser 的余额语义是"预扣后余额"（与 task_billing_test 的 seedUser 一致）：
	// 退款方向把多预扣部分加回余额。
	assert.Equal(t, 100000+(preConsumed-actual), getUserQuota(t, userID), "多预扣部分退款")
	assert.Equal(t, actual, task.Quota, "任务额度收敛到实际费用")
	assert.Equal(t, int64(0), getDebtCount(t, task.TaskID))
}

// ---------------------------------------------------------------------------
// 缺陷三补充：成功差额补扣的资金/Token/统计/日志全量一致性
// ---------------------------------------------------------------------------

// 场景16：余额恰好够差额时，钱包、Token、task.Quota、用户 used_quota、
// 渠道 used_quota 与补扣日志**全部**增加同一差额（不允许任何一项缺失）。
func TestSeedanceSettle_FullConsistencyOnExactBalance(t *testing.T) {
	truncate(t)
	ctx := context.Background()
	const userID, tokenID, channelID = 4016, 4016, 4016
	const balance, preConsumed, actual = 500, 1000, 1500 // 差额 500
	seedSeedanceUser(t, userID, balance, common.RoleCommonUser)
	seedToken(t, tokenID, userID, "sk-seed-consistency", 100000)
	seedChannel(t, channelID)
	seedUsedQuota(t, userID, channelID, preConsumed) // 预扣已计入累计消耗
	task := seedSeedanceTask(t, userID, channelID, tokenID, preConsumed)

	adaptor := &mockAdaptor{adjustReturn: actual}
	outcome := SettleSeedanceTaskBilling(ctx, adaptor, task, &relaycommon.TaskInfo{Status: model.TaskStatusSuccess})
	require.Equal(t, SeedanceSettleSuccess, outcome)

	delta := actual - preConsumed
	// 钱包：余额恰好够差额 → 0
	assert.Equal(t, int64(0), getUserQuota(t, userID), "钱包扣减差额后余额为 0")
	// Token：提交时已预扣 preConsumed，结算再扣差额 → remain = 初始 - 预扣 - 差额、
	// used = 预扣 + 差额，remain+used 不变量保持不变
	assert.Equal(t, int64(100000-preConsumed-delta), getTokenRemainQuota(t, tokenID), "Token remain 扣减同一差额")
	assert.Equal(t, int64(preConsumed+delta), getTokenUsedQuota(t, tokenID), "Token used 增加同一差额")
	assert.Equal(t, int64(100000), getTokenRemainQuota(t, tokenID)+getTokenUsedQuota(t, tokenID), "remain+used 资金不变量")
	// task.Quota：收敛到实际额度
	assert.Equal(t, int64(actual), task.Quota, "任务额度收敛到实际费用")
	// 累计消耗：用户与渠道 used_quota 各增加同一差额
	assert.Equal(t, int64(preConsumed+delta), getUserUsedQuota(t, userID), "用户累计消耗增加同一差额")
	assert.EqualValues(t, preConsumed+delta, getChannelUsedQuota(t, channelID), "渠道累计消耗增加同一差额")
	// 补扣日志：一条 LogTypeConsume，Quota=差额
	require.Equal(t, int64(1), countLogs(t), "成功补扣必须恰好一条差额日志")
	last := getLastLog(t)
	require.NotNil(t, last)
	assert.Equal(t, model.LogTypeConsume, last.Type)
	assert.Equal(t, int64(delta), last.Quota, "日志额度等于差额")
	var other map[string]interface{}
	require.NoError(t, json.Unmarshal([]byte(last.Other), &other))
	assert.Equal(t, task.TaskID, other["task_id"], "日志携带任务 ID")
}

// 场景17：重复结算（同一任务同一实际额度）不得重复扣 Token、不得重复写日志。
func TestSeedanceSettle_RepeatSettleNoDoubleTokenOrLog(t *testing.T) {
	truncate(t)
	ctx := context.Background()
	const userID, tokenID, channelID = 4017, 4017, 4017
	const balance, preConsumed, actual = 10000, 1000, 1500
	seedSeedanceUser(t, userID, balance, common.RoleCommonUser)
	seedToken(t, tokenID, userID, "sk-seed-repeat", 100000)
	seedChannel(t, channelID)
	seedUsedQuota(t, userID, channelID, preConsumed)
	task := seedSeedanceTask(t, userID, channelID, tokenID, preConsumed)

	adaptor := &mockAdaptor{adjustReturn: actual}
	taskResult := &relaycommon.TaskInfo{Status: model.TaskStatusSuccess}
	require.Equal(t, SeedanceSettleSuccess, SettleSeedanceTaskBilling(ctx, adaptor, task, taskResult))

	delta := actual - preConsumed
	afterFirst := map[string]int64{
		"wallet": getUserQuota(t, userID),
		"token":  getTokenUsedQuota(t, tokenID),
		"logs":   int64(countLogs(t)),
		"used":   getUserUsedQuota(t, userID),
	}
	assert.Equal(t, int64(preConsumed+delta), afterFirst["token"], "首次结算 Token used = 预扣 + 差额")

	// 重复结算：task.Quota 已是 actual → delta=0 → 直接返回，不做任何调整
	require.Equal(t, SeedanceSettleSuccess, SettleSeedanceTaskBilling(ctx, adaptor, task, taskResult))
	assert.Equal(t, afterFirst["wallet"], getUserQuota(t, userID), "重复结算不得重复扣钱包")
	assert.Equal(t, afterFirst["token"], getTokenUsedQuota(t, tokenID), "重复结算不得重复扣 Token")
	assert.Equal(t, afterFirst["used"], getUserUsedQuota(t, userID), "重复结算不得重复累加 used_quota")
	assert.Equal(t, int64(countLogs(t)), int64(afterFirst["logs"]), "重复结算不得重复写差额日志")
}

// 场景18：Token 扣减失败（余额不足）→ 资金已收但待补偿标记**与资金同一事务**
// 落库（task.token_delta_pending > 0），后台补偿可幂等补齐——证明不存在
// "资金已提交但 pending 没有落库"的状态（问题二崩溃窗口已消除）。
func TestSeedanceSettle_TokenShortPersistsPendingRecovery(t *testing.T) {
	truncate(t)
	ctx := context.Background()
	const userID, tokenID, channelID = 4018, 4018, 4018
	const balance, preConsumed, actual = 10000, 1000, 1500 // 差额 500
	// Token 初始 1400：fixture 预扣 1000 后剩 400 < 差额 500 → 结算扣减被守卫拒绝
	seedSeedanceUser(t, userID, balance, common.RoleCommonUser)
	seedToken(t, tokenID, userID, "sk-seed-tokshort", 1400)
	seedChannel(t, channelID)
	seedUsedQuota(t, userID, channelID, preConsumed)
	task := seedSeedanceTask(t, userID, channelID, tokenID, preConsumed)

	adaptor := &mockAdaptor{adjustReturn: actual}
	outcome := SettleSeedanceTaskBilling(ctx, adaptor, task, &relaycommon.TaskInfo{Status: model.TaskStatusSuccess})
	require.Equal(t, SeedanceSettleSuccess, outcome, "Token 不足不阻断资金结算")

	// 资金已收：钱包扣了差额
	assert.Equal(t, int64(balance-500), getUserQuota(t, userID), "资金照常收取")
	// Token 未扣：remain 保持 400（fixture 预扣后），used 保持预扣额 1000
	assert.Equal(t, int64(1400-preConsumed), getTokenRemainQuota(t, tokenID), "Token 余额不足不扣减")
	assert.Equal(t, int64(preConsumed), getTokenUsedQuota(t, tokenID), "Token used 保持预扣额")
	// 恢复信息不丢失：token_delta_pending 与资金同一事务持久化到 DB
	require.Equal(t, int64(500), task.TokenDeltaPending, "内存标记待补偿差额（事务提交后同步）")
	var reloaded model.Task
	require.NoError(t, model.DB.Select("token_delta_pending").First(&reloaded, task.ID).Error)
	assert.Equal(t, int64(500), reloaded.TokenDeltaPending, "待补偿差额必须随资金事务落库（资金已收 Token 未扣可审计可补偿）")

	// 给 Token 充值后后台补偿：幂等扣减 + 清零标记。
	// IncreaseTokenQuota(+1000) 语义：remain +1000、used -1000 →
	// remain = 400+1000 = 1400、used = 1000-1000 = 0（remain+used 不变量保持）。
	require.NoError(t, model.IncreaseTokenQuota(tokenID, "", 1000))
	compensated, err := model.CompensatePendingTokenDeltas(100)
	require.NoError(t, err)
	assert.Equal(t, 1, compensated, "后台补偿成功 1 条")
	assert.Equal(t, int64(1400-500), getTokenRemainQuota(t, tokenID), "补偿后 Token 扣减差额")
	assert.Equal(t, int64(0+500), getTokenUsedQuota(t, tokenID), "补偿后 Token used 增加差额")
	assert.Equal(t, int64(1400), getTokenRemainQuota(t, tokenID)+getTokenUsedQuota(t, tokenID), "remain+used 资金不变量")
	require.NoError(t, model.DB.Select("token_delta_pending").First(&reloaded, task.ID).Error)
	assert.Equal(t, int64(0), reloaded.TokenDeltaPending, "补偿成功后标记清零")

	// 再次补偿：无待补偿记录 → no-op，不重复扣款
	compensated, err = model.CompensatePendingTokenDeltas(100)
	require.NoError(t, err)
	assert.Equal(t, 0, compensated)
	assert.Equal(t, int64(1400-500), getTokenRemainQuota(t, tokenID), "重复补偿不重复扣款")
}

// 场景18b：故障注入——pending 落库失败必须导致**整笔事务回滚**（资金不提交），
// 证明不存在"资金已提交但 pending 没有落库"的中间状态。旧实现是"资金事务先提交、
// 第二个事务写 pending"：pending 写失败时资金已收而恢复信息丢失（崩溃窗口）。
// 新实现 pending 与资金同一事务：注入 pending UPDATE 失败 → 资金/任务额度/Token
// 全部回滚，结算返回 Retryable，无任何部分写入。
func TestSeedanceSettle_PendingWriteFailureRollsBackFunds(t *testing.T) {
	truncate(t)
	ctx := context.Background()
	const userID, tokenID, channelID = 40181, 40181, 40181
	const balance, preConsumed, actual = 10000, 1000, 1500 // 差额 500
	seedSeedanceUser(t, userID, balance, common.RoleCommonUser)
	seedToken(t, tokenID, userID, "sk-seed-pendfail", 1400) // 预扣后 400 < 500，触发 pending 路径
	seedChannel(t, channelID)
	seedUsedQuota(t, userID, channelID, preConsumed)
	task := seedSeedanceTask(t, userID, channelID, tokenID, preConsumed)

	// 注入 token_delta_pending 写入失败（模拟"第二笔写入"崩溃窗口）
	require.NoError(t, model.DB.Exec(`CREATE TRIGGER fail_seedance_pending_write
		BEFORE UPDATE OF token_delta_pending ON tasks
		BEGIN
			SELECT RAISE(ABORT, 'injected pending write failure');
		END`).Error)
	t.Cleanup(func() { _ = model.DB.Exec("DROP TRIGGER IF EXISTS fail_seedance_pending_write").Error })

	adaptor := &mockAdaptor{adjustReturn: actual}
	outcome := SettleSeedanceTaskBilling(ctx, adaptor, task, &relaycommon.TaskInfo{Status: model.TaskStatusSuccess})
	require.Equal(t, SeedanceSettleRetryable, outcome, "pending 落库失败必须整体回滚为可重试（资金不得先提交）")

	// 资金/任务额度/Token 全部未变：不存在"资金已提交但 pending 缺失"的状态
	assert.Equal(t, int64(balance), getUserQuota(t, userID), "回滚：钱包不变")
	assert.Equal(t, int64(preConsumed), task.Quota, "回滚：任务额度不变")
	assert.Equal(t, int64(1400-preConsumed), getTokenRemainQuota(t, tokenID), "回滚：Token 不变")
	assert.Equal(t, int64(0), task.TokenDeltaPending, "回滚：内存 pending 不残留")
	var reloaded model.Task
	require.NoError(t, model.DB.Select("token_delta_pending").First(&reloaded, task.ID).Error)
	assert.Equal(t, int64(0), reloaded.TokenDeltaPending, "回滚：DB pending 不落库")

	// 移除注入后可重试成功（资金 + pending 一起提交）
	require.NoError(t, model.DB.Exec("DROP TRIGGER IF EXISTS fail_seedance_pending_write").Error)
	require.Equal(t, SeedanceSettleSuccess, SettleSeedanceTaskBilling(ctx, adaptor, task, &relaycommon.TaskInfo{Status: model.TaskStatusSuccess}))
	assert.Equal(t, int64(balance-500), getUserQuota(t, userID), "重试成功：钱包扣差额")
	require.NoError(t, model.DB.Select("token_delta_pending").First(&reloaded, task.ID).Error)
	assert.Equal(t, int64(500), reloaded.TokenDeltaPending, "重试成功：pending 与资金一起提交")
}

// ---------------------------------------------------------------------------
// 缺陷八补充：并发与恢复回归
// ---------------------------------------------------------------------------

// 场景20：同一任务并发结算（生产语义：先 CAS 状态迁移，只有赢家执行结算）——
// 钱包、Token、用户/渠道累计消耗与差额日志都只扣一次。
func TestSeedanceSettle_ConcurrentSameTaskChargesOnce(t *testing.T) {
	truncate(t)
	ctx := context.Background()
	const userID, tokenID, channelID = 4020, 4020, 4020
	seedSeedanceUser(t, userID, 10000, common.RoleCommonUser)
	seedToken(t, tokenID, userID, "sk-seed-sametask", 100000)
	seedChannel(t, channelID)
	seedUsedQuota(t, userID, channelID, 1000)
	task := seedSeedanceTask(t, userID, channelID, tokenID, 1000) // token used=1000

	adaptor := &mockAdaptor{adjustReturn: 1500}
	taskResult := &relaycommon.TaskInfo{Status: model.TaskStatusSuccess}
	// 与 production 一致：把任务状态置为 Success，再并发执行
	// "CAS 状态迁移（in_progress→success）→ 赢家结算"（每个 goroutine 独立拷贝）
	const workers = 4
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			taskCopy := *task
			taskCopy.Status = model.TaskStatusSuccess
			won, err := taskCopy.UpdateWithStatus(model.TaskStatus(model.TaskStatusInProgress))
			if err != nil || !won {
				return // CAS 输家不做任何计费（与 task_polling.go 一致）
			}
			SettleSeedanceTaskBilling(ctx, adaptor, &taskCopy, taskResult)
		}()
	}
	wg.Wait()

	// 只扣一次：钱包 -500、Token used 1000+500、任务额度 1500、累计消耗各 +500
	assert.Equal(t, int64(10000-500), getUserQuota(t, userID), "钱包只扣一次")
	assert.Equal(t, int64(1000+500), getTokenUsedQuota(t, tokenID), "Token 只扣一次")
	assert.Equal(t, int64(100000), getTokenRemainQuota(t, tokenID)+getTokenUsedQuota(t, tokenID), "remain+used 不变量")
	var settled model.Task
	require.NoError(t, model.DB.First(&settled, task.ID).Error)
	assert.Equal(t, int64(1500), settled.Quota, "任务额度收敛一次（DB 视角）")
	assert.Equal(t, int64(1000+500), getUserUsedQuota(t, userID), "用户累计消耗只加一次")
	assert.Equal(t, int64(1000+500), getChannelUsedQuota(t, channelID), "渠道累计消耗只加一次")
	assert.Equal(t, int64(1), countLogs(t), "差额日志只写一次")
	assert.Equal(t, int64(0), getDebtCount(t, task.TaskID), "并发结算不得产生欠款")
}

// 场景21：Token 不足 → 结算落 pending（task.Quota 已收敛）；充值后"后台补偿"
// 与"再次结算"并发：再次结算 delta=0（no-op），只有补偿扣一次 Token。
func TestSeedanceSettle_ConcurrentCompensateAndSettleNoDoubleTokenDeduct(t *testing.T) {
	truncate(t)
	ctx := context.Background()
	const userID, tokenID, channelID = 4021, 4021, 4021
	seedSeedanceUser(t, userID, 10000, common.RoleCommonUser)
	seedToken(t, tokenID, userID, "sk-seed-settle-comp", 1400) // 预扣 1000 后剩 400 < 差额 500
	seedChannel(t, channelID)
	seedUsedQuota(t, userID, channelID, 1000)
	task := seedSeedanceTask(t, userID, channelID, tokenID, 1000)

	adaptor := &mockAdaptor{adjustReturn: 1500}
	taskResult := &relaycommon.TaskInfo{Status: model.TaskStatusSuccess}
	require.Equal(t, SeedanceSettleSuccess, SettleSeedanceTaskBilling(ctx, adaptor, task, taskResult))
	require.Equal(t, int64(500), task.TokenDeltaPending, "结算落 pending")
	require.Equal(t, int64(1500), task.Quota, "任务额度已收敛 → 再次结算 delta=0")

	// 充值后并发：补偿 worker + 重复结算（delta=0 no-op）
	require.NoError(t, model.IncreaseTokenQuota(tokenID, "", 1000)) // remain 400+1000=1400、used 1000-1000=0
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		n, _ := model.CompensatePendingTokenDeltas(100)
		require.Equal(t, 1, n, "补偿 worker 成功扣减一次")
	}()
	go func() {
		defer wg.Done()
		SettleSeedanceTaskBilling(ctx, adaptor, task, taskResult) // delta=0 → no-op
	}()
	wg.Wait()

	// Token 只扣一次（补偿的 500）：remain = 1400-500 = 900、used = 0+500 = 500
	assert.Equal(t, int64(900), getTokenRemainQuota(t, tokenID), "Token 只扣一次（补偿路径）")
	assert.Equal(t, int64(500), getTokenUsedQuota(t, tokenID), "Token used 只加一次")
	var reloaded model.Task
	require.NoError(t, model.DB.Select("token_delta_pending").First(&reloaded, task.ID).Error)
	assert.Equal(t, int64(0), reloaded.TokenDeltaPending, "补偿后标记清零")
	assert.Equal(t, int64(10000-500), getUserQuota(t, userID), "钱包只扣一次（结算差额）")
}

// ---------------------------------------------------------------------------
// 缺陷五补充：告警投递 —— 未配置通知渠道绝不标记 AlertSent
// ---------------------------------------------------------------------------

// 场景19：Root 未配置任何通知渠道（无邮箱/webhook/bark/gotify）时，
// RetryPendingDebtAlerts 不得标记 AlertSent=true，claim 释放保留重试。
func TestRetryPendingDebtAlerts_NoNotifyChannelKeepsAlertUnsent(t *testing.T) {
	truncate(t)
	const rootID = 4019
	// Root 用户：无邮箱、无 NotificationEmail、无 webhook/bark/gotify
	root := &model.User{Id: rootID, Username: "root_no_notify", Email: "", Status: common.UserStatusEnabled, Role: common.RoleRootUser, AffCode: "aff-root-no-notify"}
	require.NoError(t, model.DB.Create(root).Error)

	// 一条待发送告警的欠款
	debt := &model.TaskBillingDebt{
		UserId:           rootID,
		TaskId:           "task-alert-nonotify",
		UpstreamTaskId:   "cgt-nonotify",
		ModelName:        "doubao-seedance-2-0-260128",
		ChannelId:        1,
		PreConsumedQuota: 1000,
		ActualQuota:      1500,
		DeltaQuota:       500,
		Reason:           "余额不足",
		Status:           model.DebtStatusPending,
		AlertSent:        false,
		CreatedAt:        time.Now().Unix(),
		UpdatedAt:        time.Now().Unix(),
	}
	require.NoError(t, model.DB.Create(debt).Error)

	sent := RetryPendingDebtAlerts()
	assert.Equal(t, 0, sent, "未配置通知渠道不得计为成功")

	var reloaded model.TaskBillingDebt
	require.NoError(t, model.DB.First(&reloaded, debt.ID).Error)
	assert.False(t, reloaded.AlertSent, "未真正投递绝不标记 AlertSent=true")
	assert.Zero(t, reloaded.AlertClaimAt, "发送失败必须释放 claim 供下轮重试")
}

// ---------------------------------------------------------------------------
// 问题六：钱包代偿的消费日志原因
// ---------------------------------------------------------------------------

// 订阅欠款在订阅额度不足时经钱包代偿清偿：消费日志必须明确记录
// "订阅不足，钱包代偿"，不得使用普通清偿原因。
func TestDebtCollectionLog_WalletOverflowReason(t *testing.T) {
	truncate(t)
	const userID, subID = 4022, 22
	seedSeedanceUser(t, userID, 2000, common.RoleCommonUser)
	seedSubscription(t, subID, userID, 1000, 900) // 剩余 100 < 差额 500

	task := &model.Task{
		TaskID:    "task-debt-overflow-log",
		UserId:    userID,
		Quota:     1000,
		Status:    model.TaskStatusInProgress,
		Group:     "default",
		Data:      json.RawMessage(`{}`),
		CreatedAt: time.Now().Unix(),
		UpdatedAt: time.Now().Unix(),
		PrivateData: model.TaskPrivateData{
			BillingSource:      BillingSourceSubscription,
			SubscriptionId:     subID,
			ConsumeLogRecorded: true,
		},
	}
	require.NoError(t, model.DB.Create(task).Error)

	_, _, _, err := model.CreateDebtAndFreeze(model.DebtInput{
		UserId: userID, TaskId: task.TaskID, PreConsumedQuota: 1000, ActualQuota: 1500, DeltaQuota: 500,
		BillingSource: BillingSourceSubscription, SubscriptionId: subID,
	})
	require.NoError(t, err)
	debt, err := model.GetTaskBillingDebtByTaskId(task.TaskID)
	require.NoError(t, err)

	// 未允许钱包代偿：订阅不足 → 拒绝（保留 pending，订阅已用量不变）
	err = model.RepayTaskBillingDebt(userID, debt.ID, model.RepayDebtOptions{}, 0)
	require.ErrorIs(t, err, model.ErrDebtSubscriptionInsufficient)
	assert.Equal(t, int64(900), getSubscriptionUsed(t, subID), "订阅不足时已用量不变")

	// 允许钱包代偿：钱包收款 + 日志明确记录"订阅不足，钱包代偿"
	require.NoError(t, model.RepayTaskBillingDebt(userID, debt.ID, model.RepayDebtOptions{AllowWalletOverflow: true}, 100))
	assert.Equal(t, int64(2000-500), getUserQuota(t, userID), "钱包代偿差额")
	assert.Equal(t, int64(900), getSubscriptionUsed(t, subID), "代偿不改变订阅已用量")

	var reloadedDebt model.TaskBillingDebt
	require.NoError(t, model.DB.First(&reloadedDebt, debt.ID).Error)
	assert.True(t, reloadedDebt.WalletOverflowed, "债务必须持久化资金来源切换标记")

	require.Equal(t, int64(1), countLogs(t), "清偿必须恰好一条差额日志")
	last := getLastLog(t)
	require.NotNil(t, last)
	assert.Equal(t, model.LogTypeConsume, last.Type)
	assert.Equal(t, int64(500), last.Quota, "日志额度等于差额")
	assert.Equal(t, "欠款清偿（订阅不足，钱包代偿）", last.Content, "消费日志必须明确记录钱包代偿原因（问题六）")
}

// ---------------------------------------------------------------------------
// 任务结算 token 回显：调整日志 other.total_tokens
// ---------------------------------------------------------------------------

// seedTaskLogRatioEnv 注册 token 重算所需的模型/分组倍率，并在测试结束后恢复。
func seedTaskLogRatioEnv(t *testing.T, modelName string, modelRatio float64) {
	t.Helper()
	originalModelRatios := ratio_setting.ModelRatio2JSONString()
	originalGroupRatios := ratio_setting.GroupRatio2JSONString()
	require.NoError(t, ratio_setting.UpdateModelRatioByJSONString(fmt.Sprintf(`{%q:%v}`, modelName, modelRatio)))
	require.NoError(t, ratio_setting.UpdateGroupRatioByJSONString(`{"default":1}`))
	t.Cleanup(func() {
		require.NoError(t, ratio_setting.UpdateModelRatioByJSONString(originalModelRatios))
		require.NoError(t, ratio_setting.UpdateGroupRatioByJSONString(originalGroupRatios))
	})
}

// getLastLogOtherMap 解析最后一条日志的 Other JSON。
func getLastLogOtherMap(t *testing.T) map[string]interface{} {
	t.Helper()
	last := getLastLog(t)
	require.NotNil(t, last)
	var other map[string]interface{}
	require.NoError(t, json.Unmarshal([]byte(last.Other), &other))
	return other
}

// 场景：Seedance token 重算补扣（实际 > 预扣）→ 调整日志必须携带上游
// usage.total_tokens（other.total_tokens），供前端 Tokens 列回显。
func TestSeedanceSettle_TotalTokensRecordedInChargeLog(t *testing.T) {
	truncate(t)
	ctx := context.Background()
	const userID, tokenID, channelID = 4030, 4030, 4030
	const preConsumed, totalTokens = 5000, 4000
	seedTaskLogRatioEnv(t, "doubao-seedance-2-0-260128", 3.15)
	seedSeedanceUser(t, userID, 1000000, common.RoleCommonUser)
	seedToken(t, tokenID, userID, "sk-seed-token-log", 1000000)
	seedChannel(t, channelID)
	seedUsedQuota(t, userID, channelID, preConsumed)
	task := seedSeedanceTask(t, userID, channelID, tokenID, preConsumed)

	// adaptor 不调整（adjustReturn=0）→ 走 taskResult.TotalTokens 重算分支
	adaptor := &mockAdaptor{}
	outcome := SettleSeedanceTaskBilling(ctx, adaptor, task, &relaycommon.TaskInfo{
		Status:      model.TaskStatusSuccess,
		TotalTokens: totalTokens,
	})
	require.Equal(t, SeedanceSettleSuccess, outcome)

	require.Equal(t, int64(1), countLogs(t), "补扣必须恰好一条差额日志")
	last := getLastLog(t)
	require.NotNil(t, last)
	assert.Equal(t, model.LogTypeConsume, last.Type, "实际高于预扣 → 补扣日志")
	other := getLastLogOtherMap(t)
	assert.Equal(t, float64(totalTokens), other["total_tokens"], "调整日志必须携带上游 total_tokens")
	assert.Equal(t, task.TaskID, other["task_id"], "日志携带任务 ID")
}

// 场景：Seedance token 重算退款（实际 < 预扣）→ 退款日志同样携带 total_tokens。
func TestSeedanceSettle_TotalTokensRecordedInRefundLog(t *testing.T) {
	truncate(t)
	ctx := context.Background()
	const userID, tokenID, channelID = 4031, 4031, 4031
	// actual = 3000×3.15 = 9450 < preConsumed 12600 → 退款 3150
	const preConsumed, totalTokens = 12600, 3000
	seedTaskLogRatioEnv(t, "doubao-seedance-2-0-260128", 3.15)
	seedSeedanceUser(t, userID, 100000, common.RoleCommonUser)
	seedToken(t, tokenID, userID, "sk-seed-token-refund", 100000)
	seedChannel(t, channelID)
	seedUsedQuota(t, userID, channelID, preConsumed)
	task := seedSeedanceTask(t, userID, channelID, tokenID, preConsumed)

	adaptor := &mockAdaptor{}
	outcome := SettleSeedanceTaskBilling(ctx, adaptor, task, &relaycommon.TaskInfo{
		Status:      model.TaskStatusSuccess,
		TotalTokens: totalTokens,
	})
	require.Equal(t, SeedanceSettleSuccess, outcome)

	last := getLastLog(t)
	require.NotNil(t, last)
	assert.Equal(t, model.LogTypeRefund, last.Type, "预扣高于实际 → 退款日志")
	other := getLastLogOtherMap(t)
	assert.Equal(t, float64(totalTokens), other["total_tokens"], "退款日志必须携带上游 total_tokens")
}

// 场景：通用 token 重算路径（RecalculateTaskQuotaByTokens）同样透传 total_tokens，
// 保证非 Seedance 平台（Suno 等）的 token 重算日志口径一致。
func TestRecalculateTaskQuotaByTokens_RecordsTotalTokens(t *testing.T) {
	truncate(t)
	ctx := context.Background()
	const userID, tokenID, channelID = 4032, 4032, 4032
	const preConsumed, totalTokens = 1000, 2000
	seedTaskLogRatioEnv(t, "test-model", 2.0)
	seedUser(t, userID, 100000)
	seedToken(t, tokenID, userID, "sk-generic-token-log", 100000)
	seedChannel(t, channelID)
	seedUsedQuota(t, userID, channelID, preConsumed)
	task := makeTask(userID, channelID, preConsumed, tokenID, BillingSourceWallet, 0)
	require.NoError(t, model.DB.Create(task).Error)

	assert.True(t, RecalculateTaskQuotaByTokens(ctx, task, totalTokens))

	last := getLastLog(t)
	require.NotNil(t, last)
	// actual = 2000×2.0×1.0 = 4000 > 1000 → 补扣
	assert.Equal(t, model.LogTypeConsume, last.Type)
	other := getLastLogOtherMap(t)
	assert.Equal(t, float64(totalTokens), other["total_tokens"], "通用 token 重算日志必须携带 total_tokens")
}

// 场景：adaptor 调整路径（无 token 语义）不写 total_tokens 字段，
// 且 taskResult 无 usage 时（TotalTokens=0）同样不写——字段缺失语义清晰。
func TestSeedanceSettle_NoTotalTokensWhenUsageAbsent(t *testing.T) {
	truncate(t)
	ctx := context.Background()
	const userID, tokenID, channelID = 4033, 4033, 4033
	const preConsumed, actual = 5000, 1000
	seedSeedanceUser(t, userID, 100000, common.RoleCommonUser)
	seedToken(t, tokenID, userID, "sk-seed-no-usage", 100000)
	seedChannel(t, channelID)
	seedUsedQuota(t, userID, channelID, preConsumed)
	task := seedSeedanceTask(t, userID, channelID, tokenID, preConsumed)

	adaptor := &mockAdaptor{adjustReturn: actual}
	require.Equal(t, SeedanceSettleSuccess, SettleSeedanceTaskBilling(ctx, adaptor, task,
		&relaycommon.TaskInfo{Status: model.TaskStatusSuccess}))

	last := getLastLog(t)
	require.NotNil(t, last)
	other := getLastLogOtherMap(t)
	assert.NotContains(t, other, "total_tokens", "上游未返回 usage 时不得伪造 token 字段")
}
