package common

import (
	"context"
	"errors"
	"net"
	"net/url"
	"testing"

	"github.com/stretchr/testify/assert"
)

// 读取响应超时（请求字节已发出，结果未知）→ outcome_unknown。
func TestClassifySubmitErrorReadTimeoutIsUnknown(t *testing.T) {
	err := &url.Error{
		Op:  "read",
		URL: "https://ark.cn-beijing.volces.com/api/v3/contents/generations/tasks",
		Err: context.DeadlineExceeded,
	}
	assert.Equal(t, TaskSubmitOutcomeOutcomeUnknown, ClassifySubmitError(err))

	err2 := errors.New(`Post "https://ark...": context deadline exceeded (Client.Timeout exceeded while awaiting headers)`)
	assert.Equal(t, TaskSubmitOutcomeOutcomeUnknown, ClassifySubmitError(err2))
}

// 连接被重置（请求可能已到达服务端）→ outcome_unknown（保守）。
func TestClassifySubmitErrorConnResetIsUnknown(t *testing.T) {
	err := &url.Error{
		Op:  "read",
		URL: "https://ark.cn-beijing.volces.com/api/v3/contents/generations/tasks",
		Err: errors.New("connection reset by peer"),
	}
	assert.Equal(t, TaskSubmitOutcomeOutcomeUnknown, ClassifySubmitError(err))
}

// 拨号失败（请求字节未发出）→ pre_send_failure（可安全重试）。
func TestClassifySubmitErrorDialFailureIsPreSend(t *testing.T) {
	err := &url.Error{
		Op:  "dial",
		URL: "https://ark.cn-beijing.volces.com",
		Err: errors.New("dial tcp 127.0.0.1:443: connect: connection refused"),
	}
	assert.Equal(t, TaskSubmitOutcomePreSendFailure, ClassifySubmitError(err))
}

// DNS 解析失败（请求未发出）→ pre_send_failure。
func TestClassifySubmitErrorDNSFailureIsPreSend(t *testing.T) {
	err := &url.Error{
		Op:  "dial",
		URL: "https://ark.cn-beijing.volces.com",
		Err: &net.DNSError{Err: "no such host", Name: "ark.cn-beijing.volces.com"},
	}
	assert.Equal(t, TaskSubmitOutcomePreSendFailure, ClassifySubmitError(err))

	// 裸 DNS 错误
	assert.Equal(t, TaskSubmitOutcomePreSendFailure, ClassifySubmitError(
		&net.DNSError{Err: "no such host", Name: "example.com"}))
}

// 未知/其他错误一律保守视为结果未知。
func TestClassifySubmitErrorGenericIsUnknown(t *testing.T) {
	assert.Equal(t, TaskSubmitOutcomeOutcomeUnknown, ClassifySubmitError(errors.New("some random error")))
}

// nil → 视为成功（无错误）。
func TestClassifySubmitErrorNilIsSuccess(t *testing.T) {
	assert.Equal(t, TaskSubmitOutcomeConfirmedSuccess, ClassifySubmitError(nil))
}

// 严格幂等策略仅适用于 Seedance/豆包渠道。
func TestIsStrictIdempotencyChannel(t *testing.T) {
	assert.True(t, IsStrictIdempotencyChannel(54), "ChannelTypeDoubaoVideo 应启用严格策略")
	assert.True(t, IsStrictIdempotencyChannel(45), "ChannelTypeVolcEngine 应启用严格策略")
	assert.False(t, IsStrictIdempotencyChannel(1), "OpenAI 渠道不启用严格策略")
	assert.False(t, IsStrictIdempotencyChannel(0), "未知渠道不启用严格策略")
}
