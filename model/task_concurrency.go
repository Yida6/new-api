package model

import (
	"strconv"
	"time"

	"github.com/QuantumNous/new-api/constant"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// ---------------------------------------------------------------------------
// 单用户 Seedance 任务并发名额（跨实例原子计数）
//
// 目标：单个用户最多同时运行 MAX_CONCURRENT_TASKS 个 Seedance 任务。
//  "运行中" = 非终态任务（queued / processing，含 NOT_START / SUBMITTED 等
//  创建后尚未结束的中间态）；completed / failed / cancelled（如有）不占用。
//
// 并发安全（多实例部署）：
//   - 名额计数持久化在共享数据库表中，每个用户一行（user_id 主键）；
//   - 预留（Reserve）在数据库事务内对用户行加行锁（SELECT ... FOR UPDATE，
//     见 lockForUpdate），串行执行「检查上限 → 递增」，杜绝并发请求同时通过
//     检查导致绕过限制；
//   - 释放（Release）同样在事务内加锁后递减（下限 0）。
//
// 生命周期：
//   创建请求进入重试循环、确定渠道为 Seedance 家族后先预留名额（+1）；
//   任务行创建成功即视为"名额已转移给任务"——由轮询器在任务到达终态
//   （成功/失败/超时）时释放；若请求失败未创建任务行，则由控制器 defer
//   立即释放。
//
// 兜底清理（异常退出）：进程崩溃/未捕获 panic 可能导致预留了名额但既未创建
// 任务行、也未执行释放。ReconcileTaskConcurrencySlots 周期性对账：把
//  stale 超过阈值（updated_at 很久未更新，说明无在途预留）的计数修正为
//  真实运行任务数；计数低于真实值（如重复释放）则无条件回补。正常运行的
//  任务计数与真实值一致（预留转移后 1:1），对账不会误伤在途预留。
// ---------------------------------------------------------------------------

// TaskRunningStatuses 占用并发名额的任务状态集合。
// 即所有"非终态"状态：任务只要尚未结束（含创建后未开始/已提交/排队/处理中/
// 状态未知），就占一个名额；只有终态（SUCCESS/FAILURE）才释放。
var TaskRunningStatuses = []TaskStatus{
	TaskStatusNotStart,
	TaskStatusSubmitted,
	TaskStatusQueued,
	TaskStatusInProgress,
	TaskStatusUnknown,
}

// IsTaskRunningStatus 判断状态是否占用并发名额（非终态即占用）。
func IsTaskRunningStatus(s TaskStatus) bool {
	for _, running := range TaskRunningStatuses {
		if running == s {
			return true
		}
	}
	return false
}

// seedancePlatformStrings 属于 Seedance 家族的渠道类型字符串（Task.Platform 即
// strconv.Itoa(channelType) 形式，见 relay.GetTaskPlatform）。
var seedancePlatformStrings = func() map[string]bool {
	m := make(map[string]bool, 2)
	for _, ct := range []int{constant.ChannelTypeDoubaoVideo, constant.ChannelTypeVolcEngine} {
		m[strconv.Itoa(ct)] = true
	}
	return m
}()

// IsSeedanceTaskPlatform 判断任务平台是否为 Seedance 家族（doubao/volcengine 视频渠道）。
func IsSeedanceTaskPlatform(platform constant.TaskPlatform) bool {
	_, ok := seedancePlatformStrings[string(platform)]
	return ok
}

// TaskConcurrencySlot 单用户 Seedance 并发名额计数行。
// 一个用户一行（不分渠道），保证"单个用户总量"限制语义。
type TaskConcurrencySlot struct {
	UserId       int   `json:"user_id" gorm:"primaryKey;autoIncrement:false"`
	RunningCount int   `json:"running_count" gorm:"not null;default:0"`
	UpdatedAt    int64 `json:"updated_at" gorm:"index"` // 秒级时间戳，用于兜底对账的 stale 判定
}

func (TaskConcurrencySlot) TableName() string { return "task_concurrency_slots" }

// ReserveTaskConcurrencySlot 原子预留一个并发名额。
// 返回 (true, current) 表示预留成功（current 为预留后的计数）；
// 返回 (false, current) 表示已达上限，current 为当前占用数（用于 429 文案）。
// limit <= 0 视为不限制（直接返回 true，不落库计数）。
//
// 事务语义：
//  1. 保底建行（INSERT ... ON CONFLICT DO NOTHING）：行不存在则创建，存在则不动
//     —— 关键：绝不在检查上限前刷新 updated_at。否则被 429 拒绝的请求每次重试
//     都会把行刷新为"新鲜"，泄漏名额将永远无法被对账清理（用户被永久锁死）。
//  2. 行锁（SELECT ... FOR UPDATE，见 lockForUpdate）串行化同一用户的并发预留；
//  3. 锁内用真实运行任务数补齐计数缺口（存量运行任务 / 历史重复释放导致的偏低），
//     保证"硬限制"从第一次预留起就对全部运行任务生效，不依赖定时对账；
//  4. 检查上限 → 递增 + 刷新 updated_at（仅预留成功才刷新）。
func ReserveTaskConcurrencySlot(userId, limit int) (bool, int, error) {
	if limit <= 0 {
		return true, 0, nil // 未启用限制
	}
	now := time.Now().Unix()
	tx := DB.Begin()
	if tx.Error != nil {
		return false, 0, tx.Error
	}
	defer func() { _ = tx.Rollback() }()

	// 保底建行：存在则 no-op（不更新任何列，不刷新 updated_at）。
	// MySQL 下冲突检查会等待持有该行锁的事务，天然串行化首个预留。
	if err := tx.Clauses(clause.OnConflict{DoNothing: true}).
		Create(&TaskConcurrencySlot{UserId: userId, RunningCount: 0, UpdatedAt: now}).Error; err != nil {
		return false, 0, err
	}

	var slot TaskConcurrencySlot
	if err := lockForUpdate(tx).Where("user_id = ?", userId).First(&slot).Error; err != nil {
		return false, 0, err
	}

	// 补齐缺口：真实运行任务数（存量任务/重复释放导致计数偏低）不低于计数。
	// 锁内查询保证与预留/释放互斥，读取一致快照。
	if actual, err := countRunningSeedanceTasks(tx, userId); err != nil {
		return false, 0, err
	} else if slot.RunningCount < int(actual) {
		slot.RunningCount = int(actual)
	}
	if slot.RunningCount >= limit {
		return false, slot.RunningCount, nil
	}
	if err := tx.Model(&TaskConcurrencySlot{}).Where("user_id = ?", userId).
		Updates(map[string]any{"running_count": slot.RunningCount + 1, "updated_at": now}).Error; err != nil {
		return false, 0, err
	}
	if err := tx.Commit().Error; err != nil {
		return false, 0, err
	}
	return true, slot.RunningCount + 1, nil
}

// ReleaseTaskConcurrencySlot 原子释放一个并发名额（下限 0）。
// 名额未预留（行不存在）时视为 no-op。
// 注意：这是"按用户"的无条件释放，仅用于请求级 defer（名额尚未转移给任务/
// 恢复记录时）。任务到达终态的释放必须走 MarkTaskSlotReleasedAndDecrement
// （标记 + 递减同事务，幂等），避免同一任务被多个终态路径重复释放。
func ReleaseTaskConcurrencySlot(userId int) error {
	tx := DB.Begin()
	if tx.Error != nil {
		return tx.Error
	}
	defer func() { _ = tx.Rollback() }()

	var slot TaskConcurrencySlot
	err := lockForUpdate(tx).Where("user_id = ?", userId).First(&slot).Error
	if err == gorm.ErrRecordNotFound {
		return tx.Commit().Error
	}
	if err != nil {
		return err
	}
	if slot.RunningCount > 0 {
		if err := tx.Model(&TaskConcurrencySlot{}).Where("user_id = ?", userId).
			Updates(map[string]any{
				"running_count": slot.RunningCount - 1,
				"updated_at":    time.Now().Unix(),
			}).Error; err != nil {
			return err
		}
	}
	return tx.Commit().Error
}

// MarkTaskSlotReleasedAndDecrement 原子地完成"任务级释放标记 + 计数递减"，
// 两者在同一数据库事务内一次性提交（不落中间状态）：
//  1. 锁用户计数行（SELECT ... FOR UPDATE；行不存在则无名额可减）；
//  2. 条件更新任务标记 concurrency_released false→true；
//  3. 仅当标记抢占成功（RowsAffected==1）才递减计数。
//
// 原子性保证：若递减失败或进程在事务中途退出，标记与计数都未提交（回滚），
// 后续终态路径可再次调用重试——不会出现"标记已置 true 但计数未减"的半释放态
// （半释放会让该任务永久无法再次释放，只能依赖对账，且活跃用户的 updated_at
// 刷新可能掩盖泄漏）。
//
// 返回 released=true 表示本次调用真正递减了计数（同事务标记抢占成功）。
func MarkTaskSlotReleasedAndDecrement(task *Task) (bool, error) {
	if task == nil || task.ID == 0 {
		return false, nil
	}
	tx := DB.Begin()
	if tx.Error != nil {
		return false, tx.Error
	}
	defer func() { _ = tx.Rollback() }()

	var slot TaskConcurrencySlot
	err := lockForUpdate(tx).Where("user_id = ?", task.UserId).First(&slot).Error
	if err != nil && err != gorm.ErrRecordNotFound {
		return false, err
	}

	// 条件更新任务标记（0→1 成功才代表本路径负责释放）
	res := tx.Model(&Task{}).
		Where("id = ? AND concurrency_released = ?", task.ID, false).
		Update("concurrency_released", true)
	if res.Error != nil {
		return false, res.Error
	}
	if res.RowsAffected != 1 {
		// 已被其他终态路径释放（或任务行已不存在）：幂等 no-op
		return false, tx.Commit().Error
	}

	// 标记抢占成功：递减计数（行不存在则无名额可减，仅提交标记）
	if err == nil && slot.RunningCount > 0 {
		if err := tx.Model(&TaskConcurrencySlot{}).Where("user_id = ?", task.UserId).
			Updates(map[string]any{
				"running_count": slot.RunningCount - 1,
				"updated_at":    time.Now().Unix(),
			}).Error; err != nil {
			return false, err
		}
	}
	if err := tx.Commit().Error; err != nil {
		return false, err
	}
	return true, nil
}

// CountRunningSeedanceTasks 统计用户当前"运行中"（非终态）的 Seedance 任务数。
// 用于对账兜底与 429 文案中的当前运行数。
func CountRunningSeedanceTasks(userId int) (int64, error) {
	return countRunningSeedanceTasks(DB, userId)
}

// countRunningSeedanceTasks 在指定 DB/事务上统计（预留/对账在锁内查询时用 tx，
// 保证与行锁读取一致快照；公共入口用全局 DB）。
func countRunningSeedanceTasks(db *gorm.DB, userId int) (int64, error) {
	var count int64
	err := db.Model(&Task{}).
		Where("user_id = ?", userId).
		Where("platform IN ?", seedancePlatformKeys()).
		Where("status IN ?", TaskRunningStatuses).
		Count(&count).Error
	return count, err
}

func seedancePlatformKeys() []string {
	keys := make([]string, 0, len(seedancePlatformStrings))
	for k := range seedancePlatformStrings {
		keys = append(keys, k)
	}
	return keys
}

// ReconcileTaskConcurrencySlots 兜底对账（异常退出清理）：
//
// 名额的真实占用 = 运行中任务行 + 未超时的占名额恢复记录（outcome_unknown /
// 落库失败恢复，上游可能/确定已创建任务）+ 在途预留（无实体，靠 stale 启发式）。
//
// 修复规则（每个用户独立事务 + 行锁内重读重算，绝不基于锁外快照覆盖并发写入）：
//  1. 原子消费过期恢复记录：把超时的占名额恢复记录 concurrency_reserved 清为
//     false，并按实际清除数释放对应名额（newCount = 原计数 - 清除数），
//     下限不低于已知占用——**绝不把新鲜在途预留一并覆盖**（若把 expiredRec 当作
//     整体修正触发器，会误删"已预留但任务行尚未落库"的名额，且标记永不清零，
//     风险持续存在）；
//  2. 仅当计数行本身 stale（超过 staleAfter 未更新，判定无在途预留）时，才把
//     剩余差额整体修正为已知占用（清理进程崩溃泄漏的预留）；
//  3. 计数 < 已知占用：无条件回补（重复释放/存量任务）；
//  4. 在途预留（行新鲜、无任务行无恢复记录）：保留，不误伤。
//
// 补建：有已知占用但无计数行的用户（存量运行任务/存量占名额恢复记录）补建计数行
// （补建前同样先消费过期恢复记录）；采用 ON CONFLICT DO NOTHING，行被并发预留
// 创建时不覆盖其计数。
//
// 返回修复/补建的行数。
func ReconcileTaskConcurrencySlots(staleAfter time.Duration) (int, error) {
	if staleAfter <= 0 {
		staleAfter = 30 * time.Minute
	}
	now := time.Now().Unix()
	staleBefore := now - int64(staleAfter.Seconds())

	// 候选用户 = 已有计数行的用户 ∪ 有运行中任务或占名额恢复记录但缺计数行的用户
	seen := make(map[int]struct{})
	var slotUserIDs []int
	if err := DB.Model(&TaskConcurrencySlot{}).Distinct().Pluck("user_id", &slotUserIDs).Error; err != nil {
		return 0, err
	}
	for _, uid := range slotUserIDs {
		seen[uid] = struct{}{}
	}
	var taskUserIDs []int
	if err := DB.Model(&Task{}).
		Where("platform IN ?", seedancePlatformKeys()).
		Where("status IN ?", TaskRunningStatuses).
		Distinct().
		Pluck("user_id", &taskUserIDs).Error; err != nil {
		return 0, err
	}
	for _, uid := range taskUserIDs {
		seen[uid] = struct{}{}
	}
	var recUserIDs []int
	if err := DB.Model(&TaskSubmitRecovery{}).
		Where("concurrency_reserved = ?", true).
		Distinct().
		Pluck("user_id", &recUserIDs).Error; err != nil {
		return 0, err
	}
	for _, uid := range recUserIDs {
		seen[uid] = struct{}{}
	}

	fixed := 0
	for uid := range seen {
		// 每个用户独立事务 + 行锁，锁内重读重算，杜绝覆盖并发预留/释放。
		tx := DB.Begin()
		if tx.Error != nil {
			return fixed, tx.Error
		}
		var slot TaskConcurrencySlot
		err := lockForUpdate(tx).Where("user_id = ?", uid).First(&slot).Error
		if err == gorm.ErrRecordNotFound {
			// 补建缺失的计数行：先消费过期恢复记录，再按已知占用建行；
			//    若被并发预留抢先建行，DO NOTHING 冲突 → 跳过，不覆盖其计数。
			if cerr := consumeExpiredRecoveryReservations(tx, uid, staleBefore); cerr != nil {
				_ = tx.Rollback()
				return fixed, cerr
			}
			known, cerr := knownSlotOccupancy(tx, uid, staleBefore)
			if cerr != nil {
				_ = tx.Rollback()
				return fixed, cerr
			}
			if known > 0 {
				res := tx.Clauses(clause.OnConflict{DoNothing: true}).
					Create(&TaskConcurrencySlot{UserId: uid, RunningCount: int(known), UpdatedAt: now})
				if res.Error != nil {
					_ = tx.Rollback()
					return fixed, res.Error
				}
				if res.RowsAffected == 1 {
					fixed++
				}
			}
			if err := tx.Commit().Error; err != nil {
				return fixed, err
			}
			continue
		}
		if err != nil {
			_ = tx.Rollback()
			return fixed, err
		}

		// 锁后重读：基于当前值重新判断，绝不使用锁外快照。
		taskActual, cerr := countRunningSeedanceTasks(tx, uid)
		if cerr != nil {
			_ = tx.Rollback()
			return fixed, cerr
		}
		recActive, cerr := countActiveConcurrencyReservedRecoveries(tx, uid, staleBefore)
		if cerr != nil {
			_ = tx.Rollback()
			return fixed, cerr
		}
		known := taskActual + recActive

		// 1. 原子消费过期恢复记录：清除占名额标记（仅本事务可见，与行锁一致）。
		//    RowsAffected = 实际清除数 = 应释放的名额数。
		clearRes := tx.Model(&TaskSubmitRecovery{}).
			Where("user_id = ? AND concurrency_reserved = ? AND created_at <= ?", uid, true, staleBefore).
			Update("concurrency_reserved", false)
		if clearRes.Error != nil {
			_ = tx.Rollback()
			return fixed, clearRes.Error
		}
		expiredCleared := clearRes.RowsAffected

		// 2. 按实际清除数释放名额；下限不低于已知占用。
		//    关键：不清除"无法归因"的新鲜在途预留——newCount 只扣除已被消费的
		//    过期恢复记录名额，其余差额（含在途预留）原样保留。
		newCount := slot.RunningCount - int(expiredCleared)
		if newCount < int(known) {
			newCount = int(known)
		}

		// 3. 仅当计数行本身 stale 时整体修正（清理进程崩溃泄漏的预留；
		//    行新鲜 = 存在在途活动，绝不整体覆盖）。
		if slot.UpdatedAt < staleBefore && slot.RunningCount > int(known) {
			newCount = int(known)
		}

		if newCount == slot.RunningCount && expiredCleared == 0 {
			_ = tx.Commit() // 无变化
			continue
		}
		if newCount != slot.RunningCount {
			if err := tx.Model(&TaskConcurrencySlot{}).Where("user_id = ?", uid).
				Updates(map[string]any{"running_count": newCount, "updated_at": now}).Error; err != nil {
				_ = tx.Rollback()
				return fixed, err
			}
		}
		if err := tx.Commit().Error; err != nil {
			return fixed, err
		}
		fixed++
	}
	return fixed, nil
}

// consumeExpiredRecoveryReservations 原子清除该用户所有过期（created_at <=
// staleBefore）的占名额恢复记录标记（concurrency_reserved → false）。
// 幂等：已被清除的记录不再匹配，重复调用无副作用。
func consumeExpiredRecoveryReservations(db *gorm.DB, userId int, staleBefore int64) error {
	return db.Model(&TaskSubmitRecovery{}).
		Where("user_id = ? AND concurrency_reserved = ? AND created_at <= ?", userId, true, staleBefore).
		Update("concurrency_reserved", false).Error
}

// knownSlotOccupancy 计算用户的"已知名额占用" = 运行中任务行数 + 未超时的
// 占名额恢复记录数（恢复记录创建超过 staleAfter 即超时，不再计入占用）。
func knownSlotOccupancy(db *gorm.DB, userId int, staleBefore int64) (int64, error) {
	taskCount, err := countRunningSeedanceTasks(db, userId)
	if err != nil {
		return 0, err
	}
	recActive, err := countActiveConcurrencyReservedRecoveries(db, userId, staleBefore)
	if err != nil {
		return 0, err
	}
	return taskCount + recActive, nil
}

// countActiveConcurrencyReservedRecoveries 统计用户"未超时"（created_at >
// staleBefore）的占名额恢复记录数。过期记录由对账消费（清除标记）后不再计入。
func countActiveConcurrencyReservedRecoveries(db *gorm.DB, userId int, staleBefore int64) (int64, error) {
	var count int64
	err := db.Model(&TaskSubmitRecovery{}).
		Where("user_id = ? AND concurrency_reserved = ?", userId, true).
		Where("created_at > ?", staleBefore).
		Count(&count).Error
	return count, err
}

// GetRunningCountForUser 读取用户当前名额计数（对账与诊断用）。
func GetRunningCountForUser(userId int) (int, error) {
	var slot TaskConcurrencySlot
	err := DB.Where("user_id = ?", userId).First(&slot).Error
	if err == gorm.ErrRecordNotFound {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	return slot.RunningCount, nil
}
