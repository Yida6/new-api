package model

import (
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func createReserveTestUser(t *testing.T, quota int64) User {
	t.Helper()
	user := User{
		Username:    "reserve-user-" + common.GetRandomString(6),
		Password:    "unused-password-hash",
		Role:        common.RoleCommonUser,
		Status:      common.UserStatusEnabled,
		Group:       "default",
		AuthVersion: 1,
		Quota:       quota,
		AffCode:     "reserve-aff-" + common.GetRandomString(8),
	}
	require.NoError(t, DB.Create(&user).Error)
	return user
}

func createReserveTestToken(t *testing.T, remainQuota int64) Token {
	t.Helper()
	token := Token{
		UserId:      1,
		Key:         "reserve-token-" + common.GetRandomString(8),
		Name:        "reserve-test",
		Status:      common.TokenStatusEnabled,
		ExpiredTime: -1,
		RemainQuota: remainQuota,
	}
	require.NoError(t, token.Insert())
	return token
}

func getUserQuotaFromDB(t *testing.T, id int) int64 {
	t.Helper()
	var user User
	require.NoError(t, DB.Select("quota").First(&user, id).Error)
	return user.Quota
}

func getTokenFromDB(t *testing.T, id int) Token {
	t.Helper()
	var token Token
	require.NoError(t, DB.First(&token, id).Error)
	return token
}

func resetBatchUpdateTestState(t *testing.T) {
	t.Helper()
	oldBatchEnabled := common.BatchUpdateEnabled
	common.BatchUpdateEnabled = false
	for i := 0; i < BatchUpdateTypeCount; i++ {
		batchUpdateLocks[i].Lock()
		batchUpdateStores[i] = make(map[int]int64)
		batchUpdateLocks[i].Unlock()
	}
	t.Cleanup(func() {
		common.BatchUpdateEnabled = oldBatchEnabled
		for i := 0; i < BatchUpdateTypeCount; i++ {
			batchUpdateLocks[i].Lock()
			batchUpdateStores[i] = make(map[int]int64)
			batchUpdateLocks[i].Unlock()
		}
	})
}

func TestTryReserveQuotaWithoutRedis(t *testing.T) {
	truncateTables(t)
	resetBatchUpdateTestState(t)

	user := createReserveTestUser(t, 100)
	reserved, err := TryReserveUserQuota(user.Id, 60)
	require.NoError(t, err)
	assert.True(t, reserved)
	assert.Equal(t, int64(40), getUserQuotaFromDB(t, user.Id))

	reserved, err = TryReserveUserQuota(user.Id, 41)
	require.NoError(t, err)
	assert.False(t, reserved)
	assert.Equal(t, int64(40), getUserQuotaFromDB(t, user.Id))

	token := createReserveTestToken(t, 80)
	reserved, err = TryReserveTokenQuota(token.Id, token.Key, 25, false)
	require.NoError(t, err)
	assert.True(t, reserved)
	reloaded := getTokenFromDB(t, token.Id)
	assert.Equal(t, int64(55), reloaded.RemainQuota)
	assert.Equal(t, int64(25), reloaded.UsedQuota)

	reserved, err = TryReserveTokenQuota(token.Id, token.Key, 56, false)
	require.NoError(t, err)
	assert.False(t, reserved)
	assert.Equal(t, int64(55), getTokenFromDB(t, token.Id).RemainQuota)
}

func TestRedisBatchReserveNeverFallsBackToStaleDatabaseBalance(t *testing.T) {
	truncateTables(t)
	resetBatchUpdateTestState(t)
	useUserCacheMiniRedis(t)
	common.BatchUpdateEnabled = true

	user := createReserveTestUser(t, 10)
	reserved, err := TryReserveUserQuota(user.Id, 8)
	require.NoError(t, err)
	assert.True(t, reserved)
	// 钱包扣减方向（授权落账）无论是否批量模式都直写带守卫：
	// DB 立即扣减（不再延迟落账），消除"请求成功但未扣款"的窗口
	assert.Equal(t, int64(2), getUserQuotaFromDB(t, user.Id), "授权扣减必须立即直写落账，不能只入队")

	reserved, err = TryReserveUserQuota(user.Id, 3)
	require.NoError(t, err)
	assert.False(t, reserved, "stale DB balance must not authorize a second spend")
	cachedUser, err := GetUserCache(user.Id)
	require.NoError(t, err)
	assert.Equal(t, int64(2), cachedUser.Quota)

	token := createReserveTestToken(t, 9)
	reserved, err = TryReserveTokenQuota(token.Id, token.Key, 7, false)
	require.NoError(t, err)
	assert.True(t, reserved)
	// 问题一 P0：有限 Token 授权扣减无论批量模式都直写带守卫落账——
	// DB 立即扣减（不再延迟入队），消除"Redis 已扣、DB 旧余额"的窗口
	assert.Equal(t, int64(2), getTokenFromDB(t, token.Id).RemainQuota, "有限 Token 授权扣减必须立即直写落账，不能只入队")
	assert.Equal(t, int64(7), getTokenFromDB(t, token.Id).UsedQuota)
	reserved, err = TryReserveTokenQuota(token.Id, token.Key, 3, false)
	require.NoError(t, err)
	assert.False(t, reserved)
	assert.Equal(t, int64(2), getTokenFromDB(t, token.Id).RemainQuota, "余额不足时 DB 不得扣成负数")
	assert.Equal(t, int64(2), getTokenFromDB(t, token.Id).RemainQuota)

	batchUpdate()
	// 钱包扣减已直写（无 user 批量条目）；令牌扣减同样已直写（无 token 批量
	// 扣减条目）——批量队列只承载加方向增量（退款/充值）。
	assert.Equal(t, int64(2), getUserQuotaFromDB(t, user.Id))
	reloadedToken := getTokenFromDB(t, token.Id)
	assert.Equal(t, int64(2), reloadedToken.RemainQuota)
	assert.Equal(t, int64(7), reloadedToken.UsedQuota)
	assert.Equal(t, int64(9), reloadedToken.RemainQuota+reloadedToken.UsedQuota, "remain+used 不变量保持不变")
}

func TestReserveFallsBackToDatabaseWhenRedisIsUnavailable(t *testing.T) {
	truncateTables(t)
	resetBatchUpdateTestState(t)
	server := useUserCacheMiniRedis(t)

	user := createReserveTestUser(t, 20)
	require.NoError(t, populateUserCache(user))
	server.Close()

	// Redis 故障时降级为数据库条件更新：服务保持可用且不会超扣。
	reserved, err := TryReserveUserQuota(user.Id, 5)
	require.NoError(t, err)
	assert.True(t, reserved)
	assert.Equal(t, int64(15), getUserQuotaFromDB(t, user.Id))

	reserved, err = TryReserveUserQuota(user.Id, 16)
	require.NoError(t, err)
	assert.False(t, reserved)
	assert.Equal(t, int64(15), getUserQuotaFromDB(t, user.Id))
}

func TestSynchronousReserveCompensatesCacheWhenPersistenceFails(t *testing.T) {
	truncateTables(t)
	resetBatchUpdateTestState(t)
	useUserCacheMiniRedis(t)

	user := createReserveTestUser(t, 10)
	require.NoError(t, populateUserCache(user))
	require.NoError(t, DB.Delete(&user).Error)

	reserved, err := TryReserveUserQuota(user.Id, 6)
	assert.False(t, reserved)
	// 用户不存在/余额不足统一按"预扣失败"处理（false, nil），由调用方（WalletFunding.
	// PreConsume）映射为 ErrInsufficientWalletQuota → 403，而不是向上传播 500。
	assert.NoError(t, err)
	cached, cacheErr := cacheGetUserBase(user.Id)
	require.NoError(t, cacheErr)
	assert.Equal(t, int64(10), cached.Quota)

	token := createReserveTestToken(t, 12)
	_, err = GetTokenByKey(token.Key, true)
	require.NoError(t, err)
	require.NoError(t, DB.Delete(&token).Error)
	// 令牌已删除：缓存授权成功但数据库落账守卫拒绝（RowsAffected=0）→
	// 补偿缓存并统一按"预扣失败/额度不足"处理（false, nil，与钱包路径同语义，
	// 调用方映射为额度不足而非 500；见问题一 P0 修复）。
	reserved, err = TryReserveTokenQuota(token.Id, token.Key, 7, false)
	assert.False(t, reserved)
	assert.NoError(t, err, "守卫拒绝应按额度不足处理，不向上传播数据库错误")
	cachedToken, cacheErr := cacheGetTokenByKey(token.Key)
	require.NoError(t, cacheErr)
	assert.Equal(t, int64(12), cachedToken.RemainQuota, "授权失败后缓存必须补偿回原值")
	assert.Zero(t, cachedToken.UsedQuota)
}

func TestTokenCacheInitPreservesLiveQuotaAndFenceBlocksStaleSnapshot(t *testing.T) {
	truncateTables(t)
	resetBatchUpdateTestState(t)
	server := useUserCacheMiniRedis(t)

	token := createReserveTestToken(t, 100)
	loaded, err := GetTokenByKey(token.Key, true)
	require.NoError(t, err)
	stale := *loaded

	result, err := cacheApplyTokenQuotaDelta(token.Id, token.Key, -70)
	require.NoError(t, err)
	require.Equal(t, cacheQuotaOK, result)

	// 已存在的哈希只刷新 TTL：数据库快照不得覆盖已被原子预扣的余额。
	code, err := cacheInitToken(stale)
	require.NoError(t, err)
	assert.Equal(t, 2, code)
	cached, err := cacheGetTokenByKey(token.Key)
	require.NoError(t, err)
	assert.Equal(t, int64(30), cached.RemainQuota)

	// 变更期间：fence 删除缓存并拦截并发读者手中的过期快照。
	require.NoError(t, invalidateTokenCacheForMutation(token.Key))
	code, err = cacheInitToken(stale)
	require.NoError(t, err)
	assert.Zero(t, code, "the pre-mutation snapshot must not be published while fenced")
	_, err = cacheGetTokenByKey(token.Key)
	assert.Error(t, err)

	// fence 过期后可重新从数据库水合。
	server.FastForward(time.Duration(tokenCacheFenceSeconds+1) * time.Second)
	fresh, err := GetTokenByKey(token.Key, false)
	require.NoError(t, err)
	assert.Equal(t, int64(100), fresh.RemainQuota)
	cached, err = cacheGetTokenByKey(token.Key)
	require.NoError(t, err)
	assert.Equal(t, int64(100), cached.RemainQuota)
}

// TestTryReserveLargeQuotaRedisNoOverflow 覆盖 int64 大额度在 Redis 预扣、
// 数据库落账与退款链路中不溢出、不产生负额度：
//   - 55,555,500,000（≈111111 USD @500000）单笔预扣/退款
//   - 预扣后余额不足再扣必须拒绝（不扣成负数）
//   - Token remain+used 不变量始终成立
func TestTryReserveLargeQuotaRedisNoOverflow(t *testing.T) {
	truncateTables(t)
	resetBatchUpdateTestState(t)
	useUserCacheMiniRedis(t)

	const largeRemain = int64(100_000_000_000) // 1000 亿内部额度
	const largeReserve = int64(55_555_500_000) // ≈111111 USD @500000
	user := createReserveTestUser(t, largeRemain)
	require.NoError(t, populateUserCache(user))

	// 1) 大额预扣成功，数据库落账不溢出
	reserved, err := TryReserveUserQuota(user.Id, largeReserve)
	require.NoError(t, err)
	require.True(t, reserved)
	afterReserve := largeRemain - largeReserve
	assert.Equal(t, afterReserve, getUserQuotaFromDB(t, user.Id), "大额预扣后余额")

	// 2) 余额不足再扣拒绝，余额不变（不扣成负数）
	reserved, err = TryReserveUserQuota(user.Id, afterReserve+1)
	require.NoError(t, err)
	assert.False(t, reserved)
	assert.Equal(t, afterReserve, getUserQuotaFromDB(t, user.Id))

	// 3) 退款（增加方向）恢复余额，无溢出
	require.NoError(t, IncreaseUserQuota(user.Id, largeReserve, false))
	assert.Equal(t, largeRemain, getUserQuotaFromDB(t, user.Id), "退款后余额恢复")

	// 4) Token 大额度：预扣后 remain+used 不变量成立
	token := createReserveTestToken(t, largeRemain)
	_, err = GetTokenByKey(token.Key, true)
	require.NoError(t, err)
	reserved, err = TryReserveTokenQuota(token.Id, token.Key, largeReserve, false)
	require.NoError(t, err)
	require.True(t, reserved)
	tok := getTokenFromDB(t, token.Id)
	assert.Equal(t, largeRemain-largeReserve, tok.RemainQuota)
	assert.Equal(t, largeReserve, tok.UsedQuota)
	assert.Equal(t, largeRemain, tok.RemainQuota+tok.UsedQuota, "remain+used 不变量")

	// 5) Token 退款：IncreaseTokenQuota 恢复 remain
	require.NoError(t, IncreaseTokenQuota(token.Id, token.Key, largeReserve))
	tok = getTokenFromDB(t, token.Id)
	assert.Equal(t, largeRemain, tok.RemainQuota)
	assert.Zero(t, tok.UsedQuota)
}
