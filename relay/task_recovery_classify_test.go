package relay

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"testing"

	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestClassifyErrorKindTimeoutThroughHiddenChain 复刻真实链路：
// doRequest（relay/channel/api_request.go:535）用 ErrOptionWithHideErrMsg
// 隐藏对外文案，原始超时错误在 Unwrap 链中 —— 分类必须识别为 timeout。
func TestClassifyErrorKindTimeoutThroughHiddenChain(t *testing.T) {
	baseErr := &url.Error{Op: "Post", URL: "http://127.0.0.1:53508/api/v3/contents/generations/tasks", Err: context.DeadlineExceeded}
	apiErr := types.NewError(baseErr, types.ErrorCodeDoRequestFailed, types.ErrOptionWithHideErrMsg("upstream error: do request failed"))
	wrapped := fmt.Errorf("do request failed: %w", apiErr)

	// 前置条件：顶层 Error() 只返回通用隐藏文案（复现修复前 classifyErrorKind 看到的内容）
	require.Equal(t, "do request failed: upstream error: do request failed", wrapped.Error(), "前置条件不符：顶层错误文本应只含隐藏文案")

	require.Equal(t, "timeout", classifyErrorKind(wrapped))
}

// TestClassifyErrorKindSentinelAndNetError 验证标准错误机制优先于文本匹配。
func TestClassifyErrorKindSentinelAndNetError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want string
	}{
		{"nil -> unknown", nil, "unknown"},
		{"context.DeadlineExceeded -> timeout", context.DeadlineExceeded, "timeout"},
		{"net.OpError(timeout) -> timeout", &net.OpError{Op: "read", Err: context.DeadlineExceeded}, "timeout"},
		{"url.Error(timeout) -> timeout", &url.Error{Op: "Post", URL: "http://127.0.0.1/x", Err: context.DeadlineExceeded}, "timeout"},
		// 非超时 net.Error：Timeout()=false，走文本兜底 → conn_reset
		{"net.OpError(connection reset) -> conn_reset", &net.OpError{Op: "read", Err: errors.New("connection reset by peer")}, "conn_reset"},
		{"broken pipe -> conn_reset", errors.New("write: broken pipe"), "conn_reset"},
		{"unexpected EOF -> conn_reset", errors.New("unexpected EOF"), "conn_reset"},
		{"unmarshal -> parse", errors.New("json: cannot unmarshal object into string"), "parse"},
		{"parse -> parse", errors.New("strconv.ParseInt: parsing failed"), "parse"},
		{"plain error -> other", errors.New("some random error"), "other"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, classifyErrorKind(tt.err), "classifyErrorKind(%v)", tt.err)
		})
	}
}

// TestClassifyErrorKindConnResetThroughHiddenChain 隐藏文案包装的连接重置错误
// 也应通过 Unwrap 链识别。
func TestClassifyErrorKindConnResetThroughHiddenChain(t *testing.T) {
	baseErr := errors.New("read: connection reset by peer")
	apiErr := types.NewError(baseErr, types.ErrorCodeDoRequestFailed, types.ErrOptionWithHideErrMsg("upstream error: do request failed"))
	wrapped := fmt.Errorf("do request failed: %w", apiErr)

	require.Equal(t, "conn_reset", classifyErrorKind(wrapped))
}
