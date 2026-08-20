package model

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"time"

	"gorm.io/gorm/clause"
)

const (
	TaskClientIdempotencyPending   = "pending"
	TaskClientIdempotencyCommitted = "committed"
	TaskClientIdempotencyUnknown   = "unknown"
)

// TaskClientIdempotency 持久化客户端 Idempotency-Key 与公开任务 ID 的映射。
//
// 记录必须在调用上游前创建：这样并发请求、跨实例请求，以及上游成功后本地进程
// 异常等场景都不会再次 POST。这里只保存作用域哈希，不保存客户端原始 key。
type TaskClientIdempotency struct {
	ScopeHash          string `json:"scope_hash" gorm:"type:char(64);primaryKey"`
	UserId             int    `json:"user_id" gorm:"index"`
	TokenId            int    `json:"token_id" gorm:"index"`
	RequestFingerprint string `json:"request_fingerprint" gorm:"type:varchar(160)"`
	PublicTaskID       string `json:"public_task_id" gorm:"type:varchar(191);index"`
	State              string `json:"state" gorm:"type:varchar(20);index"`
	CreatedAt          int64  `json:"created_at" gorm:"index"`
	UpdatedAt          int64  `json:"updated_at"`
}

func (TaskClientIdempotency) TableName() string {
	return "task_client_idempotencies"
}

func taskClientIdempotencyScopeHash(userId, tokenId int, clientKey string) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%d:%d:%s", userId, tokenId, clientKey)))
	return hex.EncodeToString(sum[:])
}

// ClaimTaskClientIdempotency 原子创建客户端幂等占位。
// claimed=true 表示本请求取得提交权；false 表示已有同作用域记录。
func ClaimTaskClientIdempotency(userId, tokenId int, clientKey, requestFingerprint, publicTaskID string) (*TaskClientIdempotency, bool, error) {
	now := time.Now().Unix()
	scopeHash := taskClientIdempotencyScopeHash(userId, tokenId, clientKey)
	candidate := &TaskClientIdempotency{
		ScopeHash:          scopeHash,
		UserId:             userId,
		TokenId:            tokenId,
		RequestFingerprint: requestFingerprint,
		PublicTaskID:       publicTaskID,
		State:              TaskClientIdempotencyPending,
		CreatedAt:          now,
		UpdatedAt:          now,
	}
	result := DB.Clauses(clause.OnConflict{DoNothing: true}).Create(candidate)
	if result.Error != nil {
		return nil, false, result.Error
	}
	if result.RowsAffected == 1 {
		return candidate, true, nil
	}

	var existing TaskClientIdempotency
	if err := DB.Where("scope_hash = ?", scopeHash).First(&existing).Error; err != nil {
		return nil, false, err
	}
	return &existing, false, nil
}

func UpdateTaskClientIdempotencyState(scopeHash, publicTaskID, state string) error {
	return DB.Model(&TaskClientIdempotency{}).
		Where("scope_hash = ? AND public_task_id = ?", scopeHash, publicTaskID).
		Updates(map[string]any{"state": state, "updated_at": time.Now().Unix()}).Error
}

// DeleteTaskClientIdempotencyClaim 仅删除仍属于本请求的占位。上游明确未创建任务
// 时允许客户端用同一个 key 修正问题后重试；已关联任务/结果未知的记录不得删除。
func DeleteTaskClientIdempotencyClaim(scopeHash, publicTaskID string) error {
	return DB.Where("scope_hash = ? AND public_task_id = ? AND state = ?",
		scopeHash, publicTaskID, TaskClientIdempotencyPending).
		Delete(&TaskClientIdempotency{}).Error
}
