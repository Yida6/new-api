package common

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	"github.com/google/uuid"
)

// ---------------------------------------------------------------------------
// 客户端本地幂等键（Idempotency Key）—— 仅本地去重/关联/审计
//
// 生成策略：每次"逻辑上的新建任务"生成一个 UUID v4，保存在 TaskRelayInfo 上，
// 进入重试循环之前生成；同一次任务的自动重试全程复用同一个键，绝不重新生成。
// 全新任务 = 新的 HTTP 请求 = 新的 RelayInfo = 新键。
//
// 重要限制：火山方舟（Seedance）创建视频任务接口的公开文档未声明支持
// Idempotency-Key 请求头，因此本键不会作为请求头发送，也不提供服务端幂等。
// "同一逻辑任务重试复用同键"只能保证客户端记录一致，不能保证自动重试安全：
// 对于响应结果未知的 POST，最稳妥的默认行为是停止自动重试并标记
// outcome_unknown（见 task_outcome.go 与 controller/relay.go）。
// ---------------------------------------------------------------------------

// EnsureIdempotencyKey 返回本次逻辑任务的本地幂等键；首次调用生成 UUID v4，
// 之后（包括重试）始终返回同一个键。nil 安全。
func (t *TaskRelayInfo) EnsureIdempotencyKey() string {
	if t == nil {
		return ""
	}
	if t.IdempotencyKey == "" {
		t.IdempotencyKey = uuid.NewString()
	}
	return t.IdempotencyKey
}

// EnsureTaskIdempotencyKey 返回本次请求的本地幂等键；首次调用生成，后续复用。
func (info *RelayInfo) EnsureTaskIdempotencyKey() string {
	if info == nil || info.TaskRelayInfo == nil {
		return ""
	}
	return info.TaskRelayInfo.EnsureIdempotencyKey()
}

// EnsureClientRequestID 返回本次创建请求的 X-Client-Request-Id 值（UUID v4）。
// 每个逻辑创建任务生成一次并在重试中复用，仅用于日志串联与问题排查。
// 注意：X-Client-Request-Id 不具备幂等语义，不得将其描述为幂等键。
func (t *TaskRelayInfo) EnsureClientRequestID() string {
	if t == nil {
		return ""
	}
	if t.ClientRequestID == "" {
		t.ClientRequestID = uuid.NewString()
	}
	return t.ClientRequestID
}

// EnsureTaskClientRequestID 返回本次请求的 X-Client-Request-Id；首次调用生成，后续复用。
func (info *RelayInfo) EnsureTaskClientRequestID() string {
	if info == nil || info.TaskRelayInfo == nil {
		return ""
	}
	return info.TaskRelayInfo.EnsureClientRequestID()
}

// ---------------------------------------------------------------------------
// 内容指纹（用于"候选发现"的模糊匹配）
//
// 只对 model + 文本提示词 + 图片数量做同构摘要，不含图片 URL / Base64 /
// metadata 等敏感原始内容；与上游查询接口返回的任务内容（同样只取 model +
// 文本 + 图片数量）按相同规范计算后比较，作为"推测关联"的候选依据。
// 注意：该匹配是模糊的，只能作为候选发现，绝不能作为强一致确认。
// ---------------------------------------------------------------------------

// SubmitContentFingerprint 计算创建请求的内容指纹。
func SubmitContentFingerprint(req TaskSubmitReq) string {
	h := sha256.New()
	fmt.Fprintf(h, "model=%s\n", req.Model)
	fmt.Fprintf(h, "prompt=%s\n", req.Prompt)
	fmt.Fprintf(h, "images=%d\n", len(req.Images))
	return hex.EncodeToString(h.Sum(nil))
}

// ContentFingerprintFromParts 供上游任务项构造同构指纹使用：
// 输入模型名、文本内容列表、图片数量。
func ContentFingerprintFromParts(model string, texts []string, imageCount int) string {
	h := sha256.New()
	fmt.Fprintf(h, "model=%s\n", model)
	for _, t := range texts {
		fmt.Fprintf(h, "prompt=%s\n", t)
	}
	fmt.Fprintf(h, "images=%d\n", imageCount)
	return hex.EncodeToString(h.Sum(nil))
}
