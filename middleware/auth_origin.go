package middleware

import (
	"crypto/subtle"
	"net/http"
	"net/url"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/gin-gonic/gin"
)

// SessionCookieOriginGuard protects cookie-authenticated refresh/logout
// endpoints when secure cookie mode is enabled. In insecure local development
// mode it preserves the legacy behavior and intentionally performs no Origin
// validation. It never adds CORS response headers and must not be installed on
// relay routes.
func SessionCookieOriginGuard() gin.HandlerFunc {
	return func(c *gin.Context) {
		if !common.SessionCookieSecure {
			c.Next()
			return
		}
		origin, ok := requestBrowserOrigin(c.Request)
		if !ok || !isAllowedSessionOrigin(c.Request, origin) {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{
				"success": false,
				"code":    "AUTH_ORIGIN_FORBIDDEN",
				"message": "request origin is not allowed",
			})
			return
		}
		c.Next()
	}
}

func requestBrowserOrigin(request *http.Request) (string, bool) {
	originValues := request.Header.Values("Origin")
	if len(originValues) > 1 {
		return "", false
	}
	if len(originValues) == 1 {
		if strings.Contains(originValues[0], ",") {
			return "", false
		}
		origin, err := common.NormalizeOrigin(originValues[0])
		return origin, err == nil
	}
	refererValues := request.Header.Values("Referer")
	if len(refererValues) != 1 {
		return "", false
	}
	referer, err := url.Parse(strings.TrimSpace(refererValues[0]))
	if err != nil || referer.Scheme == "" || referer.Host == "" || referer.User != nil {
		return "", false
	}
	origin, err := common.NormalizeOrigin(referer.Scheme + "://" + referer.Host)
	return origin, err == nil
}

// isAllowedSessionOrigin is only invoked when SessionCookieSecure is true.
// In that mode TLS is terminated by the reverse proxy (Caddy), so request.TLS
// is nil even for legitimate HTTPS requests, and forwarded scheme headers are
// client-controlled and must never be trusted. Deriving the same-origin
// candidate with http here would let a plaintext origin of the same host
// (for example http://globalaiclient.com) pass the guard, so the request
// origin is always derived as https and any non-https origin is rejected.
func isAllowedSessionOrigin(request *http.Request, origin string) bool {
	if !strings.HasPrefix(origin, "https://") {
		return false
	}
	requestOrigin, err := common.NormalizeOrigin("https://" + request.Host)
	if err == nil && subtle.ConstantTimeCompare([]byte(origin), []byte(requestOrigin)) == 1 {
		return true
	}
	for _, trustedOrigin := range common.SessionCookieTrustedURLs {
		if subtle.ConstantTimeCompare([]byte(origin), []byte(trustedOrigin)) == 1 {
			return true
		}
	}
	return false
}
