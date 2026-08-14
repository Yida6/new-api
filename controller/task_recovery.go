package controller

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/relay"

	"github.com/gin-gonic/gin"
)

// ---------------------------------------------------------------------------
// 任务恢复入口（outcome_unknown 的人工确认与恢复）
//
// 背景：Seedance 创建接口不支持服务端幂等，响应结果未知（超时/连接中断）的
// POST 已由 RelayTask 停止自动重试并持久化恢复记录。本组接口让用户：
//   1. 查看自己的 outcome_unknown 记录；
//   2. 通过官方查询接口确认任务已创建后，关联上游 task_id；
//   3. 对记录执行"候选发现"（model+内容指纹+时间窗模糊匹配，绝不自动关联）；
//   4. 显式确认"承担重复创建风险"后重新创建（生成新的逻辑尝试记录）。
// ---------------------------------------------------------------------------

// taskRecoveryDTO 恢复记录的对外响应 DTO。
// 只暴露恢复流程所需字段，刻意不暴露：ChannelBaseURL / ChannelId / Fingerprint /
// ContentFingerprint / IdempotencyKey / ClientRequestID / UpstreamModelName
// （可能为方舟 Endpoint ID）等内部或敏感信息。
type taskRecoveryDTO struct {
	ID              int64  `json:"id"`
	PublicTaskID    string `json:"public_task_id,omitempty"`
	Platform        string `json:"platform"`
	Model           string `json:"model"`
	ChannelType     int    `json:"channel_type"`
	Status          string `json:"status"`
	Outcome         string `json:"outcome"`
	UpstreamTaskID  string `json:"upstream_task_id,omitempty"`
	Candidates      string `json:"candidates,omitempty"`
	Attempt         int    `json:"attempt"`
	ParentID        int64  `json:"parent_id,omitempty"`
	FirstSubmitTime int64  `json:"first_submit_time"`
	SubmitTime      int64  `json:"submit_time"`
	CreatedAt       int64  `json:"created_at"`
	UpdatedAt       int64  `json:"updated_at"`
	Note            string `json:"note,omitempty"`
}

func toRecoveryDTO(r *model.TaskSubmitRecovery) taskRecoveryDTO {
	return taskRecoveryDTO{
		ID:              r.ID,
		PublicTaskID:    r.PublicTaskID,
		Platform:        r.Platform,
		Model:           r.Model,
		ChannelType:     r.ChannelType,
		Status:          r.Status,
		Outcome:         r.Outcome,
		UpstreamTaskID:  r.UpstreamTaskID,
		// Candidates 为 JSON 文本，其中 model 字段可能携带上游回显的
		// 方舟 Endpoint ID，对外返回前统一脱敏。
		Candidates:      common.RedactCredentials(r.Candidates),
		Attempt:         r.Attempt,
		ParentID:        r.ParentID,
		FirstSubmitTime: r.FirstSubmitTime,
		SubmitTime:      r.SubmitTime,
		CreatedAt:       r.CreatedAt,
		UpdatedAt:       r.UpdatedAt,
		Note:            r.Note,
	}
}

// recoveryErrorMsg 对返回给用户的恢复流程错误信息做敏感信息脱敏：
// 上游错误文本可能携带渠道 URL、方舟 Endpoint ID 或 API Key。
func recoveryErrorMsg(err error) string {
	if err == nil {
		return ""
	}
	return common.MaskSensitiveInfo(err.Error())
}

// GetTaskRecoveries 查看当前用户的恢复记录（可按状态过滤，默认只看 outcome_unknown）。
func GetTaskRecoveries(c *gin.Context) {
	userId := c.GetInt("id")
	status := c.Query("status")
	page, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
	pageSize, _ := strconv.Atoi(c.DefaultQuery("page_size", "20"))

	list, total, err := model.GetTaskSubmitRecoveriesByUser(userId, status, page, pageSize)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": recoveryErrorMsg(err)})
		return
	}
	items := make([]taskRecoveryDTO, 0, len(list))
	for _, r := range list {
		items = append(items, toRecoveryDTO(r))
	}
	c.JSON(http.StatusOK, gin.H{
		"total": total,
		"items": items,
	})
}

// AssociateTaskRecovery 人工确认任务已创建后，关联上游 task_id。
// 关联前会调用官方查询接口尽力验证该 task_id 确实存在；验证失败则拒绝关联。
func AssociateTaskRecovery(c *gin.Context) {
	userId := c.GetInt("id")
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid recovery id"})
		return
	}
	var body struct {
		UpstreamTaskID string `json:"upstream_task_id"`
		Note           string `json:"note"`
	}
	if err := common.UnmarshalBodyReusable(c, &body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}
	upstreamTaskID := strings.TrimSpace(body.UpstreamTaskID)
	if upstreamTaskID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "upstream_task_id is required"})
		return
	}

	rec, err := model.GetTaskSubmitRecoveryByID(id, userId)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "recovery record not found"})
		return
	}
	if rec.Status == model.TaskRecoveryStatusAssociated {
		c.JSON(http.StatusBadRequest, gin.H{"error": "该记录已关联上游任务（task_id=" + rec.UpstreamTaskID + "）"})
		return
	}

	// 用官方查询接口验证任务存在（尽力而为；验证失败不允许关联）
	if err := relay.VerifyUpstreamTask(rec, upstreamTaskID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": "无法通过官方查询接口确认该 task_id 存在: " + recoveryErrorMsg(err),
		})
		return
	}

	// 验证通过后,用条件更新原子地关联：仅在状态仍为 unknown/inferred/ambiguous 时成功。
	// 若验证期间被并发 recreate 原子占位（或已被并发 associate 关联），本次关联失败(409)，
	// 保证"关联"与"人工重试"同一时刻只允许一种操作成功。
	note := "人工通过官方查询接口确认后关联上游 task_id"
	if body.Note != "" {
		note = "人工关联确认: " + body.Note
	}
	associated, err := model.MarkRecoveryAssociated(id, int64(userId), upstreamTaskID, note)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": recoveryErrorMsg(err)})
		return
	}
	if !associated {
		c.JSON(http.StatusConflict, gin.H{"error": "该恢复记录已被并发处理（已关联或已人工重试），请刷新后重试"})
		return
	}
	// 重新加载后统一走 DTO，不向调用方暴露内部字段
	updated, err := model.GetTaskSubmitRecoveryByID(id, userId)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": recoveryErrorMsg(err)})
		return
	}
	c.JSON(http.StatusOK, toRecoveryDTO(updated))
}

// DiscoverTaskRecoveryCandidates 执行"候选发现"：
// 用官方查询接口按 model 拉取任务，客户端侧按时间窗 + 内容指纹做模糊匹配。
// 唯一候选 → 状态 inferred（推测关联，仍需人工确认）；多个候选 → 状态 ambiguous，
// 不自动选择；绝不将模糊匹配描述为强一致确认。
func DiscoverTaskRecoveryCandidates(c *gin.Context) {
	userId := c.GetInt("id")
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid recovery id"})
		return
	}
	rec, err := model.GetTaskSubmitRecoveryByID(id, userId)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "recovery record not found"})
		return
	}

	updated, err := relay.DiscoverRecoveryCandidates(c, rec)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": recoveryErrorMsg(err)})
		return
	}
	// 统一走 DTO，不向调用方暴露内部字段
	c.JSON(http.StatusOK, toRecoveryDTO(updated))
}

// RecreateTaskRecovery 用户显式确认"承担重复创建风险"后重新创建。
// 请求体与普通创建任务相同，且必须携带 confirm=true；会生成新的逻辑尝试记录
// （原记录原子标记为 recreated，新尝试通过 ParentID 关联），并在响应头给出风险提示。
// 原子守卫保证并发/重复点击时只有一个请求能进入创建流程（数据库级互斥）。
func RecreateTaskRecovery(c *gin.Context) {
	userId := c.GetInt("id")
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid recovery id"})
		return
	}
	rec, err := model.GetTaskSubmitRecoveryByID(id, userId)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "recovery record not found"})
		return
	}
	if rec.Status == model.TaskRecoveryStatusAssociated {
		c.JSON(http.StatusBadRequest, gin.H{"error": "该记录已关联上游任务，重新创建必然产生重复任务；如需恢复请使用关联结果"})
		return
	}

	// 读取 confirm 标记（同时缓存请求体，供 RelayTask 复用）
	var confirmReq struct {
		Confirm bool `json:"confirm"`
	}
	if err := common.UnmarshalBodyReusable(c, &confirmReq); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}
	if !confirmReq.Confirm {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": "重新创建可能产生重复任务，请显式确认（confirm=true）",
		})
		return
	}

	// 原子占位：仅在 unknown/inferred/ambiguous 状态成功，防止并发重复创建。
	// 若已被其他请求先占位（recreated）或已关联（associated），直接拒绝。
	claimed, err := model.MarkRecoveryRecreated(id, int64(userId), "用户确认承担重复创建风险后发起人工重试（处理中）")
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": recoveryErrorMsg(err)})
		return
	}
	if !claimed {
		c.JSON(http.StatusConflict, gin.H{
			"error": "该恢复记录已有人工重试在处理或已关联上游任务，请勿重复操作",
		})
		return
	}

	// 注入恢复上下文，交由 RelayTask 执行新逻辑尝试（新幂等键/新 X-Client-Request-Id）；
	// RelayTask 完成后会把新尝试结果回填到本记录的 note。
	c.Set("task_recovery_id", id)
	c.Set("task_recovery_confirm", true)
	c.Header("X-New-Api-Duplicate-Risk", "user-confirmed: possible duplicate task")
	RelayTask(c)
}
