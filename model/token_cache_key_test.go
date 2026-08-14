package model

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ===========================================================================
// 问题三：Token 缓存异常路径回归
//
// 修复前缺陷：syncTokenQuotaCacheAfterCommit 在资金事务提交后再次
// GetTokenById 解析 key；查询失败（令牌删除/DB 故障）时只有日志，无法删除
// 已知 Token 的缓存键 → 数据库与缓存长期保留不同额度。
//
// 修复后：结算/清偿/补偿事务在事务内取得并保留 Token key
// （applyTokenQuotaDeltaTx 返回 key），提交后直接用
// syncTokenQuotaCacheAfterCommitWithKey 同步缓存——根本不依赖第二次数据库
// 查询；增量同步失败时删除缓存键；缓存失败绝不回滚已提交资金。
// ===========================================================================

// 提交后 Token 查询不可用（令牌行被删除）时，缓存仍用事务内取得的 key 正确
// 同步——证明新路径不依赖提交后的第二次 GetTokenById。
func TestTokenCacheSync_UsesTxKeyNotSecondQuery(t *testing.T) {
	truncateTables(t)
	useQuotaCacheMiniRedis(t)
	user := createReserveTestUser(t, 100000)
	tok := createReserveTestToken(t, 100000)
	require.NoError(t, DB.Model(&Token{}).Where("id = ?", tok.Id).Update("user_id", user.Id).Error)

	// 预置缓存与 DB 一致，再模拟提交时的预扣增量（remain 99000 / used 1000）
	_, err := cacheInitToken(getTokenFromDB(t, tok.Id))
	require.NoError(t, err)
	_, err = cacheApplyTokenQuotaDelta(tok.Id, tok.Key, -1000)
	require.NoError(t, err)

	// Seedance 结算：事务内取得 key，提交后直接同步缓存
	task := seedTokenSettleTask(t, user.Id, tok.Id, 0, 1000)
	res, tokenRes := ApplySeedanceSettle(task, 500, false, TaskQuotaDeltaOptions{GuardPositiveDelta: true})
	require.Equal(t, TaskQuotaDeltaSuccess, res)
	require.Equal(t, TokenAdjustOK, tokenRes)

	dbTok := getTokenFromDB(t, tok.Id)
	cached, err := cacheGetTokenByKey(tok.Key)
	require.NoError(t, err)
	assert.Equal(t, dbTok.RemainQuota, cached.RemainQuota, "结算后缓存与 DB 一致（同步已由事务内 key 完成）")
	assert.Equal(t, dbTok.UsedQuota, cached.UsedQuota)

	// 提交后令牌行被删除（GetTokenById 将失败）：WithKey 同步仍能进行，
	// 直接用事务内取得的 key，绝不触发第二次数据库查询。
	require.NoError(t, DB.Delete(&tok).Error)
	syncTokenQuotaCacheAfterCommitWithKey(tok.Id, tok.Key, -200, "post-delete sync")
	cached, err = cacheGetTokenByKey(tok.Key)
	require.NoError(t, err)
	assert.Equal(t, dbTok.RemainQuota-200, cached.RemainQuota, "令牌删除后仍能用事务内 key 同步缓存（不依赖第二次查询）")
	assert.Equal(t, dbTok.UsedQuota+200, cached.UsedQuota)
}

// 增量同步失败（Redis EVAL 故障）时，用事务内 key 直接删除缓存键，
// 强制下次读取回源数据库——绝不留下旧缓存。
func TestTokenCacheSync_FailureDeletesCacheKeyWithKey(t *testing.T) {
	truncateTables(t)
	_, client, hook := useQuotaCacheMiniRedis(t)
	ctx := context.Background()
	user := createReserveTestUser(t, 100)
	tok := createReserveTestToken(t, 1000)
	require.NoError(t, DB.Model(&Token{}).Where("id = ?", tok.Id).Update("user_id", user.Id).Error)

	_, err := cacheInitToken(getTokenFromDB(t, tok.Id))
	require.NoError(t, err)
	exists, err := client.Exists(ctx, getTokenCacheKey(tok.Key)).Result()
	require.NoError(t, err)
	require.Equal(t, int64(1), exists, "前置：缓存键必须存在")

	hook.fail = true // 注入 EVAL（增量同步）失败；SET/DEL（失效）不受影响
	syncTokenQuotaCacheAfterCommitWithKey(tok.Id, tok.Key, -300, "test")
	hook.fail = false

	exists, err = client.Exists(ctx, getTokenCacheKey(tok.Key)).Result()
	require.NoError(t, err)
	assert.Equal(t, int64(0), exists, "同步失败必须删除缓存键，强制下次回源数据库")
}

// 清偿路径同样使用事务内 key 同步缓存：Token 查询不可用时缓存仍正确。
func TestTokenCacheSync_DebtRepayUsesTxKey(t *testing.T) {
	truncateTables(t)
	useQuotaCacheMiniRedis(t)
	user := createReserveTestUser(t, 5000)
	tok := createReserveTestToken(t, 10000)
	require.NoError(t, DB.Model(&Token{}).Where("id = ?", tok.Id).Update("user_id", user.Id).Error)
	seedDebtTask(t, user.Id, "task-debt-cachekey", 1000)

	_, _, _, err := CreateDebtAndFreeze(DebtInput{
		UserId: user.Id, TaskId: "task-debt-cachekey", PreConsumedQuota: 1000, ActualQuota: 1500, DeltaQuota: 500,
		TokenId: tok.Id, BillingSource: "wallet",
	})
	require.NoError(t, err)
	debt, err := GetTaskBillingDebtByTaskId("task-debt-cachekey")
	require.NoError(t, err)

	_, err = cacheInitToken(getTokenFromDB(t, tok.Id))
	require.NoError(t, err)

	require.NoError(t, RepayTaskBillingDebt(user.Id, debt.ID, RepayDebtOptions{}, 100))
	dbTok := getTokenFromDB(t, tok.Id)
	assert.Equal(t, 10000-500, dbTok.RemainQuota)

	// 提交后删除令牌行：清偿使用的也是事务内 key，缓存保持与 DB 一致
	require.NoError(t, DB.Delete(&tok).Error)
	cached, err := cacheGetTokenByKey(tok.Key)
	require.NoError(t, err)
	assert.Equal(t, dbTok.RemainQuota, cached.RemainQuota, "清偿后缓存与 DB 一致（事务内 key，不依赖第二次查询）")
	assert.Equal(t, dbTok.UsedQuota, cached.UsedQuota)
}
