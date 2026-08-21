package model

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// ---------------------------------------------------------------------------
// TaskBillingDebt — Seedance 差额欠款 / 结算异常记录（持久化、可审计、幂等）
//
// 背景：Seedance 任务完成时按真实 usage 结算，若实际费用高于预扣且差额无法
// 从用户钱包/订阅扣除（余额不足），系统不得把钱包扣成负数，也不得静默放弃。
// 本表记录这笔"未收差额"，并将用户标记为欠款冻结（阻止继续创建付费任务），
// 直至差额被原子收款并清偿。
//
// 幂等不变量：
//   - 每个任务（task_id 唯一索引）最多一笔欠款记录；无论被轮询、恢复或人工
//     重试多少次，重复结算只会命中同一条 pending 记录（no-op），绝不重复
//     补扣、重复冻结或重复累计欠款。
//   - 状态流转：pending（未清）→ paid（已收款）→（人工核销 voided）。
//   - 只有全部 pending 欠款清偿后才解除"欠款冻结"，且解除绝不触碰管理员
//     手工禁用（users.status）——欠款冻结与手工禁用是两个独立维度。
// ---------------------------------------------------------------------------

type TaskBillingDebtStatus string

const (
	DebtStatusPending TaskBillingDebtStatus = "pending" // 未清
	DebtStatusPaid    TaskBillingDebtStatus = "paid"    // 已收款清偿
	DebtStatusVoided  TaskBillingDebtStatus = "voided"  // 人工核销
)

type TaskBillingDebt struct {
	ID               int64                 `json:"id" gorm:"primary_key;AUTO_INCREMENT"`
	UserId           int                   `json:"user_id" gorm:"index"`
	TaskId           string                `json:"task_id" gorm:"type:varchar(191);uniqueIndex"` // 本地任务 ID（task_xxxx）
	UpstreamTaskId   string                `json:"upstream_task_id" gorm:"type:varchar(191)"`     // 上游任务 ID
	ModelName        string                `json:"model_name" gorm:"type:varchar(191)"`
	ChannelId        int                   `json:"channel_id" gorm:"index"`
	PreConsumedQuota int64                 `json:"pre_consumed_quota" gorm:"type:bigint;default:0"` // 预扣额度
	ActualQuota      int64                 `json:"actual_quota" gorm:"type:bigint;default:0"`       // 实际额度
	DeltaQuota       int64                 `json:"delta_quota" gorm:"type:bigint;default:0"`        // 未付差额 = Actual - Pre（>0）
	Reason           string                `json:"reason" gorm:"type:varchar(255)"`
	Status           TaskBillingDebtStatus `json:"status" gorm:"type:varchar(20);index"`
	AlertSent        bool                  `json:"alert_sent" gorm:"default:false"` // 欠款告警是否已成功发送（失败保留可重试）
	CreatedAt        int64                 `json:"created_at"`
	UpdatedAt        int64                 `json:"updated_at"`
	CollectedAt      int64                 `json:"collected_at"` // 收款/清偿时间
	ReleasedAt       int64                 `json:"released_at"`  // 欠款冻结解除时间
	// ---- 清偿所需计费快照（欠款创建时从任务快照，清偿时据此选择资金来源）----
	BillingSource      string `json:"billing_source" gorm:"type:varchar(20);default:''"` // "wallet" / "subscription"
	SubscriptionId     int    `json:"subscription_id" gorm:"default:0"`                  // 订阅欠款的订阅 ID
	TokenId            int    `json:"token_id" gorm:"default:0"`                         // 清偿时同步扣减的令牌 ID
	Group              string `json:"group" gorm:"type:varchar(50);default:''"`          // 任务分组（日志用）
	ConsumeLogRecorded bool   `json:"consume_log_recorded" gorm:"default:false"`         // 提交时是否记录了消费日志
	BillingStatsFailed bool   `json:"billing_stats_failed" gorm:"default:false"`         // 提交时累计统计是否写入失败
	// ---- 告警投递 claim（多实例去重）----
	AlertClaimAt   int64 `json:"alert_claim_at" gorm:"default:0"` // 告警 claim 时间戳（0=未 claim；超 lease 可回收）
	AlertAttempts  int   `json:"alert_attempts" gorm:"default:0"` // 已尝试投递次数（含失败）
	// 资金来源切换记录：订阅欠款经钱包代偿时置 true（审计），由清偿流程写入。
	WalletOverflowed bool `json:"wallet_overflowed" gorm:"default:false"`
}

func (TaskBillingDebt) TableName() string { return "task_billing_debts" }

// ErrDebtAlreadyExists 同一任务已存在欠款记录（幂等 no-op 的哨兵）。
var ErrDebtAlreadyExists = errors.New("task billing debt already exists")

// ErrDebtInsufficientBalance 收款时余额仍不足（用户需继续充值/等待订阅额度）。
var ErrDebtInsufficientBalance = errors.New("insufficient balance to collect task billing debt")

// ErrDebtSubscriptionInsufficient 订阅欠款从订阅清偿但订阅额度不足，且未允许
// 钱包代偿（或代偿也失败）。
var ErrDebtSubscriptionInsufficient = errors.New("subscription insufficient to collect task billing debt")

// ErrDebtMissingTask 欠款关联任务缺失（清偿中止，不得先标 paid）。
var ErrDebtMissingTask = errors.New("task missing for debt collection")

// ErrDebtMissingToken 欠款关联令牌缺失（清偿中止，不得先标 paid）。
var ErrDebtMissingToken = errors.New("token missing for debt collection")

// ErrDebtNotFound 欠款记录不存在或状态不允许操作。
var ErrDebtNotFound = errors.New("task billing debt not found or not payable")

// ErrDebtPendingRemaining 用户仍有其他未清欠款，禁止人工解冻（解冻必须先清偿）。
var ErrDebtPendingRemaining = errors.New("user still has pending debts")

// ---------------------------------------------------------------------------
// TaskBillingDebtAudit — 欠款清偿/核销/人工解冻的审计记录
// 每条记录包含：债务 ID、用户 ID、管理员 ID（0=系统自动）、动作、原因、
// 时间与额度。清偿/核销/解冻的全部状态迁移都必须落审计，绝不静默。
// ---------------------------------------------------------------------------

type TaskBillingDebtAudit struct {
	ID         int64  `json:"id" gorm:"primary_key;AUTO_INCREMENT"`
	DebtId     int64  `json:"debt_id" gorm:"index"`
	UserId     int    `json:"user_id" gorm:"index"`
	AdminId    int    `json:"admin_id"` // 0 = 系统自动动作（如清偿后的自动解冻）
	Action     string `json:"action" gorm:"type:varchar(30)"` // repay / void / unfreeze / auto_unfreeze
	Reason     string `json:"reason" gorm:"type:varchar(255)"`
	DeltaQuota int64  `json:"delta_quota" gorm:"type:bigint;default:0"` // 该债务的差额额度
	CreatedAt  int64  `json:"created_at"`
}

func (TaskBillingDebtAudit) TableName() string { return "task_billing_debt_audits" }

// recordTaskBillingDebtAuditTx 在事务内写一条欠款状态迁移审计记录。
func recordTaskBillingDebtAuditTx(tx *gorm.DB, debt *TaskBillingDebt, adminId int, action, reason string) error {
	if debt == nil {
		return nil
	}
	return tx.Create(&TaskBillingDebtAudit{
		DebtId:     debt.ID,
		UserId:     debt.UserId,
		AdminId:    adminId,
		Action:     action,
		Reason:     truncateDebtReason(reason),
		DeltaQuota: debt.DeltaQuota,
		CreatedAt:  time.Now().Unix(),
	}).Error
}

// ListTaskBillingDebtAudits 分页查询欠款审计记录（管理端对账/审计）。
func ListTaskBillingDebtAudits(debtId int64, userId int, startIdx, num int) ([]TaskBillingDebtAudit, int64, error) {
	if num <= 0 {
		num = 20
	}
	if startIdx < 0 {
		startIdx = 0
	}
	query := DB.Model(&TaskBillingDebtAudit{})
	if debtId > 0 {
		query = query.Where("debt_id = ?", debtId)
	}
	if userId > 0 {
		query = query.Where("user_id = ?", userId)
	}
	var total int64
	if err := query.Count(&total).Error; err != nil {
		return nil, 0, err
	}
	var audits []TaskBillingDebtAudit
	if err := query.Order("id desc").Limit(num).Offset(startIdx).Find(&audits).Error; err != nil {
		return nil, 0, err
	}
	return audits, total, nil
}

// CreateTaskBillingDebtIfAbsent 幂等创建欠款记录：同一任务（task_id）已存在时
// no-op 返回 ErrDebtAlreadyExists。调用方必须将其视为"已处理"，不得重复冻结
// 或重复累计。deltaQuota 必须是正数。
func CreateTaskBillingDebtIfAbsent(d DebtInput) (bool, error) {
	if d.UserId <= 0 || d.TaskId == "" {
		return false, errors.New("invalid debt input: user_id and task_id required")
	}
	if d.DeltaQuota <= 0 {
		return false, errors.New("invalid debt input: delta_quota must be positive")
	}
	now := time.Now().Unix()
	rec := &TaskBillingDebt{
		UserId:             d.UserId,
		TaskId:             d.TaskId,
		UpstreamTaskId:     d.UpstreamTaskId,
		ModelName:          d.ModelName,
		ChannelId:          d.ChannelId,
		PreConsumedQuota:   d.PreConsumedQuota,
		ActualQuota:        d.ActualQuota,
		DeltaQuota:         d.DeltaQuota,
		Reason:             d.Reason,
		Status:             DebtStatusPending,
		CreatedAt:          now,
		UpdatedAt:          now,
		BillingSource:      d.BillingSource,
		SubscriptionId:     d.SubscriptionId,
		TokenId:            d.TokenId,
		Group:              d.Group,
		ConsumeLogRecorded: d.ConsumeLogRecorded,
		BillingStatsFailed: d.BillingStatsFailed,
	}
	res := DB.Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "task_id"}},
		DoNothing: true,
	}).Create(rec)
	if res.Error != nil {
		return false, res.Error
	}
	if res.RowsAffected == 0 {
		return false, ErrDebtAlreadyExists
	}
	return true, nil
}

// DebtInput 创建欠款记录的输入。
type DebtInput struct {
	UserId             int
	TaskId             string
	UpstreamTaskId     string
	ModelName          string
	ChannelId          int
	PreConsumedQuota   int64
	ActualQuota        int64
	DeltaQuota         int64
	Reason             string
	BillingSource      string // "wallet" / "subscription"
	SubscriptionId     int
	TokenId            int
	Group              string
	ConsumeLogRecorded bool
	BillingStatsFailed bool
}

// GetTaskBillingDebtByTaskId 查询指定任务的最新欠款记录（含已清偿）。
func GetTaskBillingDebtByTaskId(taskId string) (*TaskBillingDebt, error) {
	if taskId == "" {
		return nil, ErrDebtNotFound
	}
	var debt TaskBillingDebt
	err := DB.Where("task_id = ?", taskId).Order("id desc").First(&debt).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrDebtNotFound
	}
	if err != nil {
		return nil, err
	}
	return &debt, nil
}

// GetTaskBillingDebtByID 按主键查询欠款记录（管理端入口校验归属用）。
func GetTaskBillingDebtByID(id int64) (*TaskBillingDebt, error) {
	if id <= 0 {
		return nil, ErrDebtNotFound
	}
	var debt TaskBillingDebt
	err := DB.First(&debt, id).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrDebtNotFound
	}
	if err != nil {
		return nil, err
	}
	return &debt, nil
}

// PageTaskBillingDebts 分页查询欠款记录（管理端审计/对账）。
// userID<=0 表示不过滤；status 空表示不过滤。
func PageTaskBillingDebts(userID int, status string, startIdx, num int) ([]TaskBillingDebt, int64, error) {
	if num <= 0 {
		num = 20
	}
	if startIdx < 0 {
		startIdx = 0
	}
	query := DB.Model(&TaskBillingDebt{})
	if userID > 0 {
		query = query.Where("user_id = ?", userID)
	}
	if status != "" {
		query = query.Where("status = ?", status)
	}
	var total int64
	if err := query.Count(&total).Error; err != nil {
		return nil, 0, err
	}
	var debts []TaskBillingDebt
	if err := query.Order("id desc").Limit(num).Offset(startIdx).Find(&debts).Error; err != nil {
		return nil, 0, err
	}
	return debts, total, nil
}

// HasOpenDebtForTask 判断任务是否存在未清欠款（退款路径防御：欠款任务不得退款）。
func HasOpenDebtForTask(taskId string) (bool, error) {
	if taskId == "" {
		return false, nil
	}
	var count int64
	err := DB.Model(&TaskBillingDebt{}).
		Where("task_id = ? AND status = ?", taskId, DebtStatusPending).
		Count(&count).Error
	return count > 0, err
}

// CountOpenDebtsByUser 统计用户未清欠款数。
func CountOpenDebtsByUser(userId int) (int64, error) {
	var count int64
	err := DB.Model(&TaskBillingDebt{}).
		Where("user_id = ? AND status = ?", userId, DebtStatusPending).
		Count(&count).Error
	return count, err
}

// GetOpenDebtsByUser 列出用户未清欠款（欠款冻结解除判定用）。
func GetOpenDebtsByUser(userId int) ([]TaskBillingDebt, error) {
	var debts []TaskBillingDebt
	err := DB.Where("user_id = ? AND status = ?", userId, DebtStatusPending).
		Order("id").Find(&debts).Error
	return debts, err
}

// GetPendingDebtsWithAlertPending 查询告警未发送成功的未清欠款（重试入口）。
func GetPendingDebtsWithAlertPending(limit int) ([]TaskBillingDebt, error) {
	if limit <= 0 {
		limit = 100
	}
	var debts []TaskBillingDebt
	err := DB.Where("status = ? AND alert_sent = ?", DebtStatusPending, false).
		Order("id").Limit(limit).Find(&debts).Error
	return debts, err
}

// HasPendingDebtAlerts 是否存在告警未发送成功的未清欠款（调度器 Enabled 用，
// 空闲系统不产生任务行）。
func HasPendingDebtAlerts() bool {
	var id int64
	err := DB.Model(&TaskBillingDebt{}).
		Where("status = ? AND alert_sent = ?", DebtStatusPending, false).
		Limit(1).
		Pluck("id", &id).Error
	return err == nil && id != 0
}

// MarkTaskBillingDebtAlertSent 把欠款标记为"告警已发送"并清除 claim（幂等：
// 已标记则 no-op）。返回 true 表示本次调用真正把 false→true（用于统计）。
func MarkTaskBillingDebtAlertSent(id int64) (bool, error) {
	res := DB.Model(&TaskBillingDebt{}).
		Where("id = ? AND alert_sent = ?", id, false).
		Updates(map[string]any{"alert_sent": true, "alert_claim_at": 0, "updated_at": time.Now().Unix()})
	if res.Error != nil {
		return false, res.Error
	}
	return res.RowsAffected == 1, nil
}

// ClaimDebtAlert 原子 claim 一条欠款告警（多实例去重）：
// 仅当 status=pending 且未发送且（未 claim 或 claim 已超 lease 秒）时成功。
// claim 成功后 alert_attempts 自增，供超时回收与审计。返回 true 表示本实例
// 获得了该条告警的投递权（RowsAffected==1）。
func ClaimDebtAlert(id int64, leaseSeconds int64) (bool, error) {
	if leaseSeconds <= 0 {
		leaseSeconds = 120 // 默认 2 分钟 claim 租约
	}
	now := time.Now().Unix()
	expiredBefore := now - leaseSeconds
	res := DB.Model(&TaskBillingDebt{}).
		Where("id = ? AND status = ? AND alert_sent = ? AND (alert_claim_at = ? OR alert_claim_at < ?)",
			id, DebtStatusPending, false, int64(0), expiredBefore).
		Updates(map[string]any{
			"alert_claim_at": now,
			"alert_attempts": gorm.Expr("alert_attempts + 1"),
			"updated_at":     now,
		})
	if res.Error != nil {
		return false, res.Error
	}
	return res.RowsAffected == 1, nil
}

// ReleaseDebtAlert 释放告警 claim（发送失败后让其他实例/下轮可重试）。
func ReleaseDebtAlert(id int64) error {
	return DB.Model(&TaskBillingDebt{}).
		Where("id = ?", id).
		Update("alert_claim_at", 0).Error
}

// GetUserDebtFrozen 读取用户的欠款冻结状态（DB 直查，避免缓存窗口误放行）。
func GetUserDebtFrozen(userId int) (bool, error) {
	var user User
	err := DB.Select("debt_frozen").Where("id = ?", userId).First(&user).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return false, gorm.ErrRecordNotFound
	}
	if err != nil {
		return false, err
	}
	return user.DebtFrozen, nil
}

// freezeUserForDebtTx 在事务内把用户标记为欠款冻结。
// 管理员（role >= RoleAdminUser）不冻结（避免误冻结运维账号），返回 isAdmin=true，
// 调用方仍必须为其记录欠款并发出最高级别告警。
// 冻结条件更新带 role < 管理员 守卫，天然幂等（已冻结则 no-op）。
func freezeUserForDebtTx(tx *gorm.DB, userId int, reason string) (frozen bool, isAdmin bool, err error) {
	var user User
	if err := tx.Select("id", "role", "debt_frozen").Where("id = ?", userId).First(&user).Error; err != nil {
		return false, false, err
	}
	if user.Role >= common.RoleAdminUser {
		return false, true, nil
	}
	now := time.Now().Unix()
	res := tx.Model(&User{}).
		Where("id = ? AND role < ? AND debt_frozen = ?", userId, common.RoleAdminUser, false).
		Updates(map[string]any{
			"debt_frozen":        true,
			"debt_frozen_at":     now,
			"debt_frozen_reason": truncateDebtReason(reason),
		})
	if res.Error != nil {
		return false, false, res.Error
	}
	return res.RowsAffected == 1, false, nil
}

// FreezeUserForDebt 冻结用户（事务外封装，冻结成功后立即失效用户缓存，
// 保证新的付费请求读取到最新冻结状态）。
func FreezeUserForDebt(userId int, reason string) (frozen bool, isAdmin bool, err error) {
	err = DB.Transaction(func(tx *gorm.DB) error {
		var innerErr error
		frozen, isAdmin, innerErr = freezeUserForDebtTx(tx, userId, reason)
		return innerErr
	})
	if err != nil {
		return false, false, err
	}
	if frozen {
		_ = invalidateUserCache(userId)
	}
	return frozen, isAdmin, nil
}

// lockUserForDebtTx 在事务内锁定用户行（SELECT ... FOR UPDATE）作为欠款
// 流程的统一串行化边界。欠款创建（CreateDebtAndFreeze）、清偿
// （RepayTaskBillingDebt）、核销（VoidTaskBillingDebt）、显式解冻
// （UnfreezeUserDebtAudited）的所有 pending 检查与 debt_frozen 状态迁移
// 都必须在同一用户锁保护下执行，杜绝"查询 pending 后、更新 debt_frozen 前"
// 被并发创建的新欠款插入，最终出现"存在 pending 欠款但用户未冻结"的不一致。
//
// 锁顺序约定（死锁避免）：所有欠款流程一律**先锁用户行、再锁债务行**（如有），
// 绝不允许反序；同一用户的所有欠款相关事务因此按固定顺序获取锁，
// MySQL/PostgreSQL 下不会出现环形等待。SQLite 无 FOR UPDATE 语法（见
// lockForUpdate），由单连接串行化保证等价语义。
//
// 用户行不存在时返回 gorm.ErrRecordNotFound，由调用方按各自语义处理
// （创建欠款仍保留记录；清偿/解冻拒绝；核销允许继续）。
func lockUserForDebtTx(tx *gorm.DB, userId int) error {
	var user User
	err := lockForUpdate(tx).Select("id").Where("id = ?", userId).First(&user).Error
	if err != nil {
		return err
	}
	return nil
}

// CreateDebtAndFreeze 在同一事务内完成"欠款记录 + 欠款冻结"：
//   - 幂等：同一任务（task_id）已有欠款记录时 no-op（不重复补扣/冻结/累计）；
//   - 管理员（role >= RoleAdminUser）不冻结，但欠款记录照建（isAdmin=true 供
//     调用方发出最高级别告警）；
//   - 事务提交成功后才失效用户缓存（冻结即时生效）与发送告警（由调用方负责）。
//
// 并发正确性（问题二）：事务开头先锁定用户行（lockUserForDebtTx），与清偿/
// 核销/显式解冻共用同一串行化边界——新建欠款对并发的 pending 检查与解冻
// 原子可见，反之亦然，绝不出现"pending 欠款已存在但用户未冻结"。
//
// 返回 (created, frozen, isAdmin, err)。
func CreateDebtAndFreeze(input DebtInput) (created bool, frozen bool, isAdmin bool, err error) {
	if input.UserId <= 0 || input.TaskId == "" || input.DeltaQuota <= 0 {
		return false, false, false, errors.New("invalid debt input")
	}
	now := time.Now().Unix()
	reason := input.Reason
	if reason == "" {
		reason = "seedance差额补扣失败"
	}
	err = DB.Transaction(func(tx *gorm.DB) error {
		// 0. 统一串行化边界：先锁用户行。用户缺失时仍保留欠款记录（可审计），
		//    冻结跳过（freezeUserForDebtTx 内部同样处理缺失）。
		if lerr := lockUserForDebtTx(tx, input.UserId); lerr != nil && !errors.Is(lerr, gorm.ErrRecordNotFound) {
			return lerr
		}
		// 1. 原子创建欠款（INSERT ... ON CONFLICT DO NOTHING，唯一索引 task_id
		//    兜底并发幂等——绝不先 Count 再 Insert，避免并发下双写竞态）。
		rec := &TaskBillingDebt{
			UserId:             input.UserId,
			TaskId:             input.TaskId,
			UpstreamTaskId:     input.UpstreamTaskId,
			ModelName:          input.ModelName,
			ChannelId:          input.ChannelId,
			PreConsumedQuota:   input.PreConsumedQuota,
			ActualQuota:        input.ActualQuota,
			DeltaQuota:         input.DeltaQuota,
			Reason:             reason,
			Status:             DebtStatusPending,
			CreatedAt:          now,
			UpdatedAt:          now,
			BillingSource:      input.BillingSource,
			SubscriptionId:     input.SubscriptionId,
			TokenId:            input.TokenId,
			Group:              input.Group,
			ConsumeLogRecorded: input.ConsumeLogRecorded,
			BillingStatsFailed: input.BillingStatsFailed,
		}
		res := tx.Clauses(clause.OnConflict{
			Columns:   []clause.Column{{Name: "task_id"}},
			DoNothing: true,
		}).Create(rec)
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			return ErrDebtAlreadyExists // 并发下另一实例已创建 → 幂等 no-op
		}
		// 2. 冻结用户（管理员跳过；用户行缺失时跳过冻结但保留欠款记录，数据可审计）
		frozen, isAdmin, err = freezeUserForDebtTx(tx, input.UserId, reason)
		if errors.Is(err, gorm.ErrRecordNotFound) {
			common.SysError(fmt.Sprintf("欠款用户不存在，欠款记录仍保留: user_id=%d task_id=%s delta=%d", input.UserId, input.TaskId, input.DeltaQuota))
			frozen, isAdmin = false, false
			err = nil
		}
		return err
	})
	if err != nil {
		if errors.Is(err, ErrDebtAlreadyExists) {
			return false, false, false, nil
		}
		return false, false, false, err
	}
	created = true
	if frozen {
		_ = invalidateUserCache(input.UserId)
	}
	return created, frozen, isAdmin, nil
}

// unfreezeUserDebtTx 解除欠款冻结（仅清零欠款冻结相关字段，绝不动 users.status，
// 因此不会误解除管理员手工禁用）。返回是否真正发生了状态迁移（false→true 或
// 已解冻的 no-op），供调用方决定是否记录解冻审计（并发幂等：只记录一次迁移）。
func unfreezeUserDebtTx(tx *gorm.DB, userId int) (bool, error) {
	res := tx.Model(&User{}).
		Where("id = ? AND debt_frozen = ?", userId, true).
		Updates(map[string]any{
			"debt_frozen":        false,
			"debt_frozen_at":     0,
			"debt_frozen_reason": "",
		})
	if res.Error != nil {
		return false, res.Error
	}
	// 行不存在（用户已删除）或未冻结：no-op 即可。
	return res.RowsAffected == 1, nil
}

// UnfreezeUserDebt 直接解除用户的欠款冻结（管理对账工具用；业务清偿走
// RepayTaskBillingDebt 自动判定）。不写审计（无管理员上下文）。
func UnfreezeUserDebt(userId int) error {
	err := DB.Transaction(func(tx *gorm.DB) error {
		_, err := unfreezeUserDebtTx(tx, userId)
		return err
	})
	if err != nil {
		return err
	}
	_ = invalidateUserCache(userId)
	return nil
}

// RepayDebtOptions 清偿选项。
type RepayDebtOptions struct {
	// AllowWalletOverflow 订阅欠款在订阅额度不足时是否允许钱包代偿。
	// 默认 false：订阅欠款只能从对应订阅清偿（明确策略）。允许代偿时必须由
	// 调用者（管理员）显式确认，清偿流程会记录资金来源切换（WalletOverflowed）
	// 并在日志中注明，绝不静默改变资金来源。
	AllowWalletOverflow bool
}

// RepayTaskBillingDebt 清偿一笔欠款（原子余额守卫 + 幂等）：
//  1. 事务内锁定欠款行，仅 status=pending 可清偿；
//  2. 资金来源按欠款快照选择：
//     - 钱包欠款：原子收款 quota >= DeltaQuota（单条条件 SQL，RowsAffected==1）；
//     - 订阅欠款：从对应订阅扣除（AmountTotal 上限守卫）；不足且
//       opts.AllowWalletOverflow → 钱包代偿并标记 WalletOverflowed（审计切换）；
//       否则 ErrDebtSubscriptionInsufficient（保留 pending 等待订阅恢复）。
//  3. 同步扣减 Token 差额（debt.TokenId>0，条件扣减；无限令牌无条件扣减）——
//     失败整体回滚，**绝不**先把欠款标成 paid（"资金已收 Token 未扣"不允许）；
//  4. 任务额度/用户/渠道累计收敛到实际额度；任务缺失 → ErrDebtMissingTask 回滚；
//     统计未写入（BillingStatsFailed）跳过累计补记（与提交口径一致）；
//  5. 欠款 paid + CollectedAt（RowsAffected 校验）；
//  6. 无其他未清欠款 → 解除欠款冻结（只动 debt_frozen，绝不动 users.status
//     手工禁用），并记录自动解冻审计；
//  7. 清偿动作本身写审计记录（adminId、原因、时间、债务 ID、用户 ID、额度）。
//  事务提交后再写差额消费日志（RecordDebtCollectionLog）、同步钱包缓存
//  （仅钱包收款）与 Token 缓存（debt.TokenId>0 且已扣）。
//  重复调用（同一债务已清偿）返回 ErrDebtNotFound；并发清偿/核销/解冻只会
//  有一个状态迁移成功（status=pending 条件更新）。
func RepayTaskBillingDebt(userId int, debtId int64, opts RepayDebtOptions, adminId int) error {
	if userId <= 0 || debtId <= 0 {
		return ErrDebtNotFound
	}
	var collectedFromWallet bool
	var tokenDeducted bool
	var walletOverflowed bool
	var capturedDebt *TaskBillingDebt
	var capturedTaskID string
	var tokenKey string // 问题三：事务内取得的 Token key
	err := DB.Transaction(func(tx *gorm.DB) error {
		// 0. 统一串行化边界（问题二）：先锁用户行，再锁债务行。
		//    pending 检查与 debt_frozen 迁移（第 6 步）与欠款创建（CreateDebtAndFreeze）
		//    共用同一用户锁——"查询 pending 后、更新 debt_frozen 前"被并发新建
		//    欠款插入的竞态被锁阻断：要么清偿事务先拿到锁（解冻判定看到最新
		//    欠款），要么创建事务先拿到锁（清偿解冻时 pending 已存在→不解冻）。
		//    锁顺序与 CreateDebtAndFreeze/VoidTaskBillingDebt/UnfreezeUserDebtAudited
		//    一致（用户行 → 债务行），避免 MySQL/PostgreSQL 死锁。
		if err := lockUserForDebtTx(tx, userId); err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrDebtNotFound // 用户不存在：无法收款
			}
			return err
		}
		// 1. 锁定欠款行
		var debt TaskBillingDebt
		if err := lockForUpdate(tx).Where("id = ? AND user_id = ?", debtId, userId).First(&debt).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrDebtNotFound
			}
			return err
		}
		if debt.Status != DebtStatusPending {
			return ErrDebtNotFound // 幂等：已清偿/已核销
		}
		capturedDebt = &debt
		capturedTaskID = debt.TaskId

		// 2. 资金来源收款（按欠款快照）
		switch debt.BillingSource {
		case "subscription":
			if debt.SubscriptionId <= 0 {
				return ErrDebtSubscriptionInsufficient
			}
			err := postConsumeUserSubscriptionDeltaTx(tx, debt.SubscriptionId, int64(debt.DeltaQuota))
			if err != nil {
				// 订阅额度不足（AmountTotal 守卫）且允许钱包代偿 → 切换资金来源
				if opts.AllowWalletOverflow && strings.Contains(err.Error(), "subscription used exceeds total") {
					if err := walletCollectDebtTx(tx, userId, debt.DeltaQuota); err != nil {
						return err
					}
					collectedFromWallet = true
					walletOverflowed = true
					// 显式记录资金来源切换（审计）——同时同步内存快照：提交后的
					// 消费日志（RecordDebtCollectionLog）必须看到"订阅不足，钱包
					// 代偿"事实，不能使用普通清偿原因（问题六）。
					debt.WalletOverflowed = true
					if err := tx.Model(&TaskBillingDebt{}).Where("id = ?", debt.ID).
						Update("wallet_overflowed", true).Error; err != nil {
						return err
					}
				} else {
					return ErrDebtSubscriptionInsufficient
				}
			}
		default: // 钱包欠款
			if err := walletCollectDebtTx(tx, userId, debt.DeltaQuota); err != nil {
				return err
			}
			collectedFromWallet = true
		}

		// 3. 同步扣减 Token 差额（debt.TokenId>0，条件扣减；无限令牌无条件扣减）——
		//    失败整体回滚，**绝不**先把欠款标成 paid（"资金已收 Token 未扣"不允许）；
		//    事务内取得 Token key 供提交后直接同步缓存（问题三）。
		if debt.TokenId > 0 {
			ok, key := applyTokenQuotaDeltaTx(tx, debt.TokenId, debt.DeltaQuota)
			tokenKey = key
			if !ok {
				return ErrDebtMissingToken // 令牌缺失/余额不足：保留 pending 可重试
			}
			tokenDeducted = true
		}

		// 4. 任务额度 + 累计消耗收敛到实际额度 A（任务缺失不得先标 paid）
		var task Task
		taskErr := tx.Where("task_id = ? AND user_id = ?", debt.TaskId, userId).First(&task).Error
		if taskErr != nil {
			if errors.Is(taskErr, gorm.ErrRecordNotFound) {
				return ErrDebtMissingTask
			}
			return taskErr
		}
		if err := applyTaskQuotaDeltaTx(tx, &task, debt.DeltaQuota); err != nil {
			return err
		}
		// 统计未写入的任务（BillingStatsFailed）不补记累计消耗，与提交口径一致。
		if !task.PrivateData.BillingStatsFailed {
			if err := applyUserUsedQuotaDelta(tx, userId, debt.DeltaQuota); err != nil {
				return err
			}
			if err := applyChannelUsedQuotaDelta(tx, task.ChannelId, debt.DeltaQuota); err != nil {
				return err
			}
		}

		// 5. 标记已收款（RowsAffected 校验）
		now := time.Now().Unix()
		res := tx.Model(&TaskBillingDebt{}).Where("id = ? AND status = ?", debt.ID, DebtStatusPending).
			Updates(map[string]any{
				"status":       DebtStatusPaid,
				"collected_at": now,
				"updated_at":   now,
			})
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected != 1 {
			return ErrDebtNotFound
		}

		// 6. 无其他未清欠款 → 解除欠款冻结（只动 debt_frozen，绝不动 users.status）
		var open int64
		if err := tx.Model(&TaskBillingDebt{}).
			Where("user_id = ? AND status = ?", userId, DebtStatusPending).
			Count(&open).Error; err != nil {
			return err
		}
		if open == 0 {
			if err := tx.Model(&TaskBillingDebt{}).Where("id = ?", debt.ID).
				Update("released_at", now).Error; err != nil {
				return err
			}
			unfrozen, err := unfreezeUserDebtTx(tx, userId)
			if err != nil {
				return err
			}
			if unfrozen {
				if err := recordTaskBillingDebtAuditTx(tx, &debt, 0, "auto_unfreeze", "全部欠款清偿后自动解除欠款冻结"); err != nil {
					return err
				}
			}
		}

		// 7. 清偿动作审计（管理员 ID、原因、时间、债务 ID、用户 ID、额度）
		reason := "欠款清偿（差额收款）"
		if walletOverflowed {
			reason = "欠款清偿（订阅不足，钱包代偿）"
		}
		if err := recordTaskBillingDebtAuditTx(tx, &debt, adminId, "repay", reason); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return err
	}
	// 事务提交后：写差额消费日志（幂等：清偿仅一次，paid 守卫保证）+
	// 钱包缓存同步（仅钱包收款）+ 失效用户缓存（冻结解除即时生效）+
	// Token 缓存同步（debt.TokenId>0 且已扣减，见问题六）。
	if capturedDebt != nil {
		if task, _, tErr := GetByTaskId(userId, capturedTaskID); tErr == nil {
			reason := "欠款清偿（差额收款）"
			if capturedDebt.WalletOverflowed {
				reason = "欠款清偿（订阅不足，钱包代偿）"
			}
			RecordDebtCollectionLog(capturedDebt, task, reason)
		}
	}
	if collectedFromWallet && common.RedisEnabled {
		if err := cacheIncrUserQuota(userId, -int64(capturedDebt.DeltaQuota)); err != nil {
			common.SysLog(fmt.Sprintf("failed to sync user quota cache after debt repayment: user_id=%d debt_id=%d error=%v", userId, debtId, err))
			_ = invalidateUserCache(userId)
		}
	}
	if tokenDeducted && capturedDebt.TokenId > 0 {
		syncTokenQuotaCacheAfterCommitWithKey(capturedDebt.TokenId, tokenKey, -int64(capturedDebt.DeltaQuota), "debt repayment")
	}
	_ = invalidateUserCache(userId)
	return nil
}

// walletCollectDebtTx 从钱包原子收款（quota >= delta 守卫，RowsAffected==1）。
func walletCollectDebtTx(tx *gorm.DB, userId int, delta int64) error {
	res := tx.Model(&User{}).
		Where("id = ? AND quota >= ?", userId, delta).
		Update("quota", gorm.Expr("quota - ?", delta))
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected != 1 {
		return ErrDebtInsufficientBalance
	}
	return nil
}

// VoidTaskBillingDebt 管理员核销欠款（人工对账场景：确认无法收款时标记 voided）。
// 仅管理员可调。核销 ≠ 收款，但必须解决"核销后用户永久 debt_frozen"的问题：
// 核销事务在用户不存在其他 pending 欠款时自动解除欠款冻结（只动 debt_frozen，
// 绝不动 users.status），并分别记录核销审计与自动解冻审计。
// 并发核销/清偿只会有一个状态迁移成功（status=pending 条件更新，RowsAffected 校验）。
func VoidTaskBillingDebt(debtId int64, adminUserId int, reason string) error {
	if debtId <= 0 {
		return ErrDebtNotFound
	}
	now := time.Now().Unix()
	var userID int
	var autoUnfrozen bool
	err := DB.Transaction(func(tx *gorm.DB) error {
		// 0a. 无锁读定位债务归属（不获取锁，保持"先用户后债务"的锁获取顺序）。
		var debt TaskBillingDebt
		if err := tx.Where("id = ?", debtId).First(&debt).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrDebtNotFound
			}
			return err
		}
		userID = debt.UserId
		// 0b. 统一串行化边界（问题二）：先锁用户行，再锁债务行。
		//     pending 检查与 debt_frozen 迁移（下方）与欠款创建共用同一用户锁，
		//     杜绝"核销判定无 pending 后、解冻前"被并发新建欠款插入的竞态。
		//     用户行缺失时仍允许核销（孤儿欠款管理操作），但 pending 检查与
		//     解冻判定仍在事务内执行（Count 不依赖用户行；解冻对缺失行 no-op）。
		if err := lockUserForDebtTx(tx, debt.UserId); err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		// 0c. 重新带锁读取债务行（状态校验用）。
		if err := lockForUpdate(tx).Where("id = ?", debtId).First(&debt).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrDebtNotFound
			}
			return err
		}
		if debt.Status != DebtStatusPending {
			return ErrDebtNotFound // 幂等：已清偿/已核销
		}
		userID = debt.UserId
		res := tx.Model(&TaskBillingDebt{}).
			Where("id = ? AND status = ?", debtId, DebtStatusPending).
			Updates(map[string]any{
				"status":     DebtStatusVoided,
				"reason":     truncateDebtReason(fmt.Sprintf("%s（管理员 %d 核销）", reason, adminUserId)),
				"updated_at": now,
			})
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected != 1 {
			return ErrDebtNotFound
		}
		// 核销动作审计（管理员 ID、原因、时间、债务 ID、用户 ID、额度）
		if err := recordTaskBillingDebtAuditTx(tx, &debt, adminUserId, "void", reason); err != nil {
			return err
		}
		// 无其他未清欠款 → 自动解除欠款冻结（核销不等于收款，但用户不应因
		// 一笔已核销债务被永久冻结；管理端仍可随时审计核销原因）。
		var open int64
		if err := tx.Model(&TaskBillingDebt{}).
			Where("user_id = ? AND status = ?", debt.UserId, DebtStatusPending).
			Count(&open).Error; err != nil {
			return err
		}
		if open == 0 {
			unfrozen, err := unfreezeUserDebtTx(tx, debt.UserId)
			if err != nil {
				return err
			}
			if unfrozen {
				autoUnfrozen = true
				if err := recordTaskBillingDebtAuditTx(tx, &debt, adminUserId, "auto_unfreeze", "核销后无未清欠款，自动解除欠款冻结"); err != nil {
					return err
				}
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	if autoUnfrozen {
		_ = invalidateUserCache(userID)
	}
	return nil
}

// UnfreezeUserDebtAudited 管理员显式解除用户的欠款冻结（受 AdminAuth 保护的
// 解冻入口）：带理由与审计记录（管理员 ID、原因、时间、债务 ID、用户 ID、额度）。
// 前置守卫：用户必须不存在任何未清欠款（ErrDebtPendingRemaining），否则拒绝
// 解冻——人工解冻不得绕过清偿闭环。只动 debt_frozen，绝不动 users.status。
// 幂等：用户已解冻时 no-op 成功（不重复写审计；状态迁移只成功一次）。
//
// 并发正确性（问题二）：事务开头先锁定用户行，pending 检查与 debt_frozen
// 迁移与欠款创建/清偿/核销共用同一串行化边界——"检查无 pending 后、解冻前"
// 被并发新建欠款插入的竞态被锁阻断（要么解冻事务先拿到锁并看到最新欠款，
// 要么创建事务先拿到锁使解冻判定看到 pending 而拒绝）。
func UnfreezeUserDebtAudited(userId int, debtId int64, adminId int, reason string) error {
	if userId <= 0 || debtId <= 0 {
		return ErrDebtNotFound
	}
	if reason == "" {
		reason = "管理员人工解冻"
	}
	err := DB.Transaction(func(tx *gorm.DB) error {
		// 0. 统一串行化边界：先锁用户行（债务归属校验与 pending 检查都在锁内执行）。
		if err := lockUserForDebtTx(tx, userId); err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrDebtNotFound
			}
			return err
		}
		// 债务归属校验（锁内读取，与清偿/核销互斥，防止债务被并发转移/修改）
		var debt TaskBillingDebt
		if err := tx.Where("id = ?", debtId).First(&debt).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrDebtNotFound
			}
			return err
		}
		if debt.UserId != userId {
			return ErrDebtNotFound // 债务归属校验：只能解冻自己的欠款上下文
		}
		var open int64
		if err := tx.Model(&TaskBillingDebt{}).
			Where("user_id = ? AND status = ?", userId, DebtStatusPending).
			Count(&open).Error; err != nil {
			return err
		}
		if open > 0 {
			return ErrDebtPendingRemaining
		}
		unfrozen, err := unfreezeUserDebtTx(tx, userId)
		if err != nil {
			return err
		}
		if !unfrozen {
			return nil // 已解冻：幂等 no-op（不重复写审计）
		}
		return recordTaskBillingDebtAuditTx(tx, &debt, adminId, "unfreeze", reason)
	})
	if err != nil {
		return err
	}
	_ = invalidateUserCache(userId)
	return nil
}

// RecordDebtCollectionLog 欠款收款后写一条补扣消费日志（与 RecalculateTaskQuota
// 的补扣日志口径一致：LogTypeConsume，Quota=差额）。返回是否写入。
func RecordDebtCollectionLog(debt *TaskBillingDebt, task *Task, reason string) bool {
	if debt == nil || task == nil {
		return false
	}
	if !task.PrivateData.ConsumeLogRecorded {
		return false
	}
	other := make(map[string]interface{})
	if bc := task.PrivateData.BillingContext; bc != nil {
		other["model_price"] = bc.ModelPrice
		if bc.ModelRatio > 0 {
			other["model_ratio"] = bc.ModelRatio
		}
		other["group_ratio"] = bc.GroupRatio
	}
	other["task_id"] = debt.TaskId
	other["pre_consumed_quota"] = debt.PreConsumedQuota
	other["actual_quota"] = debt.ActualQuota
	other["debt_collection"] = true
	RecordTaskBillingLog(RecordTaskBillingLogParams{
		UserId:    debt.UserId,
		LogType:   LogTypeConsume,
		Content:   reason,
		ChannelId: debt.ChannelId,
		ModelName: debt.ModelName,
		Quota:     debt.DeltaQuota,
		TokenId:   task.PrivateData.TokenId,
		Group:     task.Group,
		Other:     other,
		NodeName:  task.PrivateData.NodeName,
	})
	return true
}

// truncateDebtReason 把原因安全截断到 varchar(255) 的**字节**上限（数据库按
// 字节计），并保证不切断多字节 UTF-8 字符（按 rune 累积，绝不按字节硬切）。
func truncateDebtReason(reason string) string {
	const maxBytes = 255
	if len(reason) <= maxBytes {
		return reason
	}
	var sb strings.Builder
	bytes := 0
	for _, r := range reason {
		rSize := len(string(r))
		if bytes+rSize > maxBytes {
			break
		}
		sb.WriteRune(r)
		bytes += rSize
	}
	return sb.String()
}
