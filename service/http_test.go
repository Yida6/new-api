package service

import (
	"net/http/httptest"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestShouldCopyUpstreamHeaderAllowlist(t *testing.T) {
	t.Parallel()

	allowed := []string{
		"Content-Type",
		"content-disposition",
		"Retry-After",
		"X-Codex-Turn-State",
	}
	for _, name := range allowed {
		require.True(t, ShouldCopyUpstreamHeader(nil, name, []string{"value"}), name)
	}

	blocked := []string{
		"Server",
		"Via",
		"CF-Ray",
		"X-Powered-By",
		"X-Upstream-Host",
		"Location",
		"Set-Cookie",
		"Transfer-Encoding",
	}
	for _, name := range blocked {
		require.False(t, ShouldCopyUpstreamHeader(nil, name, []string{"value"}), name)
	}
}

func TestShouldCopyUpstreamHeaderCapturesUpstreamRequestID(t *testing.T) {
	t.Parallel()

	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	require.False(t, ShouldCopyUpstreamHeader(c, common.RequestIdKey, []string{"upstream-request-id"}))

	value, exists := c.Get(common.UpstreamRequestIdKey)
	require.True(t, exists)
	require.Equal(t, "upstream-request-id", value)
}
