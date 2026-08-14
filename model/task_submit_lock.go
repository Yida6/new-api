package model

import (
	"time"

	"gorm.io/gorm/clause"
)

// TaskSubmitLockRow 以"请求指纹为主键"的数据库唯一约束实现跨实例提交互斥。
// 多实例部署时，进程内 mutex/map 无法阻止不同实例的重复提交，必须依赖
// 共享存储；本表即该共享互斥的数据库实现（Redis 可用时优先用 Redis SETNX）。
//
// 生命周期：提交开始前 Insert（冲突即代表有别的实例/请求在途）→ 提交完成后
// 将 ExpireAt 更新为"当前时间 + 宽限期"（仅 OwnerToken 匹配时），宽限期内
// 相同提交仍被拒绝（吸收双击延迟），宽限期过后被后续 Acquire 惰性清理。
//
// OwnerToken：每次获取生成唯一令牌；Release 必须携带同一令牌，防止原持有者
// 请求卡死后 TTL 过期、新请求拿到锁、旧请求迟到 Release 误删新持有者的锁。
type TaskSubmitLockRow struct {
	Fingerprint string `json:"fingerprint" gorm:"type:varchar(128);primaryKey"`
	OwnerToken  string `json:"owner_token" gorm:"type:varchar(64)"`
	ExpireAt    int64  `json:"expire_at"`  // 毫秒时间戳
	CreatedAt   int64  `json:"created_at"` // 毫秒时间戳
}

func (TaskSubmitLockRow) TableName() string { return "task_submit_locks" }

// AcquireTaskSubmitLockRow 尝试获取跨实例提交锁（带 owner token）。
// 返回 false 表示该指纹的提交仍持有锁（在途或宽限期内），调用方应拒绝重复提交。
// ttlMs 以毫秒为单位（宽限期为秒级或更短时避免时间戳精度冲突）。
func AcquireTaskSubmitLockRow(fingerprint, ownerToken string, ttlMs int64) (bool, error) {
	now := time.Now().UnixMilli()
	// 惰性清理该指纹的过期残留（宽限期已过）
	if err := DB.Where("fingerprint = ? AND expire_at < ?", fingerprint, now).
		Delete(&TaskSubmitLockRow{}).Error; err != nil {
		return false, err
	}
	row := TaskSubmitLockRow{
		Fingerprint: fingerprint,
		OwnerToken:  ownerToken,
		ExpireAt:    now + ttlMs,
		CreatedAt:   now,
	}
	res := DB.Clauses(clause.OnConflict{DoNothing: true}).Create(&row)
	if res.Error != nil {
		return false, res.Error
	}
	return res.RowsAffected > 0, nil
}

// ReleaseTaskSubmitLockRowGrace 提交完成后保留宽限期：把 ExpireAt 更新为
// "当前时间 + 宽限期（毫秒）"，宽限期内相同指纹的 Acquire 仍会冲突（拒绝双击延迟请求）。
// 仅当 OwnerToken 匹配时才生效：迟到的旧 Release 无法误延长新持有者的锁。
func ReleaseTaskSubmitLockRowGrace(fingerprint, ownerToken string, graceMs int64) error {
	return DB.Model(&TaskSubmitLockRow{}).
		Where("fingerprint = ? AND owner_token = ?", fingerprint, ownerToken).
		Update("expire_at", time.Now().UnixMilli()+graceMs).Error
}
