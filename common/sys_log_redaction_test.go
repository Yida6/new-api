package common

import (
	"bytes"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// TestSysLogRedactsCredentials verifies the system-log outlet (SysLog/SysError)
// never writes unmasked Ark Endpoint IDs / API keys.
func TestSysLogRedactsCredentials(t *testing.T) {
	buf := &bytes.Buffer{}
	LogWriterMu.Lock()
	old := gin.DefaultWriter
	gin.DefaultWriter = buf
	LogWriterMu.Unlock()
	t.Cleanup(func() {
		LogWriterMu.Lock()
		gin.DefaultWriter = old
		LogWriterMu.Unlock()
	})

	SysLog("task ep-20250101-abc123 failed with key sk-abc123def456ghi789, token=12345678-1234-1234-1234-123456789012")

	out := buf.String()
	require.NotContains(t, out, "ep-20250101-abc123")
	require.NotContains(t, out, "sk-abc123def456ghi789")
	require.NotContains(t, out, "12345678-1234-1234-1234-123456789012")
	require.Contains(t, out, "ep-***")
	require.Contains(t, out, "sk-***")
}

// TestSysErrorRedactsCredentials covers the error outlet.
func TestSysErrorRedactsCredentials(t *testing.T) {
	buf := &bytes.Buffer{}
	LogWriterMu.Lock()
	old := gin.DefaultErrorWriter
	gin.DefaultErrorWriter = buf
	LogWriterMu.Unlock()
	t.Cleanup(func() {
		LogWriterMu.Lock()
		gin.DefaultErrorWriter = old
		LogWriterMu.Unlock()
	})

	SysError("relay error: model EP-20250101-ABC123 unavailable, Authorization: Basic dXNlcjpwYXNz")

	out := buf.String()
	require.NotContains(t, out, "EP-20250101-ABC123")
	require.NotContains(t, out, "dXNlcjpwYXNz")
	require.Contains(t, out, "ep-***")
}

// TestSysLogKeepsPlainMessage ensures ordinary messages pass through unchanged.
func TestSysLogKeepsPlainMessage(t *testing.T) {
	buf := &bytes.Buffer{}
	LogWriterMu.Lock()
	old := gin.DefaultWriter
	gin.DefaultWriter = buf
	LogWriterMu.Unlock()
	t.Cleanup(func() {
		LogWriterMu.Lock()
		gin.DefaultWriter = old
		LogWriterMu.Unlock()
	})

	SysLog("channels synced from database")
	require.Contains(t, buf.String(), "channels synced from database")
}
