package model

import (
	"context"
	"errors"
	"fmt"

	"github.com/QuantumNous/new-api/common"
	"gorm.io/gorm"
)

type cacheQuotaResult int

const (
	cacheQuotaInsufficient cacheQuotaResult = iota
	cacheQuotaOK
	cacheQuotaMiss
)

const userQuotaReserveScript = `
if tonumber(redis.call('HGET', KEYS[1], 'Id') or '0') ~= tonumber(ARGV[2])
  or tonumber(redis.call('HGET', KEYS[1], 'CacheSchema') or '0') ~= tonumber(ARGV[3])
  or redis.call('HEXISTS', KEYS[1], 'Quota') == 0 then
  return -1
end
local quota = tonumber(redis.call('HGET', KEYS[1], 'Quota'))
if quota == nil or quota < tonumber(ARGV[1]) then
  return 0
end
redis.call('HINCRBY', KEYS[1], 'Quota', -tonumber(ARGV[1]))
return 1`

const userQuotaDeltaScript = `
if tonumber(redis.call('HGET', KEYS[1], 'Id') or '0') ~= tonumber(ARGV[2])
  or tonumber(redis.call('HGET', KEYS[1], 'CacheSchema') or '0') ~= tonumber(ARGV[3])
  or redis.call('HEXISTS', KEYS[1], 'Quota') == 0 then
  return -1
end
redis.call('HINCRBY', KEYS[1], 'Quota', tonumber(ARGV[1]))
return 1`

const tokenQuotaReserveScript = `
if tonumber(redis.call('HGET', KEYS[1], 'Id') or '0') ~= tonumber(ARGV[2])
  or redis.call('HEXISTS', KEYS[1], 'RemainQuota') == 0
  or redis.call('HEXISTS', KEYS[1], 'UsedQuota') == 0 then
  return -1
end
local remain = tonumber(redis.call('HGET', KEYS[1], 'RemainQuota'))
if remain == nil or remain < tonumber(ARGV[1]) then
  return 0
end
redis.call('HINCRBY', KEYS[1], 'RemainQuota', -tonumber(ARGV[1]))
redis.call('HINCRBY', KEYS[1], 'UsedQuota', tonumber(ARGV[1]))
redis.call('HSET', KEYS[1], 'AccessedTime', ARGV[3])
return 1`

const tokenQuotaDeltaScript = `
if tonumber(redis.call('HGET', KEYS[1], 'Id') or '0') ~= tonumber(ARGV[2])
  or redis.call('HEXISTS', KEYS[1], 'RemainQuota') == 0
  or redis.call('HEXISTS', KEYS[1], 'UsedQuota') == 0 then
  return -1
end
redis.call('HINCRBY', KEYS[1], 'RemainQuota', tonumber(ARGV[1]))
redis.call('HINCRBY', KEYS[1], 'UsedQuota', -tonumber(ARGV[1]))
redis.call('HSET', KEYS[1], 'AccessedTime', ARGV[3])
return 1`

func quotaResultFromLua(result int, err error) (cacheQuotaResult, error) {
	if err != nil {
		return cacheQuotaMiss, err
	}
	switch result {
	case 1:
		return cacheQuotaOK, nil
	case 0:
		return cacheQuotaInsufficient, nil
	default:
		return cacheQuotaMiss, nil
	}
}

func cacheTryReserveUserQuota(userID int, amount int64) (cacheQuotaResult, error) {
	result, err := common.RDB.Eval(context.Background(), userQuotaReserveScript,
		[]string{getUserCacheKey(userID)}, amount, userID, userCacheSchemaVersion).Int()
	return quotaResultFromLua(result, err)
}

func cacheApplyUserQuotaDelta(userID int, delta int64) (cacheQuotaResult, error) {
	result, err := common.RDB.Eval(context.Background(), userQuotaDeltaScript,
		[]string{getUserCacheKey(userID)}, delta, userID, userCacheSchemaVersion).Int()
	return quotaResultFromLua(result, err)
}

func cacheTryReserveTokenQuota(id int, key string, amount int64) (cacheQuotaResult, error) {
	result, err := common.RDB.Eval(context.Background(), tokenQuotaReserveScript,
		[]string{getTokenCacheKey(key)}, amount, id, common.GetTimestamp()).Int()
	return quotaResultFromLua(result, err)
}

func cacheApplyTokenQuotaDelta(id int, key string, delta int64) (cacheQuotaResult, error) {
	result, err := common.RDB.Eval(context.Background(), tokenQuotaDeltaScript,
		[]string{getTokenCacheKey(key)}, delta, id, common.GetTimestamp()).Int()
	return quotaResultFromLua(result, err)
}

// persistUserQuotaDelta 把已在缓存侧预扣成功的增量落库。扣减方向（delta<0，授权
// 落账）**无论是否批量模式都强制直写带守卫**；加方向（充值/退款，不会超扣）在
// 批量模式下入队合批。
//
// 资金守卫：扣减方向必须带 quota >= -delta 条件——缓存授权是"乐观放行"，数据库
// 落账是"最终校验"。若不加守卫，缓存旧值偏高（任务补扣已提交但缓存尚未同步）的
// 窗口内，并发请求按旧缓存授权成功后会把数据库余额扣成负数（可发生的超扣）。
// 批量模式下扣减同样不入队：批量落账是异步的，守卫失败时请求已经成功执行却未
// 扣款（"白嫖窗口"），且批量落账无法逐笔带守卫；直写带守卫使授权与扣款原子绑定——
// 请求要么成功且扣款，要么失败（补偿缓存）不扣款。
// 余额不足/用户不存在时 RowsAffected=0，返回 gorm.ErrRecordNotFound，调用方据此
// 补偿缓存并按"预扣失败"处理。
func persistUserQuotaDelta(id int, delta int64) error {
	if common.BatchUpdateEnabled && delta > 0 {
		addNewRecord(BatchUpdateTypeUserQuota, id, delta)
		return nil
	}
	var result *gorm.DB
	if delta < 0 {
		result = DB.Model(&User{}).Where("id = ? AND quota >= ?", id, -delta).
			Update("quota", gorm.Expr("quota + ?", delta))
	} else {
		result = DB.Model(&User{}).Where("id = ?", id).Update("quota", gorm.Expr("quota + ?", delta))
	}
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return gorm.ErrRecordNotFound
	}
	return nil
}

// persistTokenQuotaDelta 把已在缓存侧预扣成功的增量落库。本函数只被有限
// Token 的授权路径调用（TryReserveTokenQuota 的 unlimited 分支提前走
// DecreaseTokenQuota，不经过此处；批量队列的 Token 条目只承载加方向增量，
// 见下方说明）。
//
// 资金守卫（有限 Token）：
//   - 扣减方向（delta<0，授权落账）**无论是否批量模式都强制直写带守卫**
//     `WHERE id = ? AND remain_quota >= -delta` 且严格校验 RowsAffected==1。
//     缓存授权是"乐观放行"，数据库落账是"最终校验"：若不加守卫，缓存旧值
//     偏高（任务补扣已提交但缓存尚未同步）的窗口内，并发请求按旧缓存授权
//     成功后会把数据库余额扣成负数（可发生的超扣）。
//   - 批量模式下扣减同样不入队：批量落账是异步的，守卫失败时请求已经成功
//     执行却未扣款（"白嫖窗口"），且批量落账（increaseTokenQuota）无法逐笔
//     带守卫；直写带守卫使授权与扣款原子绑定——请求要么成功且扣款，要么
//     失败（调用方补偿缓存）不扣款。
//   - 加方向（delta>0，退款/充值，不会超扣）保持既有语义：批量模式入队
//     合批，非批量直写。
// 余额不足/令牌不存在时 RowsAffected=0，返回 gorm.ErrRecordNotFound，
// 调用方据此补偿缓存并按"预扣失败/额度不足"处理（与用户钱包路径同一语义）。
func persistTokenQuotaDelta(id int, delta int64) error {
	if common.BatchUpdateEnabled && delta > 0 {
		addNewRecord(BatchUpdateTypeTokenQuota, id, delta)
		return nil
	}
	updates := map[string]interface{}{
		"remain_quota":  gorm.Expr("remain_quota + ?", delta),
		"used_quota":    gorm.Expr("used_quota - ?", delta),
		"accessed_time": common.GetTimestamp(),
	}
	var result *gorm.DB
	if delta < 0 {
		result = DB.Model(&Token{}).Where("id = ? AND remain_quota >= ?", id, -delta).Updates(updates)
	} else {
		result = DB.Model(&Token{}).Where("id = ?", id).Updates(updates)
	}
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return gorm.ErrRecordNotFound
	}
	return nil
}

func reserveUserQuotaDB(id int, quota int64) (bool, error) {
	result := DB.Model(&User{}).
		Where("id = ? AND quota >= ?", id, quota).
		Update("quota", gorm.Expr("quota - ?", quota))
	return result.RowsAffected == 1, result.Error
}

func reserveTokenQuotaDB(id int, quota int64) (bool, error) {
	result := DB.Model(&Token{}).
		Where("id = ? AND remain_quota >= ?", id, quota).
		Updates(map[string]interface{}{
			"remain_quota":  gorm.Expr("remain_quota - ?", quota),
			"used_quota":    gorm.Expr("used_quota + ?", quota),
			"accessed_time": common.GetTimestamp(),
		})
	return result.RowsAffected == 1, result.Error
}

// TryReserveUserQuota atomically checks and deducts a user's wallet quota.
// 缓存命中时以缓存余额为准（避免批量模式下过期的数据库余额放大并发超扣）；
// Redis 异常或水合失败时降级为数据库条件更新，保证服务可用。
func TryReserveUserQuota(id int, quota int64) (bool, error) {
	if quota < 0 {
		return false, errors.New("quota 不能为负数！")
	}
	if quota == 0 {
		return true, nil
	}
	if !common.RedisEnabled {
		return reserveUserQuotaDB(id, quota)
	}

	result, err := cacheTryReserveUserQuota(id, int64(quota))
	if err == nil && result == cacheQuotaMiss {
		if _, hydrateErr := GetUserCache(id); hydrateErr == nil {
			result, err = cacheTryReserveUserQuota(id, int64(quota))
		}
	}
	if err != nil || result == cacheQuotaMiss {
		if err != nil {
			common.SysLog("user quota cache reserve unavailable, falling back to database: " + err.Error())
		}
		return reserveUserQuotaDB(id, quota)
	}
	if result == cacheQuotaInsufficient {
		return false, nil
	}
	if err = persistUserQuotaDelta(id, -quota); err != nil {
		compensated, compensateErr := cacheApplyUserQuotaDelta(id, int64(quota))
		if compensateErr != nil || compensated != cacheQuotaOK {
			common.SysError(fmt.Sprintf("failed to compensate reserved user quota: result=%d error=%v", compensated, compensateErr))
		}
		// 用户不存在或数据库余额不足（守卫拒绝）：缓存已放行但 DB 不允许 →
		// 视为"预扣失败"（非致命错误），调用方按额度不足处理，绝不把余额扣成负数。
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// TryReserveTokenQuota atomically checks and deducts a token quota. Unlimited
// tokens skip the balance check but still update remain/used accounting
// (DecreaseTokenQuota 的既有无守卫记账语义，路径与有限 Token 明确分离)。
//
// 有限 Token：Redis 缓存授权（乐观放行）成功后，落账走
// persistTokenQuotaDelta——扣减方向无论批量模式都直写带
// `remain_quota >= quota` 守卫；数据库守卫失败（余额不足/令牌缺失）时补偿
// 已成功的 Redis 预扣并返回 (false, nil)（额度不足），绝不把余额扣成负数，
// 也绝不让扣减进入无守卫批量队列。
func TryReserveTokenQuota(id int, key string, quota int64, unlimited bool) (bool, error) {
	if quota < 0 {
		return false, errors.New("quota 不能为负数！")
	}
	if quota == 0 {
		return true, nil
	}
	if unlimited {
		// unlimited：无余额守卫记账（既有语义，DecreaseTokenQuota 批量/直写）。
		return true, DecreaseTokenQuota(id, key, quota)
	}
	if !common.RedisEnabled {
		return reserveTokenQuotaDB(id, quota)
	}

	result, err := cacheTryReserveTokenQuota(id, key, int64(quota))
	if err == nil && result == cacheQuotaMiss {
		if _, hydrateErr := GetTokenByKey(key, true); hydrateErr == nil {
			result, err = cacheTryReserveTokenQuota(id, key, int64(quota))
		}
	}
	if err != nil || result == cacheQuotaMiss {
		if err != nil {
			common.SysLog("token quota cache reserve unavailable, falling back to database: " + err.Error())
		}
		return reserveTokenQuotaDB(id, quota)
	}
	if result == cacheQuotaInsufficient {
		return false, nil
	}
	// 有限 Token 落账：守卫直写（批量模式也同步执行，见 persistTokenQuotaDelta）。
	if err = persistTokenQuotaDelta(id, -quota); err != nil {
		compensated, compensateErr := cacheApplyTokenQuotaDelta(id, key, int64(quota))
		if compensateErr != nil || compensated != cacheQuotaOK {
			common.SysError(fmt.Sprintf("failed to compensate reserved token quota: result=%d error=%v", compensated, compensateErr))
		}
		// 令牌不存在或数据库余额不足（守卫拒绝）：缓存已放行但 DB 不允许 →
		// 视为"预扣失败/额度不足"（非致命错误），调用方（PreConsumeTokenQuota）
		// 按额度不足处理，绝不把余额扣成负数（与用户钱包路径同一语义）。
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}
