package common

import (
	"errors"
	"net"
	"net/url"

	"github.com/QuantumNous/new-api/constant"
)

// TaskSubmitOutcome 表示一次"创建任务"提交尝试的结果分类。
// 分类的目的是回答一个问题：服务端是否已创建任务、以及是否允许安全自动重发。
type TaskSubmitOutcome int

const (
	// TaskSubmitOutcomeUnset 尚未提交（默认零值）。
	TaskSubmitOutcomeUnset TaskSubmitOutcome = iota
	// TaskSubmitOutcomeConfirmedSuccess 明确获得上游 task_id —— 任务已创建。
	TaskSubmitOutcomeConfirmedSuccess
	// TaskSubmitOutcomeConfirmedFailure 明确收到可判定失败的响应（如 4xx/5xx
	// 错误响应体）。默认不再自动重发；由用户决定是否在恢复入口人工重试。
	TaskSubmitOutcomeConfirmedFailure
	// TaskSubmitOutcomeOutcomeUnknown 结果未知：连接中断、读取响应超时、
	// 或收到响应但无法解析出 task_id。服务端是否已创建任务无法确认，
	// 默认禁止自动重发 POST，必须持久化并进入人工确认 / 查询确认流程。
	TaskSubmitOutcomeOutcomeUnknown
	// TaskSubmitOutcomePreSendFailure 明确发生在请求字节发出之前（拨号、
	// DNS、TLS 握手、代理连接等）。服务端不可能已创建任务，可按策略安全重试。
	TaskSubmitOutcomePreSendFailure
)

func (o TaskSubmitOutcome) String() string {
	switch o {
	case TaskSubmitOutcomeConfirmedSuccess:
		return "confirmed_success"
	case TaskSubmitOutcomeConfirmedFailure:
		return "confirmed_failure"
	case TaskSubmitOutcomeOutcomeUnknown:
		return "outcome_unknown"
	case TaskSubmitOutcomePreSendFailure:
		return "pre_send_failure"
	default:
		return "unset"
	}
}

// ClassifySubmitError 将创建请求的 DoRequest 错误分类为可安全重试的
// pre_send_failure 或保守的 outcome_unknown。
//
// 判断依据：
//   - url.Error.Op 为 dial / proxyconnect / GetAddrInfo / LookupHost（含 TLS
//     握手、DNS、连接被拒等，均发生在字节发出之前）→ pre_send_failure；
//   - 其余（读取响应超时、连接被重置、EOF、发送中途断开、上下文取消等，
//     请求字节可能已到达服务端）→ outcome_unknown（保守默认）。
func ClassifySubmitError(err error) TaskSubmitOutcome {
	if err == nil {
		return TaskSubmitOutcomeConfirmedSuccess
	}
	if isPreSendError(err) {
		return TaskSubmitOutcomePreSendFailure
	}
	// 无法确认的其余情况一律保守视为结果未知
	return TaskSubmitOutcomeOutcomeUnknown
}

func isPreSendError(err error) bool {
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return true
	}
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		switch urlErr.Op {
		case "dial", "proxyconnect", "GetAddrInfo", "LookupHost", "LookupTXT":
			return true
		}
		// TLS 握手错误通常被包装在 Op=="dial" 的 url.Error 中，已被上面覆盖；
		// Op=="read"/"Post"/"Get" 表示字节已发出或响应阶段出错 → 结果未知。
	}
	// 裸 net.OpError 兜底：仅当操作明确为 dial（未建立连接）时才视为发送前失败
	var opErr *net.OpError
	if errors.As(err, &opErr) && opErr.Op == "dial" {
		return true
	}
	return false
}

// IsStrictIdempotencyChannel 判断渠道是否属于 Seedance/豆包视频家族。
// 只有这些渠道启用"结果未知即停止自动重试"的严格策略；
// 其他任务渠道保持原有重试行为（不改变无关行为）。
func IsStrictIdempotencyChannel(channelType int) bool {
	switch channelType {
	case constant.ChannelTypeDoubaoVideo, constant.ChannelTypeVolcEngine:
		return true
	default:
		return false
	}
}
