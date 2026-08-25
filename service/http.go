package service

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/logger"

	"github.com/gin-gonic/gin"
)

func CloseResponseBodyGracefully(httpResponse *http.Response) {
	if httpResponse == nil || httpResponse.Body == nil {
		return
	}
	err := httpResponse.Body.Close()
	if err != nil {
		common.SysError("failed to close response body: " + err.Error())
	}
}

// upstreamResponseHeaderAllowlist contains the upstream response headers that
// are safe and useful to expose to clients. Keep this as an allowlist: upstream
// relays and CDNs can add arbitrary identifying headers, so a denylist will
// inevitably miss new or vendor-specific names.
var upstreamResponseHeaderAllowlist = map[string]struct{}{
	"accept-ranges":        {},
	"cache-control":        {},
	"content-disposition":  {},
	"content-encoding":     {},
	"content-language":     {},
	"content-range":        {},
	"content-type":         {},
	"etag":                 {},
	"expires":              {},
	"last-modified":        {},
	"retry-after":          {},
	"x-codex-turn-state":   {},
	"x-reasoning-included": {},
}

// ShouldCopyUpstreamHeader checks whether a given upstream response header
// should be copied to the client response. Content-Length is managed locally,
// and X-Oneapi-Request-Id is captured only for server-side logging so the local
// request ID remains authoritative. Every other header must be explicitly
// allowlisted above to prevent upstream relay/CDN fingerprints from leaking.
func ShouldCopyUpstreamHeader(c *gin.Context, k string, v []string) bool {
	if strings.EqualFold(k, "Content-Length") {
		return false
	}
	if strings.EqualFold(k, common.RequestIdKey) {
		if c != nil && len(v) > 0 {
			c.Set(common.UpstreamRequestIdKey, v[0])
		}
		return false
	}
	_, ok := upstreamResponseHeaderAllowlist[strings.ToLower(strings.TrimSpace(k))]
	return ok
}

func IOCopyBytesGracefully(c *gin.Context, src *http.Response, data []byte) {
	if c.Writer == nil {
		return
	}

	body := io.NopCloser(bytes.NewBuffer(data))

	// We shouldn't set the header before we parse the response body, because the parse part may fail.
	// And then we will have to send an error response, but in this case, the header has already been set.
	// So the httpClient will be confused by the response.
	// For example, Postman will report error, and we cannot check the response at all.
	if src != nil {
		for k, v := range src.Header {
			if !ShouldCopyUpstreamHeader(c, k, v) {
				continue
			}
			c.Writer.Header().Set(k, v[0])
		}
	}

	// set Content-Length header manually BEFORE calling WriteHeader
	c.Writer.Header().Set("Content-Length", fmt.Sprintf("%d", len(data)))

	// Write header with status code (this sends the headers)
	if src != nil {
		c.Writer.WriteHeader(src.StatusCode)
	} else {
		c.Writer.WriteHeader(http.StatusOK)
	}

	_, err := io.Copy(c.Writer, body)
	if err != nil {
		logger.LogError(c, fmt.Sprintf("failed to copy response body: %s", err.Error()))
	}
	c.Writer.Flush()
}
