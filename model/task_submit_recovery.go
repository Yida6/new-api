package model

import (
	"time"
)

// 任务提交恢复记录的状态机（Status 字段）
const (
	// TaskRecoveryStatusUnknown 结果未知（outcome_unknown），等待人工/查询确认。
	TaskRecoveryStatusUnknown = "unknown"
	// TaskRecoveryStatusAssociated 已通过官方查询接口或人工确认，关联了上游 task_id。
	TaskRecoveryStatusAssociated = "associated"
	// TaskRecoveryStatusInferred 模糊候选发现仅有一个候选，已记录"推测关联"，
	// 仍需人工确认，不代表强一致确认。
	TaskRecoveryStatusInferred = "inferred"
	// TaskRecoveryStatusAmbiguous 模糊候选发现多个候选，不自动选择。
	TaskRecoveryStatusAmbiguous = "ambiguous"
	// TaskRecoveryStatusRecreated 用户确认"承担重复创建风险"后已重新创建
	// （生成了新的逻辑尝试记录）。
	TaskRecoveryStatusRecreated = "recreated"
)

// TaskSubmitRecovery 持久化"创建结果未知"等需要人工恢复的任务提交记录。
//
// 字段设计说明：
//   - 只保存请求指纹（哈希）与内容指纹，不保存完整提示词 / 图片原文 / 密钥，
//     满足"日志与存储不落敏感请求内容"的要求；
//   - 保存首次提交时间与渠道信息，便于恢复时用官方查询接口做时间窗 + 模型匹配；
//   - UpstreamTaskID 仅在查询确认 / 人工确认后填写；Candidates 为模糊候选列表。
type TaskSubmitRecovery struct {
	ID          int64  `json:"id" gorm:"primary_key;AUTO_INCREMENT"`
	UserId      int    `json:"user_id" gorm:"index"`
	PublicTaskID string `json:"public_task_id" gorm:"type:varchar(191);index"`
	Platform    string `json:"platform" gorm:"type:varchar(30)"`
	Model       string `json:"model" gorm:"type:varchar(191)"` // 用户侧（origin）模型名
	// UpstreamModelName 渠道模型映射后的上游模型名（候选发现按它过滤上游任务；
	// 未映射时与 Model 相同，留空则回退到 Model）。
	UpstreamModelName string `json:"upstream_model_name" gorm:"type:varchar(191)"`

	// 渠道信息（恢复时重新查询上游需要）
	ChannelId      int    `json:"channel_id" gorm:"index"`
	ChannelType    int    `json:"channel_type"`
	ChannelBaseURL string `json:"channel_base_url" gorm:"type:varchar(255)"`

	// 本地标识
	IdempotencyKey    string `json:"idempotency_key" gorm:"type:varchar(64);index"`
	ClientRequestID   string `json:"client_request_id" gorm:"type:varchar(64);index"`
	Fingerprint       string `json:"fingerprint" gorm:"type:varchar(128);index"`
	ContentFingerprint string `json:"content_fingerprint" gorm:"type:varchar(128)"`

	// 结果与恢复状态
	Outcome        string `json:"outcome" gorm:"type:varchar(20)"`           // outcome_unknown / confirmed_success
	Status         string `json:"status" gorm:"type:varchar(20);index"`      // unknown/associated/inferred/ambiguous/recreated
	UpstreamTaskID string `json:"upstream_task_id" gorm:"type:varchar(191)"` // 确认后关联
	// Candidates 为模糊候选列表的 JSON 字符串。用通用 TEXT（SQLite/MySQL/PostgreSQL
	// 均支持），与仓库新增表的 TEXT 回退约定保持一致，不使用 gorm 的 type:json。
	Candidates string `json:"candidates" gorm:"type:text"`

	// 重试链
	Attempt  int   `json:"attempt"` // 逻辑尝试次数（人工重试 +1）
	ParentID int64 `json:"parent_id" gorm:"index"`

	// 时间
	FirstSubmitTime int64 `json:"first_submit_time"`
	SubmitTime      int64 `json:"submit_time"`
	CreatedAt       int64 `json:"created_at"`
	UpdatedAt       int64 `json:"updated_at"`

	// ConcurrencyReserved 标记该恢复记录占用了一个 Seedance 并发名额。
	// 场景：outcome_unknown（上游可能已创建任务）/ 已取得上游 task_id 但本地落库
	// 失败——此时本地没有任务行，但上游任务可能/确定在运行，名额必须由恢复记录
	// 继续持有，绝不能随请求结束立即释放（否则用户可继续提交，上游实际运行任务
	// 数会超过限制）。名额持有至恢复记录创建超过对账 TTL（见
	// ReconcileTaskConcurrencySlots 的 stale 判定）后由对账统一释放。
	ConcurrencyReserved bool `json:"-" gorm:"column:concurrency_reserved;default:false"`

	Note string `json:"note" gorm:"type:varchar(1000)"` // 人工备注 / 风险确认记录
}

func (TaskSubmitRecovery) TableName() string { return "task_submit_recoveries" }

func (r *TaskSubmitRecovery) Insert() error {
	now := time.Now().Unix()
	if r.CreatedAt == 0 {
		r.CreatedAt = now
	}
	if r.SubmitTime == 0 {
		r.SubmitTime = now
	}
	r.UpdatedAt = now
	return DB.Create(r).Error
}

func (r *TaskSubmitRecovery) Update() error {
	r.UpdatedAt = time.Now().Unix()
	return DB.Model(r).Select(
		"status", "outcome", "upstream_task_id", "candidates", "attempt",
		"parent_id", "note", "updated_at", "submit_time",
	).Updates(r).Error
}

// GetTaskSubmitRecoveryByID 按 ID 查询（可选按用户隔离）。
func GetTaskSubmitRecoveryByID(id int64, userId int) (*TaskSubmitRecovery, error) {
	var r TaskSubmitRecovery
	q := DB.Where("id = ?", id)
	if userId > 0 {
		q = q.Where("user_id = ?", userId)
	}
	if err := q.First(&r).Error; err != nil {
		return nil, err
	}
	return &r, nil
}

// GetTaskSubmitRecoveriesByUser 分页查询用户的恢复记录，可按状态过滤。
func GetTaskSubmitRecoveriesByUser(userId int, status string, page, pageSize int) ([]*TaskSubmitRecovery, int64, error) {
	if page <= 0 {
		page = 1
	}
	if pageSize <= 0 || pageSize > 100 {
		pageSize = 20
	}
	q := DB.Model(&TaskSubmitRecovery{}).Where("user_id = ?", userId)
	if status != "" {
		q = q.Where("status = ?", status)
	}
	var total int64
	if err := q.Count(&total).Error; err != nil {
		return nil, 0, err
	}
	var list []*TaskSubmitRecovery
	if err := q.Order("id DESC").Offset((page - 1) * pageSize).Limit(pageSize).Find(&list).Error; err != nil {
		return nil, 0, err
	}
	return list, total, nil
}

// GetTaskSubmitRecoveryByParent 查找某恢复记录的子尝试（新逻辑尝试记录）。
func GetTaskSubmitRecoveryByParent(parentID int64) (*TaskSubmitRecovery, error) {
	var r TaskSubmitRecovery
	if err := DB.Where("parent_id = ?", parentID).Order("id ASC").First(&r).Error; err != nil {
		return nil, err
	}
	return &r, nil
}

// MarkRecoveryRecreated 原子地把恢复记录标记为 recreated（用户确认承担风险后的
// 人工重试占位）。仅在状态为显式白名单 unknown/inferred/ambiguous 时成功
// （RowsAffected==1）；已 recreated 或已 associated 的记录不可重复占用，保证
// 并发/重复点击人工重试时只有一个请求能进入创建流程（数据库级互斥，多实例同样
// 有效）。使用显式白名单而非 NOT IN，避免未来新增或异常状态被意外接受。
func MarkRecoveryRecreated(id, userId int64, note string) (bool, error) {
	res := DB.Model(&TaskSubmitRecovery{}).
		Where("id = ? AND user_id = ? AND status IN ?",
			id, userId, []string{TaskRecoveryStatusUnknown, TaskRecoveryStatusInferred, TaskRecoveryStatusAmbiguous}).
		Updates(map[string]any{
			"status":     TaskRecoveryStatusRecreated,
			"note":       note,
			"updated_at": time.Now().Unix(),
		})
	if res.Error != nil {
		return false, res.Error
	}
	return res.RowsAffected > 0, nil
}

// UpdateRecoveryNote 更新恢复记录备注与更新时间的原子操作（结果回填用）。
func UpdateRecoveryNote(id, userId int64, note string) error {
	return DB.Model(&TaskSubmitRecovery{}).
		Where("id = ? AND user_id = ?", id, userId).
		Updates(map[string]any{
			"note":       note,
			"updated_at": time.Now().Unix(),
		}).Error
}

// UpdateRecoveryDiscoveryResult 条件更新候选发现结果：仅在状态仍为非终态
// （unknown/inferred/ambiguous）时生效。
//
// 解决竞态：discover 执行上游查询期间，若并发 recreate 已把记录原子占位为
// recreated（或并发 associate 已关联），本更新必须失败（RowsAffected==0），
// 绝不覆盖并发操作写下的终态。返回 false 表示状态已被并发变更，结果应丢弃。
func UpdateRecoveryDiscoveryResult(id, userId int64, status, candidates, note string) (bool, error) {
	res := DB.Model(&TaskSubmitRecovery{}).
		Where("id = ? AND user_id = ? AND status IN ?",
			id, userId, []string{TaskRecoveryStatusUnknown, TaskRecoveryStatusInferred, TaskRecoveryStatusAmbiguous}).
		Updates(map[string]any{
			"status":     status,
			"candidates": candidates,
			"note":       note,
			"updated_at": time.Now().Unix(),
		})
	if res.Error != nil {
		return false, res.Error
	}
	return res.RowsAffected > 0, nil
}

// MarkRecoveryAssociated 原子地把恢复记录关联到上游 task_id。
// 仅在状态为显式白名单 unknown/inferred/ambiguous 时成功：与人工重试（recreated）
// 互斥，保证"关联"与"人工重试"同一时刻只允许一种操作成功（验证期间被并发占位
// 则失败）。使用显式白名单而非 NOT IN，避免未来新增或异常状态被意外接受。
func MarkRecoveryAssociated(id, userId int64, upstreamTaskID, note string) (bool, error) {
	res := DB.Model(&TaskSubmitRecovery{}).
		Where("id = ? AND user_id = ? AND status IN ?",
			id, userId, []string{TaskRecoveryStatusUnknown, TaskRecoveryStatusInferred, TaskRecoveryStatusAmbiguous}).
		Updates(map[string]any{
			"status":          TaskRecoveryStatusAssociated,
			"upstream_task_id": upstreamTaskID,
			"note":            note,
			"updated_at":      time.Now().Unix(),
		})
	if res.Error != nil {
		return false, res.Error
	}
	return res.RowsAffected > 0, nil
}

// ResetRecoveryForRetry 人工重试明确失败（未创建任务且无子恢复记录）后，把
// 记录重新打开为 unknown，提供可执行的恢复路径（可再次关联/候选发现/重试）。
// 仅当状态仍为 recreated 时生效，避免覆盖并发操作。
func ResetRecoveryForRetry(id, userId int64, note string) error {
	return DB.Model(&TaskSubmitRecovery{}).
		Where("id = ? AND user_id = ? AND status = ?", id, userId, TaskRecoveryStatusRecreated).
		Updates(map[string]any{
			"status":     TaskRecoveryStatusUnknown,
			"note":       note,
			"updated_at": time.Now().Unix(),
		}).Error
}
