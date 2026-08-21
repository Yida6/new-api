package model

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"

	"github.com/alicebob/miniredis/v2"
	"github.com/go-redis/redis/v8"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// failEvalHook 精确注入 EVAL 命令失败（其他命令放行），用于模拟
// "钱包缓存增量同步失败但 Redis 本身可用"的场景——这是补扣后旧缓存余额偏高、
// 仍按旧值授权的真实故障形态。
type failEvalHook struct {
	fail bool
}

func (h *failEvalHook) BeforeProcess(ctx context.Context, cmd redis.Cmder) (context.Context, error) {
	if h.fail && strings.EqualFold(cmd.Name(), "eval") {
		return ctx, errors.New("injected eval failure")
	}
	return ctx, nil
}
func (h *failEvalHook) AfterProcess(ctx context.Context, cmd redis.Cmder) error { return nil }
func (h *failEvalHook) BeforeProcessPipeline(ctx context.Context, cmds []redis.Cmder) (context.Context, error) {
	return ctx, nil
}
func (h *failEvalHook) AfterProcessPipeline(ctx context.Context, cmds []redis.Cmder) error {
	return nil
}

func useQuotaCacheMiniRedis(t *testing.T) (*miniredis.Miniredis, *redis.Client, *failEvalHook) {
	t.Helper()
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	hook := &failEvalHook{}
	client.AddHook(hook)
	oldRDB, oldEnabled := common.RDB, common.RedisEnabled
	common.RDB, common.RedisEnabled = client, true
	t.Cleanup(func() {
		client.Close()
		common.RDB, common.RedisEnabled = oldRDB, oldEnabled
	})
	return server, client, hook
}

// Fix P1#2：资金事务提交后的钱包缓存同步失败时，必须删除缓存键——
// 否则补扣后缓存仍为旧（偏高）余额，TryReserveUserQuota 按缓存授权时
// 会继续放行超额请求，把数据库余额扣成负数。
func TestSyncWalletQuotaCacheAfterCommit_DeletesCacheOnSyncFailure(t *testing.T) {
	truncateTables(t)
	_, client, hook := useQuotaCacheMiniRedis(t)
	ctx := context.Background()

	user := createReserveTestUser(t, 2000)
	uid := user.Id
	// 预置缓存（旧余额 2000，模拟"事务已提交但缓存尚未同步"的危险状态）
	require.NoError(t, populateUserCache(user))

	key := getUserCacheKey(uid)
	exists, err := client.Exists(ctx, key).Result()
	require.NoError(t, err)
	require.Equal(t, int64(1), exists, "前置：缓存必须存在")

	// 模拟调用方已提交的扣款事务（数据库余额已变为 800，缓存仍为 2000）
	require.NoError(t, DB.Model(&User{}).Where("id = ?", uid).Update("quota", 800).Error)

	// 注入缓存增量同步失败 → 必须删除缓存键
	hook.fail = true
	syncWalletQuotaCacheAfterCommit(uid, -1200, "task delta")
	hook.fail = false

	exists, err = client.Exists(ctx, key).Result()
	require.NoError(t, err)
	assert.Equal(t, int64(0), exists, "同步失败后必须删除缓存键，防止旧余额继续授权")

	// 下次读取从数据库水合正确余额（800），不再按旧的 2000 授权
	ub, err := GetUserCache(uid)
	require.NoError(t, err)
	assert.Equal(t, int64(800), ub.Quota, "水合后的余额必须来自数据库")
}

// 成功路径对照：同步成功时缓存余额与数据库一致，不删除缓存。
func TestSyncWalletQuotaCacheAfterCommit_SyncSuccessKeepsCache(t *testing.T) {
	truncateTables(t)
	_, client, _ := useQuotaCacheMiniRedis(t)
	ctx := context.Background()

	user := createReserveTestUser(t, 2000)
	uid := user.Id
	require.NoError(t, populateUserCache(user))
	require.NoError(t, DB.Model(&User{}).Where("id = ?", uid).Update("quota", 800).Error)

	syncWalletQuotaCacheAfterCommit(uid, -1200, "task delta")

	ub, err := GetUserCache(uid)
	require.NoError(t, err)
	assert.Equal(t, int64(800), ub.Quota, "同步成功后缓存余额即为数据库余额")
	exists, err := client.Exists(ctx, getUserCacheKey(uid)).Result()
	require.NoError(t, err)
	assert.Equal(t, int64(1), exists, "同步成功不应删除缓存")
}

// ===========================================================================
// 钱包扣减 DB 层守卫测试（Fix P1#1：杜绝"缓存旧值授权 + DB 无守卫扣减"的超扣）
// ===========================================================================

// 场景：任务补扣事务已提交（DB 余额已减）但缓存尚未同步（缓存仍为旧的高余额）。
// 并发请求按旧缓存授权成功，但 DB 落账守卫（quota >= 扣减额）必须拒绝——
// 绝不把数据库余额扣成负数；缓存被补偿回原值，请求按"预扣失败"处理。
func TestTryReserveUserQuota_DBGuardRejectsStaleCacheWindow(t *testing.T) {
	truncateTables(t)
	_, client, _ := useQuotaCacheMiniRedis(t)
	ctx := context.Background()

	user := createReserveTestUser(t, 100)
	uid := user.Id
	require.NoError(t, populateUserCache(user)) // 缓存余额 100
	// 模拟"补扣已提交但缓存未同步"：数据库余额只剩 50，缓存仍为 100
	require.NoError(t, DB.Model(&User{}).Where("id = ?", uid).Update("quota", 50).Error)

	// 缓存按旧值 100 授权 60 → DB 守卫（50 >= 60 不成立）→ 预扣失败，不得超扣
	reserved, err := TryReserveUserQuota(uid, 60)
	require.NoError(t, err, "余额不足应视为预扣失败而非数据库错误")
	assert.False(t, reserved, "数据库余额不足时预扣必须失败")

	assert.Equal(t, int64(50), getUserQuotaFromDB(t, uid), "数据库余额不得被扣成负数")

	// 缓存被补偿回 100（授权未生效，余额仍为旧值）
	quotaVal, err := client.HGet(ctx, getUserCacheKey(uid), "Quota").Result()
	require.NoError(t, err)
	assert.Equal(t, "100", quotaVal, "授权失败后缓存必须补偿回原值")

	// 正常路径不受影响：缓存与 DB 一致时预扣成功
	require.NoError(t, DB.Model(&User{}).Where("id = ?", uid).Update("quota", 100).Error)
	reserved, err = TryReserveUserQuota(uid, 60)
	require.NoError(t, err)
	assert.True(t, reserved)
	assert.Equal(t, int64(40), getUserQuotaFromDB(t, uid))
}

// ===========================================================================
// 批量模式（BatchUpdateEnabled=true）下的资金正确性（Fix P1#1 补充场景）
// ===========================================================================

// Fix：批量模式下授权扣减（persistUserQuotaDelta delta<0）仍强制直写带守卫——
// 若走批量入队，落账是异步的，守卫失败时请求已经成功执行却未扣款（"白嫖窗口"），
// 且批量落账无法逐笔带守卫。直写带守卫使授权与扣款原子绑定：窗口内请求被同步
// 拒绝（不产生"成功但未扣款"），余额充足时立即落账，加方向（充值/退款）仍批量。
func TestTryReserveUserQuota_BatchModeDeductionDirectGuarded(t *testing.T) {
	truncateTables(t)
	resetBatchUpdateTestState(t)
	// resetBatchUpdateTestState 会把开关置 false，此处显式开启批量模式
	oldBatch := common.BatchUpdateEnabled
	common.BatchUpdateEnabled = true
	t.Cleanup(func() { common.BatchUpdateEnabled = oldBatch })
	_, client, _ := useQuotaCacheMiniRedis(t)
	ctx := context.Background()

	user := createReserveTestUser(t, 100)
	uid := user.Id
	require.NoError(t, populateUserCache(user)) // 缓存 100
	// 模拟"补扣已提交但缓存未同步"：数据库余额只剩 50
	require.NoError(t, DB.Model(&User{}).Where("id = ?", uid).Update("quota", 50).Error)

	// 窗口内请求：缓存按旧值 100 授权，但扣减直写守卫（DB 50 < 60）→ 同步拒绝，
	// 绝不出现"请求成功执行却未扣款"
	reserved, err := TryReserveUserQuota(uid, 60)
	require.NoError(t, err)
	assert.False(t, reserved, "批量模式下窗口内请求必须被同步拒绝，杜绝白嫖窗口")
	assert.Equal(t, int64(50), getUserQuotaFromDB(t, uid), "DB 不得为负")

	// 被拒绝的授权未入队（没有待异步落账的扣款）
	batchUpdateLocks[BatchUpdateTypeUserQuota].Lock()
	_, enqueued := batchUpdateStores[BatchUpdateTypeUserQuota][uid]
	batchUpdateLocks[BatchUpdateTypeUserQuota].Unlock()
	assert.False(t, enqueued, "被拒绝的授权不得进入批量队列")

	// 缓存被补偿回原值
	quotaVal, err := client.HGet(ctx, getUserCacheKey(uid), "Quota").Result()
	require.NoError(t, err)
	assert.Equal(t, "100", quotaVal, "授权失败后缓存必须补偿回原值")

	// 成功路径：余额充足时扣减立即直写落账（非批量入队），DB 与缓存即时一致
	require.NoError(t, DB.Model(&User{}).Where("id = ?", uid).Update("quota", 100).Error)
	reserved, err = TryReserveUserQuota(uid, 30)
	require.NoError(t, err)
	assert.True(t, reserved)
	assert.Equal(t, int64(70), getUserQuotaFromDB(t, uid), "批量模式下授权扣减必须立即直写落账")

	// 加方向（充值/退款落账）仍走批量队列
	require.NoError(t, persistUserQuotaDelta(uid, +10))
	batchUpdateLocks[BatchUpdateTypeUserQuota].Lock()
	enqueuedVal, ok := batchUpdateStores[BatchUpdateTypeUserQuota][uid]
	batchUpdateLocks[BatchUpdateTypeUserQuota].Unlock()
	assert.True(t, ok, "加方向应进入批量队列")
	assert.Equal(t, int64(10), enqueuedVal)
}
