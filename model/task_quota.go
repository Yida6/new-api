package model

import (
	"errors"
	"fmt"
	"strings"
	"sync/atomic"

	"github.com/QuantumNous/new-api/common"

	"gorm.io/gorm"
)

// errTaskQuotaRefundGuard 表示退款方向累计消耗（用户或渠道）不足以冲减，
// 用于整体回滚事务、避免把累计消耗冲成负数或产生部分写入分叉。
var errTaskQuotaRefundGuard = errors.New("task quota refund guard: insufficient cumulative usage")

// errTaskQuotaTokenRefundFailed 表示 Seedance 结算退款方向（delta<0）的 Token
// 退款失败（令牌缺失 / used_quota 不足以冲减）。退款方向的资金与 Token 必须
// 一起提交：Token 退不回去就不允许把资金退出去，整体回滚并等待重试
// （可恢复、可审计），绝不静默成功。
var errTaskQuotaTokenRefundFailed = errors.New("task quota token refund failed")

// postConsumeUserSubscriptionDeltaTx 在给定事务内调整订阅已用量（正增负减）。
// 与 PostConsumeUserSubscriptionDelta 语义一致，但接受外部事务以支持与其他写入原子提交。
func postConsumeUserSubscriptionDeltaTx(tx *gorm.DB, userSubscriptionId int, delta int64) error {
	if userSubscriptionId <= 0 {
		return errors.New("invalid userSubscriptionId")
	}
	if delta == 0 {
		return nil
	}
	var sub UserSubscription
	if err := lockForUpdate(tx).Where("id = ?", userSubscriptionId).First(&sub).Error; err != nil {
		return err
	}
	newUsed := sub.AmountUsed + delta
	if newUsed < 0 {
		newUsed = 0
	}
	if sub.AmountTotal > 0 && newUsed > sub.AmountTotal {
		return fmt.Errorf("subscription used exceeds total, used=%d total=%d", newUsed, sub.AmountTotal)
	}
	sub.AmountUsed = newUsed
	return tx.Save(&sub).Error
}

// applyUserUsedQuotaDelta 在事务内调整用户累计消耗；退款方向带 used_quota >= -delta 守卫。
func applyUserUsedQuotaDelta(tx *gorm.DB, userId int, delta int64) error {
	if delta < 0 {
		res := tx.Model(&User{}).Where("id = ? AND used_quota >= ?", userId, -delta).
			Update("used_quota", gorm.Expr("used_quota + ?", delta))
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected != 1 {
			return fmt.Errorf("%w: user_id=%d refund=%d", errTaskQuotaRefundGuard, userId, -delta)
		}
		return nil
	}
	return tx.Model(&User{}).Where("id = ?", userId).
		Update("used_quota", gorm.Expr("used_quota + ?", delta)).Error
}

// applyChannelUsedQuotaDelta 在事务内调整渠道累计消耗；退款方向带 used_quota >= -delta 守卫，
// 正方向要求渠道行存在（RowsAffected==1，渠道缺失时累计值写入落空，会让后续退款守卫失败）。
func applyChannelUsedQuotaDelta(tx *gorm.DB, channelId int, delta int64) error {
	if channelId <= 0 {
		// 无渠道（channel_id=0）不维护渠道累计消耗，直接跳过（与历史行为一致）。
		return nil
	}
	if delta < 0 {
		res := tx.Model(&Channel{}).Where("id = ? AND used_quota >= ?", channelId, -delta).
			Update("used_quota", gorm.Expr("used_quota + ?", delta))
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected != 1 {
			return fmt.Errorf("%w: channel_id=%d refund=%d", errTaskQuotaRefundGuard, channelId, -delta)
		}
		return nil
	}
	res := tx.Model(&Channel{}).Where("id = ?", channelId).
		Update("used_quota", gorm.Expr("used_quota + ?", delta))
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected != 1 {
		return fmt.Errorf("channel not found for used quota delta: channel_id=%d", channelId)
	}
	return nil
}

// ApplyTaskQuotaDelta 在单个事务内原子完成异步任务的一次计费调整：
//
//	资金（钱包/订阅）+ 用户累计消耗 + 渠道累计消耗 + 任务剩余额度。
//
// delta > 0 补扣、delta < 0 退款；退款方向用户与渠道累计消耗均带 used_quota >= -delta 守卫，
// 任一守卫失败或任一步失败则整体回滚返回 false，杜绝部分写入造成的永久分叉。
// 不修改 request_count（累计消耗的请求计数由提交时的消费日志单独维护）。
//
// 例外：任务标记 BillingStatsFailed（提交时累计统计写入失败，used_quota 从未累加）
// 时，结算/退款**两个方向**都跳过用户/渠道累计消耗调整——
//   - 退款方向（delta<0）：否则守卫（used_quota >= refund，此时 used_quota=0）永久
//     卡死，资金无法退还；
//   - 补扣方向（delta>0）：否则会把差额写进从未累加过的 used_quota，产生错误残值
//     （正确值应为实际总消耗 actualQuota，而非差额），且后续退款会残留虚假消耗。
//
// 统计缺失由人工对账（提交时已 SysError 告警），本函数只保证资金方向正确。
func ApplyTaskQuotaDelta(task *Task, delta int64, isSubscription bool) bool {
	return ApplyTaskQuotaDeltaGuarded(task, delta, isSubscription, TaskQuotaDeltaOptions{}).IsSuccess()
}

// TaskQuotaDeltaResult 带守卫结算的可识别结果（余额不足 / 用户不存在 / 数据库
// 错误必须可区分，不能折叠成普通成功或静默失败）。
type TaskQuotaDeltaResult int

const (
	TaskQuotaDeltaSuccess               TaskQuotaDeltaResult = iota // 结算成功
	TaskQuotaDeltaInsufficientBalance                                // 钱包余额不足（用户存在，未扣减）
	TaskQuotaDeltaUserNotFound                                       // 用户不存在（未扣减）
	TaskQuotaDeltaSubscriptionExceeded                               // 订阅额度超出总额度上限（未扣减）
	TaskQuotaDeltaDBError                                            // 数据库/守卫错误（整体回滚，可重试）
)

// IsSuccess 是否成功。
func (r TaskQuotaDeltaResult) IsSuccess() bool { return r == TaskQuotaDeltaSuccess }

// IsRetryable 是否可安全重试（数据库错误可重试；余额/用户/订阅问题重试无意义）。
func (r TaskQuotaDeltaResult) IsRetryable() bool { return r == TaskQuotaDeltaDBError }

// String 可读名称（日志/欠款原因用）。
func (r TaskQuotaDeltaResult) String() string {
	switch r {
	case TaskQuotaDeltaSuccess:
		return "success"
	case TaskQuotaDeltaInsufficientBalance:
		return "insufficient_balance"
	case TaskQuotaDeltaUserNotFound:
		return "user_not_found"
	case TaskQuotaDeltaSubscriptionExceeded:
		return "subscription_exceeded"
	case TaskQuotaDeltaDBError:
		return "db_error"
	default:
		return "unknown"
	}
}

// TaskQuotaDeltaOptions 结算策略参数（显式隔离，避免一刀切改变其他调用方语义）。
type TaskQuotaDeltaOptions struct {
	// GuardPositiveDelta 为 true 时，钱包补扣方向（delta>0）执行原子条件更新
	// （quota >= delta 才扣，RowsAffected==1 才成功）；false 时保持通用语义
	// （无条件扣减——仓库现有分层计费/组升级路径存在"允许欠费"的兼容行为，
	// 必须由调用方显式选择守卫，默认不改变其语义）。
	GuardPositiveDelta bool
}

// 带守卫结算的事务内哨兵错误。
var (
	errTaskQuotaInsufficientBalance = errors.New("task quota insufficient balance")
	errTaskQuotaUserNotFound        = errors.New("task quota user not found")
	errTaskQuotaSubscriptionExceeded = errors.New("task quota subscription exceeded total")
)

// ApplyTaskQuotaDeltaGuarded 带守卫的异步任务计费调整（Seedance 专用结算路径）。
// 语义与 ApplyTaskQuotaDelta 一致，区别：
//   - opts.GuardPositiveDelta 时钱包补扣走单条条件 SQL：`UPDATE users SET
//     quota = quota - ? WHERE id = ? AND quota >= ?` 且严格校验 RowsAffected==1。
//     余额不足、用户不存在、数据库错误返回可识别结果，绝不把钱包扣成负数。
//   - 数据库守卫是最终资金边界；Redis 缓存只优化授权（TryReserveUserQuota 的
//     缓存路径也会在落账时再走数据库守卫）。事务提交后再同步/失效钱包缓存。
//   - 订阅补扣维持既有 AmountTotal 上限守卫，超限返回 SubscriptionExceeded。
//   - 事务失败时内存 task.Quota 不被污染（占位模型 UPDATE），成功提交后才更新。
func ApplyTaskQuotaDeltaGuarded(task *Task, delta int64, isSubscription bool, opts TaskQuotaDeltaOptions) TaskQuotaDeltaResult {
	if delta == 0 {
		return TaskQuotaDeltaSuccess
	}
	billingStatsFailed := task.PrivateData.BillingStatsFailed
	err := DB.Transaction(func(tx *gorm.DB) error {
		// 1. 资金调整（钱包或订阅）
		if isSubscription {
			if err := postConsumeUserSubscriptionDeltaTx(tx, task.PrivateData.SubscriptionId, int64(delta)); err != nil {
				if strings.Contains(err.Error(), "subscription used exceeds total") {
					return errTaskQuotaSubscriptionExceeded
				}
				return err
			}
		} else if delta > 0 && opts.GuardPositiveDelta {
			// 原子条件补扣：quota >= delta 才扣，RowsAffected==1 才成功。
			res := tx.Model(&User{}).
				Where("id = ? AND quota >= ?", task.UserId, delta).
				Update("quota", gorm.Expr("quota - ?", delta))
			if res.Error != nil {
				return res.Error
			}
			if res.RowsAffected != 1 {
				// 余额不足 或 用户不存在：查证后区分返回。
				var user User
				qErr := tx.Select("id").Where("id = ?", task.UserId).First(&user).Error
				if errors.Is(qErr, gorm.ErrRecordNotFound) {
					return errTaskQuotaUserNotFound
				}
				if qErr != nil {
					return qErr
				}
				return errTaskQuotaInsufficientBalance
			}
		} else if err := tx.Model(&User{}).Where("id = ?", task.UserId).
			Update("quota", gorm.Expr("quota - ?", delta)).Error; err != nil {
			return err
		}
		// 2. 用户累计消耗（统计未写入的任务双向跳过调整）
		if !billingStatsFailed {
			if err := applyUserUsedQuotaDelta(tx, task.UserId, delta); err != nil {
				return err
			}
		}
		// 3. 渠道累计消耗（同上）
		if !billingStatsFailed {
			if err := applyChannelUsedQuotaDelta(tx, task.ChannelId, delta); err != nil {
				return err
			}
		}
		// 4. 任务剩余额度（占位模型更新，失败不污染内存对象，见 ApplyTaskQuotaDelta 注释）
		return applyTaskQuotaDeltaTx(tx, task, delta)
	})
	if err != nil {
		switch {
		case errors.Is(err, errTaskQuotaInsufficientBalance):
			return TaskQuotaDeltaInsufficientBalance
		case errors.Is(err, errTaskQuotaUserNotFound):
			return TaskQuotaDeltaUserNotFound
		case errors.Is(err, errTaskQuotaSubscriptionExceeded):
			return TaskQuotaDeltaSubscriptionExceeded
		case errors.Is(err, errTaskQuotaRefundGuard):
			common.SysError(fmt.Sprintf("task quota refund guard failed: %v；请运行历史修复脚本对账", err))
			return TaskQuotaDeltaDBError
		default:
			common.SysLog(fmt.Sprintf("apply guarded task quota delta failed: task_id=%s delta=%d error=%v", task.TaskID, delta, err))
			return TaskQuotaDeltaDBError
		}
	}
	// 事务提交成功后才更新内存对象；失败路径保持原值（与数据库一致）。
	task.Quota += delta
	// 交易提交成功后同步钱包 Redis 缓存余额（订阅不涉及用户钱包余额缓存）。
	if !isSubscription {
		syncWalletQuotaCacheAfterCommit(task.UserId, -int64(delta), "guarded task delta")
	}
	return TaskQuotaDeltaSuccess
}

// applyTaskQuotaDeltaTx 更新任务剩余额度并严格校验行命中数（任务被删除时
// 不得静默成功）。使用占位值避免 GORM 回写污染内存对象。
func applyTaskQuotaDeltaTx(tx *gorm.DB, task *Task, delta int64) error {
	newQuota := task.Quota + delta
	res := tx.Model(&Task{}).Where("id = ?", task.ID).Update("quota", newQuota)
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected != 1 {
		return fmt.Errorf("task not found for quota delta: task_id=%d", task.ID)
	}
	return nil
}

// ---------------------------------------------------------------------------
// TokenAdjustResult — 结算时 Token 额度调整结果
// ---------------------------------------------------------------------------

type TokenAdjustResult int

const (
	TokenAdjustNotApplicable TokenAdjustResult = iota // 任务无 Token（TokenId<=0）
	TokenAdjustOK                                      // Token 扣减/退还成功（同事务）
	TokenAdjustFailed                                  // Token 扣减失败（不足/令牌缺失/DB错误）
)

// ApplySeedanceSettle Seedance 专用结算（与 ApplyTaskQuotaDeltaGuarded 同语义），
// 额外在同一事务内调整 Token 额度（delta>0 扣减带 remain_quota >= delta 守卫，
// delta<0 退款带 used_quota >= abs(delta) 守卫；无限令牌跳过守卫）：
//
//   - delta>0 且 Token 调整失败：在同一事务内原子累加 tasks.token_delta_pending
//     ——资金、用户/渠道累计、task.Quota 与 pending **一起提交**，杜绝
//     "资金已提交但 pending 未落库"的崩溃窗口（service 层不再依赖提交后的
//     第二次数据库写入来保证恢复信息）；事务提交后才同步内存
//     task.TokenDeltaPending。
//   - delta<0 且 Token 退款失败：绝不静默成功——整笔退款事务回滚并等待重试
//     （可恢复、可审计）。
//
// 事务提交成功后：同步钱包 Redis 缓存（钱包资金来源）与 Token Redis 缓存
// （Token 成功调整时）；缓存操作失败不回滚已提交资金，只记录错误并尝试失效。
func ApplySeedanceSettle(task *Task, delta int64, isSubscription bool, opts TaskQuotaDeltaOptions) (TaskQuotaDeltaResult, TokenAdjustResult) {
	if delta == 0 {
		return TaskQuotaDeltaSuccess, TokenAdjustNotApplicable
	}
	billingStatsFailed := task.PrivateData.BillingStatsFailed
	tokenResult := TokenAdjustNotApplicable
	var tokenKey string // 事务内取得的 Token key（问题三：提交后直接用它同步缓存）
	err := DB.Transaction(func(tx *gorm.DB) error {
		// 1. 资金（同 ApplyTaskQuotaDeltaGuarded）
		if isSubscription {
			if err := postConsumeUserSubscriptionDeltaTx(tx, task.PrivateData.SubscriptionId, int64(delta)); err != nil {
				if strings.Contains(err.Error(), "subscription used exceeds total") {
					return errTaskQuotaSubscriptionExceeded
				}
				return err
			}
		} else if delta > 0 && opts.GuardPositiveDelta {
			res := tx.Model(&User{}).
				Where("id = ? AND quota >= ?", task.UserId, delta).
				Update("quota", gorm.Expr("quota - ?", delta))
			if res.Error != nil {
				return res.Error
			}
			if res.RowsAffected != 1 {
				var user User
				qErr := tx.Select("id").Where("id = ?", task.UserId).First(&user).Error
				if errors.Is(qErr, gorm.ErrRecordNotFound) {
					return errTaskQuotaUserNotFound
				}
				if qErr != nil {
					return qErr
				}
				return errTaskQuotaInsufficientBalance
			}
		} else if err := tx.Model(&User{}).Where("id = ?", task.UserId).
			Update("quota", gorm.Expr("quota - ?", delta)).Error; err != nil {
			return err
		}
		// 2/3. 用户与渠道累计
		if !billingStatsFailed {
			if err := applyUserUsedQuotaDelta(tx, task.UserId, delta); err != nil {
				return err
			}
			if err := applyChannelUsedQuotaDelta(tx, task.ChannelId, delta); err != nil {
				return err
			}
		}
		// 4. 任务额度
		if err := applyTaskQuotaDeltaTx(tx, task, delta); err != nil {
			return err
		}
		// 5. Token 额度（与资金同一事务；见函数头注释的崩溃窗口消除语义）
		if task.PrivateData.TokenId > 0 {
			ok, key := applyTokenQuotaDeltaTx(tx, task.PrivateData.TokenId, delta)
			tokenKey = key // 问题三：事务内取得 key，提交后直接同步缓存
			if ok {
				tokenResult = TokenAdjustOK
			} else if delta > 0 {
				// 补扣失败：同一事务内原子累加待补偿标记（资金已收、Token
				// 未扣的可审计恢复信息），与资金/累计/task.Quota 一起提交。
				if err := addTaskTokenDeltaPendingTx(tx, task.ID, delta); err != nil {
					return err
				}
				tokenResult = TokenAdjustFailed
			} else {
				// 退款失败（令牌缺失/used_quota 不足）：整体回滚，等待重试。
				return errTaskQuotaTokenRefundFailed
			}
		}
		return nil
	})
	if err != nil {
		switch {
		case errors.Is(err, errTaskQuotaInsufficientBalance):
			return TaskQuotaDeltaInsufficientBalance, tokenResult
		case errors.Is(err, errTaskQuotaUserNotFound):
			return TaskQuotaDeltaUserNotFound, tokenResult
		case errors.Is(err, errTaskQuotaSubscriptionExceeded):
			return TaskQuotaDeltaSubscriptionExceeded, tokenResult
		case errors.Is(err, errTaskQuotaRefundGuard):
			common.SysError(fmt.Sprintf("task quota refund guard failed: %v；请运行历史修复脚本对账", err))
			return TaskQuotaDeltaDBError, tokenResult
		case errors.Is(err, errTaskQuotaTokenRefundFailed):
			common.SysError(fmt.Sprintf("seedance settle token refund failed: task_id=%s delta=%d；资金与 Token 已整体回滚，等待重试/人工对账", task.TaskID, delta))
			return TaskQuotaDeltaDBError, tokenResult
		default:
			common.SysLog(fmt.Sprintf("apply seedance settle failed: task_id=%s delta=%d error=%v", task.TaskID, delta, err))
			return TaskQuotaDeltaDBError, tokenResult
		}
	}
	task.Quota += delta
	// 补扣且 Token 未扣：事务已把 pending 落库，此处只同步内存对象。
	if tokenResult == TokenAdjustFailed && delta > 0 {
		task.TokenDeltaPending += delta
	}
	if !isSubscription {
		syncWalletQuotaCacheAfterCommit(task.UserId, -int64(delta), "seedance settle")
	}
	// Token 已成功调整（DB 已改）→ 同步/失效 Token Redis 缓存（问题三：直接用
	// 事务内取得的 key，绝不依赖提交后的第二次 GetTokenById）。
	if tokenResult == TokenAdjustOK && task.PrivateData.TokenId > 0 {
		syncTokenQuotaCacheAfterCommitWithKey(task.PrivateData.TokenId, tokenKey, -int64(delta), "seedance settle")
	}
	return TaskQuotaDeltaSuccess, tokenResult
}

// applyTokenQuotaDeltaTx 在事务内调整令牌额度（与资金同一事务原子提交）：
//   - 先读取令牌行（id、key、unlimited_quota）；令牌不存在一律视为调整失败
//     （RowsAffected==1 校验：Token 不存在绝不能当作成功）；
//   - 有限额度令牌：
//     - delta>0 补扣：带 remain_quota >= delta 条件守卫（RowsAffected==1 才成功）；
//     - delta<0 退款：remain_quota += abs(delta)、used_quota -= abs(delta)，
//       带 used_quota >= abs(delta) 条件守卫（used_quota 绝不减成负数）；
//   - 无限额度令牌：跳过余额守卫，无条件记录 remain/used 变化（与
//     TryReserveTokenQuota 的既有语义一致——无限 Token 是"允许无余额守卫地
//     记录 remain/used 变化"，不是跳过额度统计）；
//   - 无论方向，remain_quota + used_quota 在扣减/退款前后保持不变。
//
// 返回值：ok 是否成功调整；tokenKey 为事务内取得的令牌 Key（供提交后直接
// 同步 Redis 缓存——绝不依赖提交后的第二次 GetTokenById，见问题三）。
// 调用方语义：
//   - delta>0 失败 → 调用方在同一事务内累加 tasks.token_delta_pending 待补偿；
//   - delta<0 失败 → 调用方应整体回滚事务（资金与 Token 不可分叉）。
func applyTokenQuotaDeltaTx(tx *gorm.DB, tokenId int, delta int64) (ok bool, tokenKey string) {
	if tokenId <= 0 || delta == 0 {
		return true, ""
	}
	var tok Token
	if err := tx.Select("id", "key", "unlimited_quota").Where("id = ?", tokenId).First(&tok).Error; err != nil {
		return false, "" // 令牌缺失：不得当作成功
	}
	tokenKey = tok.Key
	now := common.GetTimestamp()
	if delta > 0 {
		if tok.UnlimitedQuota {
			// 无限令牌：无条件扣减（跳过 remain_quota 守卫）
			res := tx.Model(&Token{}).Where("id = ?", tokenId).
				Updates(map[string]interface{}{
					"remain_quota":  gorm.Expr("remain_quota - ?", delta),
					"used_quota":    gorm.Expr("used_quota + ?", delta),
					"accessed_time": now,
				})
			return res.Error == nil && res.RowsAffected == 1, tokenKey
		}
		res := tx.Model(&Token{}).
			Where("id = ? AND remain_quota >= ?", tokenId, delta).
			Updates(map[string]interface{}{
				"remain_quota":  gorm.Expr("remain_quota - ?", delta),
				"used_quota":    gorm.Expr("used_quota + ?", delta),
				"accessed_time": now,
			})
		return res.Error == nil && res.RowsAffected == 1, tokenKey
	}
	// delta < 0：退款。remain_quota 加回 abs(delta)，used_quota 冲减 abs(delta)。
	absDelta := -delta
	if tok.UnlimitedQuota {
		// 无限令牌：退款方向同样不设 used_quota 下限（与 IncreaseTokenQuota
		// 语义一致——无限令牌的 remain/used 本就是名义记账）。
		res := tx.Model(&Token{}).Where("id = ?", tokenId).
			Updates(map[string]interface{}{
				"remain_quota":  gorm.Expr("remain_quota + ?", absDelta),
				"used_quota":    gorm.Expr("used_quota - ?", absDelta),
				"accessed_time": now,
			})
		return res.Error == nil && res.RowsAffected == 1, tokenKey
	}
	res := tx.Model(&Token{}).
		Where("id = ? AND used_quota >= ?", tokenId, absDelta).
		Updates(map[string]interface{}{
			"remain_quota":  gorm.Expr("remain_quota + ?", absDelta),
			"used_quota":    gorm.Expr("used_quota - ?", absDelta),
			"accessed_time": now,
		})
	return res.Error == nil && res.RowsAffected == 1, tokenKey
}

// ApplyWalletRefundUsedQuota 在单个事务内原子完成钱包退款 + 用户/渠道累计消耗冲减。
// 用于 Midjourney 构图失败退款：退款方向用户与渠道累计消耗均带 used_quota >= refund 守卫，
// 任一守卫失败或任一步失败则整体回滚返回 false。
// statsRecorded 表示提交时累计统计是否已写入：统计写入失败（BillingStatsFailed）时
// used_quota 从未累加，必须跳过冲减，否则守卫永久卡死。
func ApplyWalletRefundUsedQuota(userId int, channelId int, refund int64, statsRecorded bool) bool {
	if refund <= 0 {
		return true
	}
	err := DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Model(&User{}).Where("id = ?", userId).
			Update("quota", gorm.Expr("quota + ?", refund)).Error; err != nil {
			return err
		}
		if statsRecorded {
			if err := applyUserUsedQuotaDelta(tx, userId, -refund); err != nil {
				return err
			}
			return applyChannelUsedQuotaDelta(tx, channelId, -refund)
		}
		return nil
	})
	if err != nil {
		if errors.Is(err, errTaskQuotaRefundGuard) {
			common.SysError(fmt.Sprintf("midjourney refund guard failed: %v；请运行历史修复脚本对账", err))
		} else {
			common.SysLog(fmt.Sprintf("midjourney refund failed: user_id=%d channel_id=%d refund=%d error=%v", userId, channelId, refund, err))
		}
		return false
	}
	// 同步钱包 Redis 缓存余额，避免退款后缓存仍为旧余额导致额度暂时不可用。
	syncWalletQuotaCacheAfterCommit(userId, int64(refund), "midjourney refund")
	return true
}

// walletQuotaCacheSyncAttempts 记录钱包 Redis 缓存同步的尝试次数（仅测试
// 观测用：订阅资金来源的结算**不得**触发钱包缓存同步，见
// TestSettleSubscriptionDoesNotTouchWalletCache；生产行为不受影响）。
var walletQuotaCacheSyncAttempts atomic.Int64

// syncWalletQuotaCacheAfterCommit 在资金事务提交后把增量同步进钱包 Redis 缓存。
//
// 已知窗口：事务提交到缓存同步之间存在"数据库已扣减、缓存仍为旧余额"的间隙，
// TryReserveUserQuota 按缓存授权时会短暂按旧值放行。资金正确性由数据库层守卫兜底：
// 授权落账（persistUserQuotaDelta）带 quota >= 扣减额 条件，余额不足时拒绝扣减并
// 补偿缓存——窗口内的并发请求只会被拒绝（可重试），绝不会把余额扣成负数
// （见 model/quota_reserve.go）。同步失败时仍删除缓存键，避免旧余额长期滞留。
// 注意：仅钱包资金来源调用；订阅结算不经过钱包余额，绝不能修改钱包缓存。
func syncWalletQuotaCacheAfterCommit(userId int, delta int64, op string) {
	walletQuotaCacheSyncAttempts.Add(1)
	if !common.RedisEnabled {
		return
	}
	if err := cacheIncrUserQuota(userId, delta); err != nil {
		common.SysLog(fmt.Sprintf("failed to sync user quota cache after %s: user_id=%d delta=%d error=%v，删除缓存键以强制下次从数据库水合", op, userId, delta, err))
		if invErr := invalidateUserCache(userId); invErr != nil {
			common.SysError(fmt.Sprintf("failed to invalidate user quota cache after %s: user_id=%d error=%v，请人工处理", op, userId, invErr))
		}
	}
}

// ApplyPreConsumeUsedQuota 在单个事务内原子完成异步任务预扣的累计消耗写入：
// 用户 used_quota + request_count + 渠道 used_quota。
// 用户/渠道若分两次更新，任意一方失败后任务仍会插入，后续退款会因渠道守卫失败而永久卡住；
// 此处统一在事务内提交，任一步失败整体回滚并返回错误，由调用方决定是否终止任务提交。
//
// 行存在性校验（RowsAffected）：GORM 的 UPDATE 在目标行不存在时无错误但影响 0 行，
// 若不检查会把"用户/渠道不存在"当成"统计写入成功"——任务照常插入但累计值缺失，
// 后续退款守卫（used_quota >= refund）永久失败。因此用户行必须命中（RowsAffected==1），
// 渠道行（channelId>0）由 applyChannelUsedQuotaDelta 校验，缺失即返回错误。
func ApplyPreConsumeUsedQuota(userId int, channelId int, quota int64) error {
	return DB.Transaction(func(tx *gorm.DB) error {
		res := tx.Model(&User{}).Where("id = ?", userId).Updates(map[string]interface{}{
			"used_quota":    gorm.Expr("used_quota + ?", quota),
			"request_count": gorm.Expr("request_count + ?", 1),
		})
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected != 1 {
			return fmt.Errorf("user not found for pre-consume used quota: user_id=%d", userId)
		}
		return applyChannelUsedQuotaDelta(tx, channelId, quota)
	})
}

// ---------------------------------------------------------------------------
// TokenDeltaPending 幂等补偿（资金已收、Token 未扣的持久化恢复）
// ---------------------------------------------------------------------------

// GetTasksWithPendingTokenDelta 查询存在待补偿 Token 差额的任务（限 limit 条）。
func GetTasksWithPendingTokenDelta(limit int) ([]*Task, error) {
	if limit <= 0 {
		limit = 100
	}
	var tasks []*Task
	err := DB.Where("token_delta_pending > 0").Order("id").Limit(limit).Find(&tasks).Error
	return tasks, err
}

// HasTasksWithPendingTokenDelta 是否存在待补偿 Token 差额的任务
// （系统任务 Enabled 用：空闲系统不产生任务行）。
func HasTasksWithPendingTokenDelta() bool {
	var id int64
	err := DB.Model(&Task{}).
		Where("token_delta_pending > 0").
		Limit(1).
		Pluck("id", &id).Error
	return err == nil && id != 0
}

// CompensatePendingTokenDeltas 幂等补偿待扣 Token 差额：对每个任务在事务内
// 条件扣减 Token（有限令牌 remain_quota >= pending；无限令牌无条件扣减——
// 与 applyTokenQuotaDeltaTx 同一语义）并清零任务标记；Token 余额不足/
// 令牌缺失/DB 错误时保留标记（下次再试），绝不重复扣款（成功清零是唯一出路）。
// 事务提交成功后同步 Token Redis 缓存。返回本次成功补偿的任务数。
func CompensatePendingTokenDeltas(limit int) (int, error) {
	tasks, err := GetTasksWithPendingTokenDelta(limit)
	if err != nil {
		return 0, err
	}
	compensated := 0
	for _, task := range tasks {
		pending := task.TokenDeltaPending
		if pending <= 0 || task.PrivateData.TokenId <= 0 {
			// 无令牌可扣：保留 pending 作为恢复证据并持续告警，绝不静默清零
			// （问题四：pending>0 且 TokenId<=0 属数据异常，清零会永久丢失
			// 恢复证据；管理端可通过任务查询接口看到 token_delta_pending）。
			if pending > 0 {
				common.SysError(fmt.Sprintf("token delta pending but no token: task_id=%s pending=%d；保留 pending 等待人工处理（管理端可查询），不自动清零", task.TaskID, pending))
			}
			continue
		}
		var tokenKey string // 问题三：事务内取得的 Token key
		err := DB.Transaction(func(tx *gorm.DB) error {
			// 条件扣减（有限令牌带 remain_quota >= pending 守卫），失败保留标记
			ok, key := applyTokenQuotaDeltaTx(tx, task.PrivateData.TokenId, pending)
			tokenKey = key
			if !ok {
				return errTokenDeltaCompensateSkipped
			}
			// 清零标记（仅当本次真正扣减成功）
			clear := tx.Model(&Task{}).
				Where("id = ? AND token_delta_pending = ?", task.ID, pending).
				Update("token_delta_pending", 0)
			if clear.Error != nil {
				return clear.Error
			}
			if clear.RowsAffected != 1 {
				return errTokenDeltaCompensateSkipped // 并发已处理/标记变化 → 放弃
			}
			return nil
		})
		if err != nil {
			if errors.Is(err, errTokenDeltaCompensateSkipped) {
				continue // 余额不足等：保留标记，下轮再试
			}
			common.SysLog(fmt.Sprintf("compensate token delta failed: task_id=%s pending=%d error=%v", task.TaskID, pending, err))
			continue
		}
		compensated++
		// 事务提交成功后才同步 Token Redis 缓存（DB 已扣，缓存需一致；
		// 直接用事务内取得的 key，见问题三）。
		syncTokenQuotaCacheAfterCommitWithKey(task.PrivateData.TokenId, tokenKey, -int64(pending), "pending compensate")
	}
	return compensated, nil
}

// errTokenDeltaCompensateSkipped 补偿因余额不足/并发处理被跳过（保留标记重试）。
var errTokenDeltaCompensateSkipped = errors.New("token delta compensate skipped")

// addTaskTokenDeltaPendingTx 在给定事务内原子累加任务的待补偿 Token 差额
// （结算与 pending 落库同一事务提交，杜绝崩溃窗口；见 ApplySeedanceSettle）。
func addTaskTokenDeltaPendingTx(tx *gorm.DB, taskID int64, delta int64) error {
	if taskID <= 0 || delta <= 0 {
		return nil
	}
	res := tx.Model(&Task{}).
		Where("id = ?", taskID).
		Update("token_delta_pending", gorm.Expr("token_delta_pending + ?", delta))
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected != 1 {
		return fmt.Errorf("task not found for token delta pending: task_id=%d", taskID)
	}
	return nil
}

// AddTaskTokenDeltaPending 原子累加任务的待补偿 Token 差额（幂等：同一任务
// 重复结算不会重复累计，因为结算成功后任务即终态不再重入；此处仍用原子
// UPDATE 防止并发补偿与结算交错）。返回更新后的标记值。
// 注意：Seedance 结算路径（ApplySeedanceSettle）已把 pending 与资金放在同一
// 事务提交，本函数只保留给显式修复/管理工具使用，绝不再作为结算的恢复依据。
func AddTaskTokenDeltaPending(taskID int64, delta int64) error {
	return addTaskTokenDeltaPendingTx(DB, taskID, delta)
}

// syncTokenQuotaCacheAfterCommitWithKey 在 Token 额度事务提交后用**事务内取得
// 的 key** 直接同步 Redis 缓存（问题三：绝不依赖提交后的第二次数据库查询）。
// delta 是缓存语义增量（缓存哈希 remain += delta、used -= delta）：
// 补扣 q → delta=-q；退款 q → delta=+q。
//
// 只允许在事务成功提交后调用；缓存操作失败绝不能回滚已提交资金，而是记录
// 错误并删除缓存键（invalidateTokenCacheForMutation），让下次读取回源数据库
// （保证数据库与缓存不会长期保留不同额度）。tokenKey 为空（令牌已删除/事务
// 内查询失败）时无法定位缓存键，直接跳过——令牌已删除时缓存随令牌生命周期
// 失效（Delete 前已 invalidateTokenCacheForMutation），不产生长期分叉。
func syncTokenQuotaCacheAfterCommitWithKey(tokenId int, tokenKey string, delta int64, op string) {
	if !common.RedisEnabled || tokenId <= 0 {
		return
	}
	if tokenKey == "" {
		return
	}
	res, err := cacheApplyTokenQuotaDelta(tokenId, tokenKey, delta)
	if err != nil || res != cacheQuotaOK {
		common.SysLog(fmt.Sprintf("failed to sync token quota cache after %s: token_id=%d delta=%d res=%d error=%v，删除缓存键强制下次回源", op, tokenId, delta, res, err))
		if invErr := invalidateTokenCacheForMutation(tokenKey); invErr != nil {
			common.SysError(fmt.Sprintf("failed to invalidate token quota cache after %s: token_id=%d error=%v，请人工处理", op, tokenId, invErr))
		}
	}
}

// syncTokenQuotaCacheAfterCommit 兼容旧调用：通过提交后的 GetTokenById 解析
// key 再同步缓存。问题三：该路径依赖提交后的第二次数据库查询，查询失败时
// 无法删除已知 Token 的缓存键、会留下旧缓存；新代码一律使用
// syncTokenQuotaCacheAfterCommitWithKey（资金事务内取得并保留 key）。
func syncTokenQuotaCacheAfterCommit(tokenId int, delta int64, op string) {
	if !common.RedisEnabled || tokenId <= 0 {
		return
	}
	token, err := GetTokenById(tokenId)
	if err != nil {
		common.SysLog(fmt.Sprintf("failed to resolve token for cache sync after %s: token_id=%d error=%v", op, tokenId, err))
		return
	}
	syncTokenQuotaCacheAfterCommitWithKey(tokenId, token.Key, delta, op)
}
