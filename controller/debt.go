package controller

import (
	"strconv"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"

	"github.com/gin-gonic/gin"
)

// ---------------------------------------------------------------------------
// Seedance 欠款管理（管理员受鉴权入口）
// 清偿/核销只允许 AdminAuth 调用；清偿默认不允许订阅欠款钱包代偿，
// 只有调用方显式传 allow_wallet_overflow=true 时才允许（并记录资金来源切换）。
// ---------------------------------------------------------------------------

type repayDebtRequest struct {
	AllowWalletOverflow bool `json:"allow_wallet_overflow"` // 订阅欠款是否允许钱包代偿
}

// AdminRepayTaskDebt 管理员代用户清偿一笔欠款。
// POST /api/user/debt/:id/repay（AdminAuth）
func AdminRepayTaskDebt(c *gin.Context) {
	debtID, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || debtID <= 0 {
		common.ApiErrorMsg(c, "invalid debt id")
		return
	}
	var req repayDebtRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		common.ApiErrorMsg(c, "invalid request body")
		return
	}

	// 欠款归属校验：先读记录确认存在，避免任意 ID 清偿他人欠款
	debt, err := model.GetTaskBillingDebtByID(debtID)
	if err != nil {
		common.ApiErrorMsg(c, "debt not found")
		return
	}

	adminID := c.GetInt("id")
	opts := model.RepayDebtOptions{AllowWalletOverflow: req.AllowWalletOverflow}
	if err := model.RepayTaskBillingDebt(debt.UserId, debtID, opts, adminID); err != nil {
		switch {
		case err == model.ErrDebtNotFound:
			common.ApiErrorMsg(c, "debt already settled or voided")
		case err == model.ErrDebtInsufficientBalance:
			common.ApiErrorMsg(c, "用户钱包余额不足，无法清偿（请先充值）")
		case err == model.ErrDebtSubscriptionInsufficient:
			common.ApiErrorMsg(c, "订阅额度不足，且未允许钱包代偿")
		case err == model.ErrDebtMissingTask:
			common.ApiErrorMsg(c, "欠款关联任务缺失，无法完成清偿（请人工对账）")
		case err == model.ErrDebtMissingToken:
			common.ApiErrorMsg(c, "欠款关联令牌缺失或额度不足，无法完成清偿（请人工对账）")
		default:
			common.ApiErrorMsg(c, "清偿失败："+err.Error())
		}
		return
	}
	common.ApiSuccess(c, gin.H{"debt_id": debtID, "user_id": debt.UserId, "delta_quota": debt.DeltaQuota})
}

// AdminVoidTaskDebt 管理员核销一笔欠款（人工对账：确认无法收款时）。
// 核销 ≠ 收款：欠款记录保留审计；核销后用户无其他未清欠款时自动解除欠款
// 冻结（只动 debt_frozen，绝不动 users.status）。
// POST /api/user/debt/:id/void?reason=xxx（AdminAuth）
func AdminVoidTaskDebt(c *gin.Context) {
	debtID, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || debtID <= 0 {
		common.ApiErrorMsg(c, "invalid debt id")
		return
	}
	reason := c.Query("reason")
	if reason == "" {
		reason = "管理员核销"
	}
	adminID := c.GetInt("id")
	if err := model.VoidTaskBillingDebt(debtID, adminID, reason); err != nil {
		if err == model.ErrDebtNotFound {
			common.ApiErrorMsg(c, "debt not found or already settled")
		} else {
			common.ApiErrorMsg(c, "核销失败："+err.Error())
		}
		return
	}
	common.ApiSuccess(c, gin.H{"debt_id": debtID, "status": model.DebtStatusVoided})
}

// AdminUnfreezeUserDebt 管理员显式解除一笔欠款关联用户的欠款冻结（人工解冻入口，
// 带理由与审计记录）。用户仍存在未清欠款时拒绝解冻（不得绕过清偿闭环）。
// POST /api/user/debt/:id/unfreeze?reason=xxx（AdminAuth）
func AdminUnfreezeUserDebt(c *gin.Context) {
	debtID, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || debtID <= 0 {
		common.ApiErrorMsg(c, "invalid debt id")
		return
	}
	// 欠款归属校验：先读记录确认存在
	debt, err := model.GetTaskBillingDebtByID(debtID)
	if err != nil {
		common.ApiErrorMsg(c, "debt not found")
		return
	}
	reason := c.Query("reason")
	if reason == "" {
		reason = "管理员人工解冻"
	}
	adminID := c.GetInt("id")
	if err := model.UnfreezeUserDebtAudited(debt.UserId, debtID, adminID, reason); err != nil {
		switch {
		case err == model.ErrDebtNotFound:
			common.ApiErrorMsg(c, "debt not found")
		case err == model.ErrDebtPendingRemaining:
			common.ApiErrorMsg(c, "用户仍有未清欠款，无法解冻（请先清偿或核销）")
		default:
			common.ApiErrorMsg(c, "解冻失败："+err.Error())
		}
		return
	}
	common.ApiSuccess(c, gin.H{"debt_id": debtID, "user_id": debt.UserId, "status": "unfrozen"})
}

// AdminListTaskDebtAudits 分页查询欠款审计记录（对账/审计用）。
// GET /api/user/debt/audits?debt_id=&user_id=&page=&size=（AdminAuth）
func AdminListTaskDebtAudits(c *gin.Context) {
	pageInfo := common.GetPageQuery(c)
	debtID, _ := strconv.ParseInt(c.Query("debt_id"), 10, 64)
	userID, _ := strconv.Atoi(c.Query("user_id"))
	items, total, err := model.ListTaskBillingDebtAudits(debtID, userID, pageInfo.GetStartIdx(), pageInfo.GetPageSize())
	if err != nil {
		common.ApiErrorMsg(c, err.Error())
		return
	}
	pageInfo.SetTotal(int(total))
	pageInfo.SetItems(items)
	common.ApiSuccess(c, pageInfo)
}

// AdminListTaskDebts 分页查询欠款记录（审计/对账用）。
// GET /api/user/debt?user_id=&status=&page=&size=（AdminAuth）
func AdminListTaskDebts(c *gin.Context) {
	pageInfo := common.GetPageQuery(c)
	userID, _ := strconv.Atoi(c.Query("user_id"))
	status := c.Query("status")
	items, total, err := model.PageTaskBillingDebts(userID, status, pageInfo.GetStartIdx(), pageInfo.GetPageSize())
	if err != nil {
		common.ApiErrorMsg(c, err.Error())
		return
	}
	pageInfo.SetTotal(int(total))
	pageInfo.SetItems(items)
	common.ApiSuccess(c, pageInfo)
}
