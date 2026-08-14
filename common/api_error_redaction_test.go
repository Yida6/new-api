package common

import (
	"errors"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// TestApiErrorRedactsCredentials verifies the generic API error outlet never
// echoes unmasked credentials that may ride along DB/upstream error text.
func TestApiErrorRedactsCredentials(t *testing.T) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("GET", "/api/log", nil)

	ApiError(c, errors.New("query failed: model ep-20250101-abc123 not found, key sk-abc123def456ghi789"))

	body := w.Body.String()
	require.NotContains(t, body, "ep-20250101-abc123")
	require.NotContains(t, body, "sk-abc123def456ghi789")
	require.Contains(t, body, "ep-***")
	require.Contains(t, body, "sk-***")
	// 普通错误文本保留
	require.Contains(t, body, "query failed")
}

// TestApiErrorMsgRedactsCredentials verifies the string-based outlet masks
// credentials too, while ordinary messages pass through unchanged.
func TestApiErrorMsgRedactsCredentials(t *testing.T) {
	gin.SetMode(gin.TestMode)

	t.Run("credentials masked", func(t *testing.T) {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest("GET", "/api/log", nil)
		ApiErrorMsg(c, "upstream Authorization: Bearer abc123def456ghi789, EP-20250101-ABC123")
		body := w.Body.String()
		require.NotContains(t, body, "Bearer abc123def456ghi789")
		require.NotContains(t, body, "EP-20250101-ABC123")
	})

	t.Run("plain message unchanged", func(t *testing.T) {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest("GET", "/api/log", nil)
		ApiErrorMsg(c, "查询日志失败")
		require.Contains(t, w.Body.String(), "查询日志失败")
	})
}

// TestApiErrorMsgNoPlaceholderNoise ensures plain business messages produce no
// masked-placeholder artifacts.
func TestApiErrorMsgNoPlaceholderNoise(t *testing.T) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("GET", "/api/log", nil)
	ApiErrorMsg(c, "invalid request body")
	require.Contains(t, w.Body.String(), "invalid request body")
	require.False(t, strings.Contains(w.Body.String(), "***"))
}
