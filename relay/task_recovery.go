package relay

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	taskdoubao "github.com/QuantumNous/new-api/relay/channel/task/doubao"
	relaycommon "github.com/QuantumNous/new-api/relay/common"

	"github.com/gin-gonic/gin"
)

// taskListFetcher 候选发现所需的"查询内容生成任务列表"能力。
// 由各适配器按需实现（当前仅 doubao/Seedance 实现）。
type taskListFetcher interface {
	ListTasks(baseUrl, key string, body map[string]any, proxy string) (*http.Response, error)
}

// recoveryCandidate 候选发现结果条目（仅保留可展示的最小字段，不含提示词原文）。
type recoveryCandidate struct {
	UpstreamTaskID string `json:"upstream_task_id"`
	Model          string `json:"model"`
	Status         string `json:"status"`
	CreatedAt      int64  `json:"created_at"`
}

// discoveryTimeWindow 候选发现的时间窗（提交后的窗口，覆盖重试与查询延迟）。
const discoveryTimeWindow = 30 * time.Minute

// discoveryClockSkew 候选发现的提交前时钟偏差容忍（很小的窗口，避免把
// 提交之前创建的无任务扫入候选）。
const discoveryClockSkew = 5 * time.Minute

// buildRecoveryRecord 从本次提交的 RelayInfo 构建恢复记录（不含敏感请求内容）。
// reservedSlot 表示本次提交是否已预留 Seedance 并发名额（Seedance 渠道且启用
// 限制时预留）；为 true 时恢复记录持有该名额（上游可能/确定已创建任务但本地
// 无任务行，名额不能随请求结束释放）。
func buildRecoveryRecord(c *gin.Context, info *relaycommon.RelayInfo, fingerprint, contentFingerprint, outcome, status string, reservedSlot bool) *model.TaskSubmitRecovery {
	rec := &model.TaskSubmitRecovery{
		UserId:              info.UserId,
		Platform:            strconv.Itoa(c.GetInt("channel_type")),
		Model:               info.OriginModelName,
		UpstreamModelName:   info.UpstreamModelName,
		ChannelId:           info.ChannelId,
		ChannelType:         info.ChannelType,
		ChannelBaseURL:      info.ChannelBaseUrl,
		IdempotencyKey:      info.EnsureTaskIdempotencyKey(),
		ClientRequestID:     info.EnsureTaskClientRequestID(),
		Fingerprint:         fingerprint,
		ContentFingerprint:  contentFingerprint,
		Outcome:             outcome,
		Status:              status,
		ConcurrencyReserved: reservedSlot,
		Attempt:             1,
		FirstSubmitTime:     info.StartTime.Unix(),
		SubmitTime:          time.Now().Unix(),
	}
	if info.TaskRelayInfo != nil {
		rec.PublicTaskID = info.TaskRelayInfo.PublicTaskID
		if info.TaskRelayInfo.RecoveryParentID > 0 {
			// 人工重试链：继承父记录的尝试次数并建立父子关联
			if parent, err := model.GetTaskSubmitRecoveryByID(info.TaskRelayInfo.RecoveryParentID, info.UserId); err == nil && parent != nil {
				rec.Attempt = parent.Attempt + 1
			}
			rec.ParentID = info.TaskRelayInfo.RecoveryParentID
		}
	}
	return rec
}

// PersistOutcomeUnknown 持久化"结果未知"的创建请求（结果分类：
// 连接中断、读取响应超时、200 但无 task_id 等）。调用方（控制器）必须
// 在此之后停止自动重试，禁止再次 POST。
//
// 安全：只记录错误类型标识（timeout / conn_reset / parse / other），
// 绝不把错误原文（可能回显请求内容）写入 Note 或日志。
// reservedSlot：本次提交是否已预留 Seedance 并发名额（true 时记录持有名额）。
func PersistOutcomeUnknown(c *gin.Context, info *relaycommon.RelayInfo, fingerprint, contentFingerprint string, err error, reservedSlot bool) (*model.TaskSubmitRecovery, error) {
	rec := buildRecoveryRecord(c, info, fingerprint, contentFingerprint,
		relaycommon.TaskSubmitOutcomeOutcomeUnknown.String(), model.TaskRecoveryStatusUnknown, reservedSlot)
	rec.Note = "outcome_unknown: " + classifyErrorKind(err)
	if e := rec.Insert(); e != nil {
		return nil, e
	}
	return rec, nil
}

// classifyErrorKind 把错误归类为简短的非敏感标识（不包含任何错误原文）。
//
// 识别顺序（优先标准错误机制，文本兜底只作最后手段）：
//  1. errors.Is(err, context.DeadlineExceeded) —— 超时哨兵错误；
//  2. errors.As 到 net.Error 且 Timeout() 为 true —— http.Client 的
//     Client.Timeout 会包成 *url.Error（其 Timeout() 透传底层超时）；
//  3. 遍历完整单链 errors.Unwrap 收集链上全部错误文本后做关键词匹配。
//     不能只检查顶层 err.Error()：doRequest 经 ErrOptionWithHideErrMsg
//     （relaykit/types）把对外文案替换为通用文本，原始错误在 Unwrap 链中。
//
// 只读错误链做分类，绝不把错误原文写入恢复记录或客户端响应。
func classifyErrorKind(err error) string {
	if err == nil {
		return "unknown"
	}

	// 1) 标准错误机制：上下文超时哨兵
	if errors.Is(err, context.DeadlineExceeded) {
		return "timeout"
	}

	// 2) net.Error 且 Timeout() 为 true（含 *url.Error 透传的客户端超时）
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return "timeout"
	}

	// 3) 遍历完整单链收集全部错误文本（覆盖隐藏文案包装的底层原因）
	var text strings.Builder
	for e := err; e != nil; {
		text.WriteString(e.Error())
		text.WriteByte(' ')
		u := errors.Unwrap(e)
		if u == nil {
			break
		}
		e = u
	}
	lower := strings.ToLower(text.String())
	switch {
	case strings.Contains(lower, "timeout"), strings.Contains(lower, "deadline exceeded"),
		strings.Contains(lower, "client.timeout"):
		return "timeout"
	case strings.Contains(lower, "connection reset"), strings.Contains(lower, "broken pipe"),
		strings.Contains(lower, "connection closed"), strings.Contains(lower, "eof"):
		return "conn_reset"
	case strings.Contains(lower, "unmarshal"), strings.Contains(lower, "parse"):
		return "parse"
	default:
		return "other"
	}
}

// RecoverTaskAfterInsertFailure 处理"已取得上游 task_id 但本地任务落库失败"：
// 先重试一次本地保存（可能是瞬时 DB 错误），仍失败则用该 task_id 调用官方
// 查询接口确认任务状态并记录恢复记录。绝不再次 POST 创建任务。
//
// 安全：Note 只记录错误类别与上游状态，不写入落库错误的原文（避免敏感内容落库）。
//
// reservedSlot：本次提交是否已预留 Seedance 并发名额（true 时恢复记录持有名额，
// 因为上游任务已确定存在而本地无任务行，名额不能随请求结束释放）。
//
// 返回值：是否创建了"占用并发名额"的恢复记录。重试 Insert 成功（任务行已存在）
// 或未预留名额时返回 false——前者名额归任务生命周期，后者本就无名额可释放。
func RecoverTaskAfterInsertFailure(c *gin.Context, info *relaycommon.RelayInfo, task *model.Task, upstreamTaskID string, insertErr error, reservedSlot bool) bool {
	if task == nil || upstreamTaskID == "" {
		common.SysError("recover task after insert failure: missing task/upstream id")
		return false
	}
	// 重试一次本地保存（瞬时 DB 错误可自愈）→ 任务行已存在，名额归任务生命周期
	if retryErr := task.Insert(); retryErr == nil {
		common.SysLog(fmt.Sprintf("task %s insert retried successfully after transient failure", task.TaskID))
		return false
	}

	rec := buildRecoveryRecord(c, info, "", "",
		relaycommon.TaskSubmitOutcomeConfirmedSuccess.String(), model.TaskRecoveryStatusAssociated, reservedSlot)
	rec.UpstreamTaskID = upstreamTaskID
	rec.Note = "本地任务落库失败，已用上游 task_id 查询确认（类别: " + classifyErrorKind(insertErr) + "），未重复创建"

	// 用 task_id 调用官方查询接口恢复/记录上游状态
	if status, err := fetchUpstreamTaskStatus(info, upstreamTaskID); err == nil && status != "" {
		rec.Note = "本地任务落库失败，已用上游 task_id 查询确认（上游状态=" + status + "），未重复创建"
	}
	if e := rec.Insert(); e != nil {
		common.SysError("insert task recovery after insert failure error: " + e.Error())
		return false
	}
	return rec.ConcurrencyReserved
}

// fetchUpstreamTaskStatus 用官方查询接口按 task_id 查询任务状态（GET，非 POST）。
func fetchUpstreamTaskStatus(info *relaycommon.RelayInfo, upstreamTaskID string) (string, error) {
	adaptor := GetTaskAdaptorForChannel(info.ChannelType)
	if adaptor == nil {
		return "", fmt.Errorf("no task adaptor for channel type %d", info.ChannelType)
	}
	resp, err := adaptor.FetchTask(info.ChannelBaseUrl, info.ApiKey, map[string]any{
		"task_id": upstreamTaskID,
		"action":  constant.TaskActionGenerate,
	}, info.ChannelSetting.Proxy)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("fetch upstream task %s failed: status %d", upstreamTaskID, resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	ti, err := adaptor.ParseTaskResult(body)
	if err != nil {
		return "", err
	}
	if ti == nil {
		return "", fmt.Errorf("empty task result")
	}
	return ti.Status, nil
}

// DiscoverRecoveryCandidates 执行"候选发现"：用官方查询接口按 model 拉取任务，
// 客户端侧按时间窗 + 内容指纹做模糊匹配。
//
//   - 0 个候选：状态保持 unknown，不自动确认；
//   - 唯一候选：状态标记 inferred（"推测关联"，仍需人工确认，绝不视为强一致）；
//   - 多个候选：状态标记 ambiguous，不自动选择。
//
// 绝不将模糊匹配描述为强一致确认。
// 仅允许在非终态（unknown/inferred/ambiguous）上执行：终态（associated/recreated）
// 不允许被 discover 重新打开。
func DiscoverRecoveryCandidates(c *gin.Context, recovery *model.TaskSubmitRecovery) (*model.TaskSubmitRecovery, error) {
	if recovery == nil {
		return nil, fmt.Errorf("missing recovery record")
	}
	switch recovery.Status {
	case model.TaskRecoveryStatusAssociated:
		return recovery, fmt.Errorf("recovery %d already associated with upstream task %s", recovery.ID, recovery.UpstreamTaskID)
	case model.TaskRecoveryStatusRecreated:
		return recovery, fmt.Errorf("recovery %d is already recreated (terminal state), discovery not allowed", recovery.ID)
	}

	ch, err := model.GetChannelById(recovery.ChannelId, true)
	if err != nil {
		return recovery, fmt.Errorf("get channel failed: %w", err)
	}
	if ch == nil || ch.Status != common.ChannelStatusEnabled {
		return recovery, fmt.Errorf("channel %d is not enabled", recovery.ChannelId)
	}

	adaptor := GetTaskAdaptorForChannel(recovery.ChannelType)
	lister, ok := adaptor.(taskListFetcher)
	if !ok || adaptor == nil {
		return recovery, fmt.Errorf("channel type %d does not support task list query", recovery.ChannelType)
	}

	// 用上游模型名过滤（存在模型映射时 recovery.Model 是用户侧模型名，
	// 上游任务只会带上游模型名；未映射时回退到 Model）。
	upstreamModel := recovery.UpstreamModelName
	if upstreamModel == "" {
		upstreamModel = recovery.Model
	}
	resp, err := lister.ListTasks(ch.GetBaseURL(), ch.Key, map[string]any{
		"model":     upstreamModel,
		"page_size": 100,
	}, ch.GetSetting().Proxy)
	if err != nil {
		return recovery, fmt.Errorf("list tasks failed: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return recovery, fmt.Errorf("read list response failed: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return recovery, fmt.Errorf("list tasks failed: status %d", resp.StatusCode)
	}
	items, err := taskdoubao.ParseTaskList(body)
	if err != nil {
		return recovery, fmt.Errorf("parse list response failed: %w", err)
	}

	// 时间窗锚定在首次提交时间：重复任务只能创建于提交时刻附近
	// （提交前仅容忍小量时钟偏差，提交后覆盖重试与查询延迟），
	// 不使用"当前时间"作为窗口上界（否则发现时机越晚越容易扫入无关新任务）。
	windowStart := recovery.FirstSubmitTime - int64(discoveryClockSkew.Seconds())
	windowEnd := recovery.FirstSubmitTime + int64(discoveryTimeWindow.Seconds())
	var candidates []recoveryCandidate
	for _, it := range items {
		createdAt := int64(it.CreatedAt)
		if createdAt < windowStart || createdAt > windowEnd {
			continue
		}
		if it.ContentFingerprint() == recovery.ContentFingerprint {
			candidates = append(candidates, recoveryCandidate{
				UpstreamTaskID: it.ID,
				Model:          it.Model,
				Status:         it.Status,
				CreatedAt:      createdAt,
			})
		}
	}

	candJSON, _ := common.Marshal(candidates)
	newStatus := model.TaskRecoveryStatusUnknown
	newNote := ""
	switch len(candidates) {
	case 0:
		newStatus = model.TaskRecoveryStatusUnknown
		newNote = "候选发现：0 个匹配候选，无法自动确认（未创建或参数不可比）"
	case 1:
		// 唯一候选：仅记录"推测关联"，仍需人工确认
		newStatus = model.TaskRecoveryStatusInferred
		newNote = "推测关联：唯一候选（时间窗+内容指纹模糊匹配），仍需人工确认，非强一致确认"
	default:
		newStatus = model.TaskRecoveryStatusAmbiguous
		newNote = "模糊匹配：多个候选，不自动选择，请人工查询确认"
	}

	// 条件更新：仅当状态仍为非终态（unknown/inferred/ambiguous）时写入结果。
	// 若上游查询期间被并发 recreate/associate 占位，此处必须失败并丢弃结果，
	// 绝不覆盖并发操作写下的终态（discover 不得重新打开 recreated/associated）。
	updated, err := model.UpdateRecoveryDiscoveryResult(recovery.ID, int64(recovery.UserId), newStatus, string(candJSON), newNote)
	if err != nil {
		return recovery, fmt.Errorf("persist discovery result failed: %w", err)
	}
	if !updated {
		return recovery, fmt.Errorf("recovery %d state changed concurrently (claimed/associated)，discovery result discarded", recovery.ID)
	}
	recovery.Status = newStatus
	recovery.Candidates = string(candJSON)
	recovery.Note = newNote
	return recovery, nil
}

// VerifyUpstreamTask 用官方查询接口验证某个上游 task_id 确实存在。
// 仅用于"人工确认后关联"，验证失败返回错误（不自动放行）。
func VerifyUpstreamTask(rec *model.TaskSubmitRecovery, upstreamTaskID string) error {
	if rec == nil {
		return fmt.Errorf("missing recovery record")
	}
	ch, err := model.GetChannelById(rec.ChannelId, true)
	if err != nil {
		return fmt.Errorf("get channel failed: %w", err)
	}
	if ch == nil || ch.Status != common.ChannelStatusEnabled {
		return fmt.Errorf("channel %d is not enabled", rec.ChannelId)
	}
	adaptor := GetTaskAdaptorForChannel(rec.ChannelType)
	if adaptor == nil {
		return fmt.Errorf("no task adaptor for channel type %d", rec.ChannelType)
	}
	resp, err := adaptor.FetchTask(ch.GetBaseURL(), ch.Key, map[string]any{
		"task_id": upstreamTaskID,
		"action":  constant.TaskActionGenerate,
	}, ch.GetSetting().Proxy)
	if err != nil {
		return fmt.Errorf("verify upstream task failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("upstream task %s not found (status %d)", upstreamTaskID, resp.StatusCode)
	}
	return nil
}

// GetTaskAdaptorForChannel 根据渠道类型返回任务适配器（nil 安全）。
func GetTaskAdaptorForChannel(channelType int) interface {
	FetchTask(baseUrl, key string, body map[string]any, proxy string) (*http.Response, error)
	ParseTaskResult(respBody []byte) (*relaycommon.TaskInfo, error)
} {
	adaptor := GetTaskAdaptor(constant.TaskPlatform(fmt.Sprintf("%d", channelType)))
	if adaptor == nil {
		return nil
	}
	return adaptor
}
