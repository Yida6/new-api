package model

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// ===========================================================================
// Seedance 结算的 Token 额度资金不变量（问题一/五/六定向回归）
// ===========================================================================

var tokenSettleTaskSeq int64

// seedTokenSettleTask 创建 Seedance 结算任务（带 Token），并模拟提交时的
// Token 预扣（remain -= preConsumed、used += preConsumed），使退款方向的
// used_quota >= abs(delta) 守卫在测试中与生产路径一致。
func seedTokenSettleTask(t *testing.T, userID, tokenID, channelID int, preConsumed int64) *Task {
	t.Helper()
	seq := atomic.AddInt64(&tokenSettleTaskSeq, 1)
	task := &Task{
		TaskID:    fmt.Sprintf("task-token-settle-%d-%d", userID, seq),
		UserId:    userID,
		ChannelId: channelID,
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
	if tokenID > 0 && preConsumed > 0 {
		res := DB.Model(&Token{}).Where("id = ?", tokenID).Updates(map[string]interface{}{
			"remain_quota": gorm.Expr("remain_quota - ?", preConsumed),
			"used_quota":   gorm.Expr("used_quota + ?", preConsumed),
		})
		require.NoError(t, res.Error)
		require.Equal(t, int64(1), res.RowsAffected, "fixture: token 必须存在")
	}
	return task
}

func tokenPending(t *testing.T, taskID int64) int {
	t.Helper()
	var pending int
	require.NoError(t, DB.Model(&Task{}).Where("id = ?", taskID).Pluck("token_delta_pending", &pending).Error)
	return pending
}

// 问题一：Token 退款保持 remain+used 资金不变量——remain 加回、used 冲减
// （绝不增加），且 remain+used 在扣减/退款前后保持不变。
func TestTokenSettle_RefundKeepsRemainUsedInvariant(t *testing.T) {
	truncateTables(t)
	const preConsumed, refund = 5000, 4000
	user := createReserveTestUser(t, 100000)
	tok := createReserveTestToken(t, 100000)
	require.NoError(t, DB.Model(&Token{}).Where("id = ?", tok.Id).Update("user_id", user.Id).Error)
	seedUsedQuotaForUser(t, user.Id, preConsumed)

	task := seedTokenSettleTask(t, user.Id, tok.Id, 0, preConsumed)
	// 提交后：remain = 95000、used = 5000（不变量：remain+used = 100000）
	before := getTokenFromDB(t, tok.Id)
	assert.Equal(t, int64(100000-preConsumed), before.RemainQuota)
	assert.Equal(t, int64(preConsumed), before.UsedQuota)
	assert.Equal(t, int64(100000), before.RemainQuota+before.UsedQuota, "扣减后 remain+used 不变量")

	// 结算退款 delta = -4000：remain 加回、used 冲减，总和不变
	res, tokenRes := ApplySeedanceSettle(task, -refund, false, TaskQuotaDeltaOptions{})
	require.Equal(t, TaskQuotaDeltaSuccess, res)
	require.Equal(t, TokenAdjustOK, tokenRes)

	after := getTokenFromDB(t, tok.Id)
	assert.Equal(t, int64(100000-preConsumed+refund), after.RemainQuota, "remain 加回 abs(delta)")
	assert.Equal(t, int64(preConsumed-refund), after.UsedQuota, "used 冲减 abs(delta)（绝不增加）")
	assert.Equal(t, int64(100000), after.RemainQuota+after.UsedQuota, "退款后 remain+used 不变量保持不变")
	// 钱包退款 + 任务额度收敛
	assert.Equal(t, int64(100000+refund), getUserQuotaFromDB(t, user.Id), "钱包加回退款额")
	assert.Equal(t, int64(preConsumed-refund), task.Quota, "任务额度收敛到实际费用")
	assert.Equal(t, 0, tokenPending(t, task.ID), "退款成功不得产生 pending")
}

// 问题一：退款守卫——used_quota 不足以冲减时整体回滚（绝不把 used 减成负数，
// 也不静默成功），返回可重试的 DB 错误。
func TestTokenSettle_RefundGuardNeverNegativeUsed(t *testing.T) {
	truncateTables(t)
	const preConsumed = 1000
	user := createReserveTestUser(t, 100000)
	tok := createReserveTestToken(t, 100000)
	require.NoError(t, DB.Model(&Token{}).Where("id = ?", tok.Id).Update("user_id", user.Id).Error)

	seedUsedQuotaForUser(t, user.Id, preConsumed)
	task := seedTokenSettleTask(t, user.Id, tok.Id, 0, preConsumed)
	// 人为把 used_quota 调低到 100（< 退款 800）——模拟数据异常
	require.NoError(t, DB.Model(&Token{}).Where("id = ?", tok.Id).Update("used_quota", 100).Error)

	// 退款 delta = -800：守卫失败 → 整笔回滚（钱包不退款、Token 不变）
	res, _ := ApplySeedanceSettle(task, -800, false, TaskQuotaDeltaOptions{})
	require.Equal(t, TaskQuotaDeltaDBError, res, "used_quota 不足必须回滚为可重试错误")
	reloaded := getTokenFromDB(t, tok.Id)
	assert.GreaterOrEqual(t, int64(reloaded.UsedQuota), int64(0), "used_quota 绝不减成负数")
	assert.Equal(t, int64(100), reloaded.UsedQuota, "回滚：Token 不变")
	assert.Equal(t, int64(100000), getUserQuotaFromDB(t, user.Id), "回滚：钱包不退款")
	assert.Equal(t, int64(preConsumed), task.Quota, "回滚：任务额度不变")
}

// 问题五：无限 Token 语义——跳过余额守卫但记录 remain/used 变化（与
// TryReserveTokenQuota 既有语义一致），结算补扣绝不因 remain < delta 进入
// pending，退款方向也不因 used_quota 不足而失败。
func TestTokenSettle_UnlimitedTokenNoPermanentPending(t *testing.T) {
	truncateTables(t)
	user := createReserveTestUser(t, 100000)
	tok := Token{
		UserId: user.Id, Key: "sk-unlimited-" + common.GetRandomString(8), Name: "unlimited",
		Status: common.TokenStatusEnabled, ExpiredTime: -1, RemainQuota: -1, UsedQuota: 0, UnlimitedQuota: true,
	}
	require.NoError(t, tok.Insert())

	// 补扣方向：无限令牌无条件扣减（remain 可继续为负），不得进入 pending
	task := seedTokenSettleTask(t, user.Id, tok.Id, 0, 1000) // remain = -1-1000 = -1001
	res, tokenRes := ApplySeedanceSettle(task, 500, false, TaskQuotaDeltaOptions{GuardPositiveDelta: true})
	require.Equal(t, TaskQuotaDeltaSuccess, res)
	require.Equal(t, TokenAdjustOK, tokenRes, "unlimited Token 补扣必须成功")
	assert.Equal(t, int64(-1001-500), getTokenFromDB(t, tok.Id).RemainQuota)
	assert.Equal(t, int64(1000+500), getTokenFromDB(t, tok.Id).UsedQuota)
	assert.Equal(t, 0, tokenPending(t, task.ID), "unlimited Token 不得因 remain < delta 永久进入 pending")

	// 退款方向：无条件加回（不设 used_quota 下限，与 IncreaseTokenQuota 一致）。
	// task2 fixture 预扣 1000 后再退款 300 → remain = -1501-1000+300 = -2201、
	// used = 1500+1000-300 = 2200；remain+used 不变量（初始 -1）保持。
	seedUsedQuotaForUser(t, user.Id, 1000)
	task2 := seedTokenSettleTask(t, user.Id, tok.Id, 0, 1000)
	res, tokenRes = ApplySeedanceSettle(task2, -300, false, TaskQuotaDeltaOptions{})
	require.Equal(t, TaskQuotaDeltaSuccess, res)
	require.Equal(t, TokenAdjustOK, tokenRes, "unlimited Token 退款必须成功")
	reloaded := getTokenFromDB(t, tok.Id)
	assert.Equal(t, int64(-1501-1000+300), reloaded.RemainQuota, "退款 remain 加回")
	assert.Equal(t, int64(1500+1000-300), reloaded.UsedQuota, "退款 used 冲减")
	assert.Equal(t, int64(-1), reloaded.RemainQuota+reloaded.UsedQuota, "remain+used 不变量（无限令牌初始 -1）")
	assert.Equal(t, 0, tokenPending(t, task2.ID))
}

// 问题五：unlimited Token 的欠款清偿路径——debt.TokenId 指向无限令牌时，
// 清偿无条件扣减成功（不会因 remain < delta 而 ErrDebtMissingToken）。
func TestTokenSettle_UnlimitedTokenDebtRepay(t *testing.T) {
	truncateTables(t)
	user := createReserveTestUser(t, 5000)
	tok := Token{
		UserId: user.Id, Key: "sk-unlimited-repay-" + common.GetRandomString(8), Name: "unlimited-repay",
		Status: common.TokenStatusEnabled, ExpiredTime: -1, RemainQuota: -1, UsedQuota: 0, UnlimitedQuota: true,
	}
	require.NoError(t, tok.Insert())
	seedDebtTask(t, user.Id, "task-debt-unlimited", 1000)

	_, _, _, err := CreateDebtAndFreeze(DebtInput{
		UserId: user.Id, TaskId: "task-debt-unlimited", PreConsumedQuota: 1000, ActualQuota: 1500, DeltaQuota: 500,
		TokenId: tok.Id, BillingSource: "wallet",
	})
	require.NoError(t, err)
	debt, err := GetTaskBillingDebtByTaskId("task-debt-unlimited")
	require.NoError(t, err)

	require.NoError(t, RepayTaskBillingDebt(user.Id, debt.ID, RepayDebtOptions{}, 0), "unlimited Token 清偿必须成功")
	reloaded := getTokenFromDB(t, tok.Id)
	assert.Equal(t, int64(-1-500), reloaded.RemainQuota, "unlimited Token 清偿无条件扣减")
	assert.Equal(t, int64(500), reloaded.UsedQuota)
	assert.Equal(t, int64(5000-500), getUserQuotaFromDB(t, user.Id), "钱包收款差额")
}

// 问题五：unlimited Token 的补偿路径——即便出现 pending（数据异常），补偿
// 也按 applyTokenQuotaDeltaTx 的 unlimited 语义无条件扣减并清零，不卡死。
func TestTokenSettle_UnlimitedTokenCompensate(t *testing.T) {
	truncateTables(t)
	user := createReserveTestUser(t, 10000)
	tok := Token{
		UserId: user.Id, Key: "sk-unlimited-comp-" + common.GetRandomString(8), Name: "unlimited-comp",
		Status: common.TokenStatusEnabled, ExpiredTime: -1, RemainQuota: -1, UsedQuota: 0, UnlimitedQuota: true,
	}
	require.NoError(t, tok.Insert())
	task := seedTokenSettleTask(t, user.Id, tok.Id, 0, 1000) // remain = -1001
	require.NoError(t, AddTaskTokenDeltaPending(task.ID, 500))

	compensated, err := CompensatePendingTokenDeltas(100)
	require.NoError(t, err)
	assert.Equal(t, 1, compensated, "unlimited Token 补偿必须成功")
	reloaded := getTokenFromDB(t, tok.Id)
	assert.Equal(t, int64(-1001-500), reloaded.RemainQuota, "补偿无条件扣减")
	assert.Equal(t, int64(1000+500), reloaded.UsedQuota)
	assert.Equal(t, 0, tokenPending(t, task.ID), "补偿后标记清零")
}

// ===========================================================================
// 并发与恢复（问题八）
// ===========================================================================

// 两个补偿 worker 并发处理同一 pending：只扣 Token 一次、清零一次。
func TestCompensatePendingTokenDeltas_ConcurrentWorkersDeductOnce(t *testing.T) {
	truncateTables(t)
	user := createReserveTestUser(t, 100000)
	tok := createReserveTestToken(t, 10000)
	require.NoError(t, DB.Model(&Token{}).Where("id = ?", tok.Id).Update("user_id", user.Id).Error)
	task := seedTokenSettleTask(t, user.Id, tok.Id, 0, 1000) // remain 9000、used 1000
	require.NoError(t, AddTaskTokenDeltaPending(task.ID, 500))

	var wg sync.WaitGroup
	results := make([]int, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i], _ = CompensatePendingTokenDeltas(100)
		}(i)
	}
	wg.Wait()

	assert.Equal(t, 1, results[0]+results[1], "两个补偿 worker 并发只允许一个成功（合计补偿 1 条）")
	reloaded := getTokenFromDB(t, tok.Id)
	assert.Equal(t, int64(9000-500), reloaded.RemainQuota, "Token 只扣一次")
	assert.Equal(t, int64(1000+500), reloaded.UsedQuota, "Token used 只加一次")
	assert.Equal(t, 0, tokenPending(t, task.ID), "标记只清零一次")

	// 再次补偿 no-op
	compensated, err := CompensatePendingTokenDeltas(100)
	require.NoError(t, err)
	assert.Equal(t, 0, compensated)
	assert.Equal(t, int64(9000-500), getTokenFromDB(t, tok.Id).RemainQuota, "重复补偿不重复扣款")
}

// Token 余额不足时，资金提交与 pending 原子出现（同一事务）：
// 结算成功 → 钱包扣款 + task.Quota 收敛 + token_delta_pending 落库三者同在。
func TestTokenSettle_ShortPendingAtomicWithFunds(t *testing.T) {
	truncateTables(t)
	const preConsumed = 1000
	user := createReserveTestUser(t, 10000)
	tok := createReserveTestToken(t, 600) // 预扣 1000 后 remain = -400 < 差额 500
	require.NoError(t, DB.Model(&Token{}).Where("id = ?", tok.Id).Update("user_id", user.Id).Error)
	task := seedTokenSettleTask(t, user.Id, tok.Id, 0, preConsumed)

	res, tokenRes := ApplySeedanceSettle(task, 500, false, TaskQuotaDeltaOptions{GuardPositiveDelta: true})
	require.Equal(t, TaskQuotaDeltaSuccess, res)
	require.Equal(t, TokenAdjustFailed, tokenRes, "Token 不足必须标记 Failed（pending 已随资金事务落库）")
	assert.Equal(t, int64(10000-500), getUserQuotaFromDB(t, user.Id), "资金已收")
	assert.Equal(t, int64(preConsumed+500), task.Quota, "任务额度收敛")
	assert.Equal(t, 500, tokenPending(t, task.ID), "pending 与资金同事务落库（无崩溃窗口）")
	assert.Equal(t, int64(500), task.TokenDeltaPending, "内存 pending 已同步")
}

// 同一任务并发结算（生产语义：先 CAS 状态迁移，只有赢家结算）——
// 钱包、Token、累计消费只扣一次。
func TestTokenSettle_ConcurrentSameTaskChargesOnce(t *testing.T) {
	truncateTables(t)
	user := createReserveTestUser(t, 10000)
	tok := createReserveTestToken(t, 100000)
	require.NoError(t, DB.Model(&Token{}).Where("id = ?", tok.Id).Update("user_id", user.Id).Error)
	task := seedTokenSettleTask(t, user.Id, tok.Id, 0, 1000) // token used=1000
	// 与 production 一致：先把任务状态置为 Success，再并发执行
	// "CAS 状态迁移（in_progress→success）→ 赢家结算"

	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			taskCopy := *task
			taskCopy.Status = TaskStatusSuccess
			won, err := taskCopy.UpdateWithStatus(TaskStatusInProgress)
			if err != nil || !won {
				return // CAS 输家不做任何计费（与 task_polling.go 一致）
			}
			// 结算结果通过最终 DB 状态断言（goroutine 内不得使用 require）
			ApplySeedanceSettle(&taskCopy, 500, false, TaskQuotaDeltaOptions{GuardPositiveDelta: true})
		}()
	}
	wg.Wait()

	// 只扣一次：钱包 -500、Token used 1000+500、任务额度 1500、无 pending
	assert.Equal(t, int64(10000-500), getUserQuotaFromDB(t, user.Id), "钱包只扣一次")
	tokReloaded := getTokenFromDB(t, tok.Id)
	assert.Equal(t, int64(100000), tokReloaded.RemainQuota+tokReloaded.UsedQuota, "remain+used 不变量")
	assert.Equal(t, int64(1000+500), tokReloaded.UsedQuota, "Token 只扣一次")
	require.NoError(t, DB.First(task, task.ID).Error)
	assert.Equal(t, int64(1000+500), task.Quota, "任务额度收敛一次")
	assert.Equal(t, 0, tokenPending(t, task.ID), "无待补偿")
}

// ===========================================================================
// Token Redis 缓存同步/失效（问题六）
// ===========================================================================

// 正常补扣/退款：事务提交后 Token 缓存增量同步，数据库与缓存不长期保留
// 不同额度。
func TestTokenSettle_CacheSyncedAfterCommit(t *testing.T) {
	truncateTables(t)
	useQuotaCacheMiniRedis(t)
	user := createReserveTestUser(t, 100000)
	tok := createReserveTestToken(t, 100000)
	require.NoError(t, DB.Model(&Token{}).Where("id = ?", tok.Id).Update("user_id", user.Id).Error)

	// 初始化 Token 缓存（与 DB 一致），再模拟提交预扣对缓存的影响
	_, err := cacheInitToken(getTokenFromDB(t, tok.Id))
	require.NoError(t, err)
	_, err = cacheApplyTokenQuotaDelta(tok.Id, tok.Key, -1000) // 提交预扣 1000
	require.NoError(t, err)

	// 补扣方向（delta=500）：缓存应同步到 remain=98500、used=1500
	task := seedTokenSettleTask(t, user.Id, tok.Id, 0, 1000)
	res, tokenRes := ApplySeedanceSettle(task, 500, false, TaskQuotaDeltaOptions{GuardPositiveDelta: true})
	require.Equal(t, TaskQuotaDeltaSuccess, res)
	require.Equal(t, TokenAdjustOK, tokenRes)
	dbTok := getTokenFromDB(t, tok.Id)
	assert.Equal(t, int64(98500), dbTok.RemainQuota, "DB 预扣+结算")
	cached, err := cacheGetTokenByKey(tok.Key)
	require.NoError(t, err)
	assert.Equal(t, dbTok.RemainQuota, cached.RemainQuota, "缓存 remain 与 DB 一致")
	assert.Equal(t, dbTok.UsedQuota, cached.UsedQuota, "缓存 used 与 DB 一致")

	// 退款方向（delta=-300）：缓存同步 remain 加回、used 冲减。
	// 注意：task2 的 fixture 已把 DB 预扣 1000（remain 97500/used 2500），
	// 缓存也需镜像该预扣（模拟提交路径的缓存预扣），再结算退款。
	seedUsedQuotaForUser(t, user.Id, 1000)
	task2 := seedTokenSettleTask(t, user.Id, tok.Id, 0, 1000)
	_, err = cacheApplyTokenQuotaDelta(tok.Id, tok.Key, -1000)
	require.NoError(t, err)
	res, tokenRes = ApplySeedanceSettle(task2, -300, false, TaskQuotaDeltaOptions{})
	require.Equal(t, TaskQuotaDeltaSuccess, res)
	require.Equal(t, TokenAdjustOK, tokenRes)
	dbTok = getTokenFromDB(t, tok.Id)
	cached, err = cacheGetTokenByKey(tok.Key)
	require.NoError(t, err)
	assert.Equal(t, dbTok.RemainQuota, cached.RemainQuota, "退款后缓存与 DB 一致")
	assert.Equal(t, dbTok.UsedQuota, cached.UsedQuota, "退款后缓存 used 与 DB 一致")
}

// 缓存同步失败：不得回滚已提交资金，必须记录错误并失效缓存键，让下次读取
// 回源数据库（数据库与缓存不长期保留不同额度）。
func TestTokenSettle_CacheSyncFailureInvalidatesNotRollsBack(t *testing.T) {
	truncateTables(t)
	_, client, hook := useQuotaCacheMiniRedis(t)
	ctx := context.Background()
	user := createReserveTestUser(t, 100000)
	tok := createReserveTestToken(t, 100000)
	require.NoError(t, DB.Model(&Token{}).Where("id = ?", tok.Id).Update("user_id", user.Id).Error)

	_, err := cacheInitToken(getTokenFromDB(t, tok.Id))
	require.NoError(t, err)
	_, err = cacheApplyTokenQuotaDelta(tok.Id, tok.Key, -1000)
	require.NoError(t, err)

	// 注入 EVAL 失败（模拟 Redis 增量同步故障）
	hook.fail = true
	task := seedTokenSettleTask(t, user.Id, tok.Id, 0, 1000)
	res, tokenRes := ApplySeedanceSettle(task, 500, false, TaskQuotaDeltaOptions{GuardPositiveDelta: true})
	hook.fail = false

	require.Equal(t, TaskQuotaDeltaSuccess, res, "资金结算不受缓存同步失败影响")
	require.Equal(t, TokenAdjustOK, tokenRes)
	assert.Equal(t, int64(100000-500), getUserQuotaFromDB(t, user.Id), "资金已提交（不回滚）")
	exists, err := client.Exists(ctx, getTokenCacheKey(tok.Key)).Result()
	require.NoError(t, err)
	assert.Equal(t, int64(0), exists, "缓存同步失败必须删除 Token 缓存键，强制下次回源数据库")

	// 下次读取回源数据库（缓存键已删除 / fence 生效期间读取走 DB）：
	// GetTokenByKey 返回数据库中的最新额度。
	dbTok := getTokenFromDB(t, tok.Id)
	fromDB, err := GetTokenByKey(tok.Key, false)
	require.NoError(t, err)
	assert.Equal(t, dbTok.RemainQuota, fromDB.RemainQuota, "读取回源数据库，拿到与 DB 一致的额度")
}

// 欠款清偿：debt.TokenId>0 且已扣减 → 提交后同步 Token 缓存（与 DB 一致）。
func TestTokenSettle_DebtRepaySyncsTokenCache(t *testing.T) {
	truncateTables(t)
	useQuotaCacheMiniRedis(t)
	user := createReserveTestUser(t, 5000)
	tok := createReserveTestToken(t, 10000)
	require.NoError(t, DB.Model(&Token{}).Where("id = ?", tok.Id).Update("user_id", user.Id).Error)
	seedDebtTask(t, user.Id, "task-debt-cache", 1000)

	_, _, _, err := CreateDebtAndFreeze(DebtInput{
		UserId: user.Id, TaskId: "task-debt-cache", PreConsumedQuota: 1000, ActualQuota: 1500, DeltaQuota: 500,
		TokenId: tok.Id, BillingSource: "wallet",
	})
	require.NoError(t, err)
	debt, err := GetTaskBillingDebtByTaskId("task-debt-cache")
	require.NoError(t, err)

	// 初始化缓存（DB remain 10000，无预扣；此处模拟清偿前缓存与 DB 一致）
	_, err = cacheInitToken(getTokenFromDB(t, tok.Id))
	require.NoError(t, err)

	require.NoError(t, RepayTaskBillingDebt(user.Id, debt.ID, RepayDebtOptions{}, 100))
	dbTok := getTokenFromDB(t, tok.Id)
	assert.Equal(t, int64(10000-500), dbTok.RemainQuota, "清偿扣减 Token 差额")
	cached, err := cacheGetTokenByKey(tok.Key)
	require.NoError(t, err)
	assert.Equal(t, dbTok.RemainQuota, cached.RemainQuota, "清偿后 Token 缓存与 DB 一致")
	assert.Equal(t, dbTok.UsedQuota, cached.UsedQuota, "清偿后 used 缓存与 DB 一致")
}

// 补偿路径：事务提交后同步 Token 缓存。
func TestTokenSettle_CompensateSyncsTokenCache(t *testing.T) {
	truncateTables(t)
	useQuotaCacheMiniRedis(t)
	user := createReserveTestUser(t, 100000)
	tok := createReserveTestToken(t, 10000)
	require.NoError(t, DB.Model(&Token{}).Where("id = ?", tok.Id).Update("user_id", user.Id).Error)
	task := seedTokenSettleTask(t, user.Id, tok.Id, 0, 1000) // DB remain 9000 used 1000
	require.NoError(t, AddTaskTokenDeltaPending(task.ID, 500))

	// 初始化缓存：此时 fixture 已把 DB 预扣（remain 9000），缓存直接取 DB 快照
	// （与 DB 一致，无需再手动应用预扣增量）。
	_, err := cacheInitToken(getTokenFromDB(t, tok.Id))
	require.NoError(t, err)

	compensated, err := CompensatePendingTokenDeltas(100)
	require.NoError(t, err)
	assert.Equal(t, 1, compensated)
	dbTok := getTokenFromDB(t, tok.Id)
	assert.Equal(t, int64(9000-500), dbTok.RemainQuota, "DB 补偿扣减")
	cached, err := cacheGetTokenByKey(tok.Key)
	require.NoError(t, err)
	assert.Equal(t, dbTok.RemainQuota, cached.RemainQuota, "补偿后 Token 缓存与 DB 一致")
}

// ===========================================================================
// 问题四：禁止丢弃无法补偿的 pending
// ===========================================================================

func getTokenDeltaPendingFromDB(t *testing.T, taskID int64) int {
	t.Helper()
	var pending int
	require.NoError(t, DB.Model(&Task{}).Where("id = ?", taskID).Pluck("token_delta_pending", &pending).Error)
	return pending
}

// 资金已收但任务无 Token（TokenId<=0）且 pending>0：补偿必须保留 pending
// 并持续告警（绝不静默清零——清零会永久丢失恢复证据），且管理端能查询。
func TestCompensatePending_KeepsPendingWithoutToken(t *testing.T) {
	truncateTables(t)
	user := createReserveTestUser(t, 10000)
	task := &Task{
		TaskID:    "task-pending-notoken",
		UserId:    user.Id,
		Quota:     1000,
		Status:    TaskStatusSuccess,
		Data:      []byte(`{}`),
		CreatedAt: time.Now().Unix(),
		UpdatedAt: time.Now().Unix(),
		PrivateData: TaskPrivateData{
			BillingSource: "wallet", // TokenId = 0（数据异常：资金已收但无令牌可扣）
		},
	}
	require.NoError(t, DB.Create(task).Error)
	require.NoError(t, DB.Model(&Task{}).Where("id = ?", task.ID).Update("token_delta_pending", 500).Error)

	compensated, err := CompensatePendingTokenDeltas(100)
	require.NoError(t, err)
	assert.Equal(t, 0, compensated, "无 Token 可扣的 pending 不得计入成功补偿")

	// pending 必须保留（恢复证据不丢失），绝不静默清零
	assert.Equal(t, 500, getTokenDeltaPendingFromDB(t, task.ID), "无 Token 的 pending 必须保留，等待人工处理")

	// 管理端可查询（GetTasksWithPendingTokenDelta 暴露给管理端，且任务查询
	// 接口 JSON 已含 token_delta_pending 字段）
	tasks, err := GetTasksWithPendingTokenDelta(100)
	require.NoError(t, err)
	found := false
	for _, t2 := range tasks {
		if t2.TaskID == task.TaskID && t2.TokenDeltaPending == 500 {
			found = true
		}
	}
	assert.True(t, found, "管理端必须能查询到该待人工处理记录")
}

// 补偿成功后 pending 才清零；再次扫描 no-op（不重复扣款、不误清其他 pending）。
func TestCompensatePending_ClearsOnlyOnSuccess(t *testing.T) {
	truncateTables(t)
	user := createReserveTestUser(t, 100000)
	tok := createReserveTestToken(t, 10000)
	require.NoError(t, DB.Model(&Token{}).Where("id = ?", tok.Id).Update("user_id", user.Id).Error)
	task := seedTokenSettleTask(t, user.Id, tok.Id, 0, 1000) // remain 9000
	require.NoError(t, AddTaskTokenDeltaPending(task.ID, 500))

	compensated, err := CompensatePendingTokenDeltas(100)
	require.NoError(t, err)
	assert.Equal(t, 1, compensated)
	assert.Equal(t, 0, getTokenDeltaPendingFromDB(t, task.ID), "真正扣减成功才清零")
	assert.Equal(t, int64(9000-500), getTokenFromDB(t, tok.Id).RemainQuota, "Token 只扣一次")
}
