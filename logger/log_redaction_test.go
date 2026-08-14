package logger

import (
	"bytes"
	"context"
	"testing"

	"github.com/QuantumNous/new-api/common"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// captureErrorWriter swaps gin.DefaultErrorWriter for the duration of a test.
func captureErrorWriter(t *testing.T) *bytes.Buffer {
	t.Helper()
	buf := &bytes.Buffer{}
	common.LogWriterMu.Lock()
	old := gin.DefaultErrorWriter
	gin.DefaultErrorWriter = buf
	common.LogWriterMu.Unlock()
	t.Cleanup(func() {
		common.LogWriterMu.Lock()
		gin.DefaultErrorWriter = old
		common.LogWriterMu.Unlock()
	})
	return buf
}

// TestLogErrorRedactsCredentials verifies the server-side log outlet never
// writes unmasked Ark Endpoint IDs / API keys, even when the logged text is an
// upstream response/error body that echoes them.
func TestLogErrorRedactsCredentials(t *testing.T) {
	buf := captureErrorWriter(t)

	LogError(context.Background(),
		"upstream body: model ep-20250101-abc123 not found, key sk-abc123def456ghi789, Authorization: Bearer abcdef1234567890")

	out := buf.String()
	require.NotContains(t, out, "ep-20250101-abc123")
	require.NotContains(t, out, "sk-abc123def456ghi789")
	require.NotContains(t, out, "Bearer abcdef1234567890")
	// Non-credential diagnostic content is preserved.
	require.Contains(t, out, "upstream body")
	require.Contains(t, out, "ep-***")
	require.Contains(t, out, "sk-***")
}

// TestLogWarnRedactsCredentials covers the WARN outlet (DefaultErrorWriter).
func TestLogWarnRedactsCredentials(t *testing.T) {
	buf := captureErrorWriter(t)

	LogWarn(context.Background(), "task ep-20250101-abc123 failed with key sk-abc123def456ghi789")

	out := buf.String()
	require.NotContains(t, out, "ep-20250101-abc123")
	require.NotContains(t, out, "sk-abc123def456ghi789")
	require.Contains(t, out, "ep-***")
}

// TestLogErrorKeepsPlainMessage ensures ordinary messages pass through unchanged.
func TestLogErrorKeepsPlainMessage(t *testing.T) {
	buf := captureErrorWriter(t)

	LogError(context.Background(), "任务超时（60分钟），quota=15754")

	out := buf.String()
	require.Contains(t, out, "任务超时（60分钟），quota=15754")
	require.NotContains(t, out, "ep-***") // nothing to redact, no placeholder noise
}
