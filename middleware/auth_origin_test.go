package middleware

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type originGuardRequest struct {
	target  string
	origin  string
	referer string
	headers map[string]string
}

func runOriginGuardRequest(t *testing.T, request originGuardRequest) *httptest.ResponseRecorder {
	t.Helper()
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.POST("/api/user/auth/refresh", SessionCookieOriginGuard(), func(c *gin.Context) {
		c.Status(http.StatusNoContent)
	})
	target := request.target
	if target == "" {
		target = "https://panel.example.com/api/user/auth/refresh"
	}
	httpRequest := httptest.NewRequest(http.MethodPost, target, nil)
	httpRequest.Host = httpRequest.URL.Host
	if request.origin != "" {
		httpRequest.Header.Set("Origin", request.origin)
	}
	if request.referer != "" {
		httpRequest.Header.Set("Referer", request.referer)
	}
	for key, value := range request.headers {
		httpRequest.Header.Set(key, value)
	}
	response := httptest.NewRecorder()
	router.ServeHTTP(response, httpRequest)
	return response
}

func assertOriginForbidden(t *testing.T, response *httptest.ResponseRecorder) {
	t.Helper()
	require.Equal(t, http.StatusForbidden, response.Code)
	var body struct {
		Success bool   `json:"success"`
		Code    string `json:"code"`
	}
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &body))
	assert.False(t, body.Success)
	assert.Equal(t, "AUTH_ORIGIN_FORBIDDEN", body.Code)
}

func TestSessionCookieOriginGuard(t *testing.T) {
	previousSecure := common.SessionCookieSecure
	previousTrustedURLs := common.SessionCookieTrustedURLs
	common.SessionCookieSecure = true
	common.SessionCookieTrustedURLs = []string{"https://trusted.example.com"}
	t.Cleanup(func() {
		common.SessionCookieSecure = previousSecure
		common.SessionCookieTrustedURLs = previousTrustedURLs
	})

	tests := []struct {
		name     string
		origin   string
		referer  string
		expected int
	}{
		{name: "same origin", origin: "https://panel.example.com", expected: http.StatusNoContent},
		{name: "trusted exact origin", origin: "https://trusted.example.com", expected: http.StatusNoContent},
		{name: "referer fallback", referer: "https://panel.example.com/profile", expected: http.StatusNoContent},
		{name: "missing both", expected: http.StatusForbidden},
		{name: "null origin", origin: "null", expected: http.StatusForbidden},
		{name: "suffix attack", origin: "https://trusted.example.com.evil.test", expected: http.StatusForbidden},
		{name: "scheme mismatch", origin: "http://panel.example.com", expected: http.StatusForbidden},
		{name: "http referer fallback", referer: "http://panel.example.com/profile", expected: http.StatusForbidden},
		{name: "path in origin", origin: "https://panel.example.com/profile", expected: http.StatusForbidden},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			response := runOriginGuardRequest(t, originGuardRequest{origin: test.origin, referer: test.referer})
			assert.Equal(t, test.expected, response.Code)
			assert.Empty(t, response.Header().Get("Access-Control-Allow-Origin"))
			if test.expected == http.StatusForbidden {
				assertOriginForbidden(t, response)
			}
		})
	}
}

// TestSessionCookieOriginGuardSecureModeRejectsPlaintextOrigins covers the
// production matrix with the real domains: behind the TLS-terminating reverse
// proxy (request.TLS == nil) the guard must reject plaintext origins of the
// same host and any untrusted origin, while the two configured trusted HTTPS
// origins must pass.
func TestSessionCookieOriginGuardSecureModeRejectsPlaintextOrigins(t *testing.T) {
	previousSecure := common.SessionCookieSecure
	previousTrustedURLs := common.SessionCookieTrustedURLs
	common.SessionCookieSecure = true
	common.SessionCookieTrustedURLs = []string{"https://globalaiclient.com", "https://www.globalaiclient.com"}
	t.Cleanup(func() {
		common.SessionCookieSecure = previousSecure
		common.SessionCookieTrustedURLs = previousTrustedURLs
	})

	tests := []struct {
		name     string
		origin   string
		expected int
	}{
		{name: "plaintext apex origin rejected", origin: "http://globalaiclient.com", expected: http.StatusForbidden},
		{name: "plaintext www origin rejected", origin: "http://www.globalaiclient.com", expected: http.StatusForbidden},
		{name: "untrusted https origin rejected", origin: "https://evil.example.com", expected: http.StatusForbidden},
		{name: "trusted apex https origin allowed", origin: "https://globalaiclient.com", expected: http.StatusNoContent},
		{name: "trusted www https origin allowed", origin: "https://www.globalaiclient.com", expected: http.StatusNoContent},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			response := runOriginGuardRequest(t, originGuardRequest{
				target: "http://globalaiclient.com/api/user/auth/refresh",
				origin: test.origin,
			})
			assert.Equal(t, test.expected, response.Code)
			if test.expected == http.StatusForbidden {
				assertOriginForbidden(t, response)
			}
		})
	}
}

// TestSessionCookieOriginGuardSameHostHttpsOriginPassesBehindReverseProxy
// verifies that a same-host https browser origin still passes after the
// reverse proxy terminated TLS (request.TLS == nil), without relying on any
// forwarded header.
func TestSessionCookieOriginGuardSameHostHttpsOriginPassesBehindReverseProxy(t *testing.T) {
	previousSecure := common.SessionCookieSecure
	previousTrustedURLs := common.SessionCookieTrustedURLs
	common.SessionCookieSecure = true
	common.SessionCookieTrustedURLs = nil
	t.Cleanup(func() {
		common.SessionCookieSecure = previousSecure
		common.SessionCookieTrustedURLs = previousTrustedURLs
	})

	response := runOriginGuardRequest(t, originGuardRequest{
		target: "http://panel.example.com/api/user/auth/refresh",
		origin: "https://panel.example.com",
	})
	assert.Equal(t, http.StatusNoContent, response.Code)
	assert.Empty(t, response.Header().Get("Access-Control-Allow-Origin"))
}

// TestSessionCookieOriginGuardDoesNotTrustForwardedProtoFromClient verifies
// that client-forged forwarded scheme headers never upgrade a plaintext
// origin: X-Forwarded-Proto, X-Forwarded-Protocol and Forwarded are all
// ignored and the request is still rejected.
func TestSessionCookieOriginGuardDoesNotTrustForwardedProtoFromClient(t *testing.T) {
	previousSecure := common.SessionCookieSecure
	previousTrustedURLs := common.SessionCookieTrustedURLs
	common.SessionCookieSecure = true
	common.SessionCookieTrustedURLs = nil
	t.Cleanup(func() {
		common.SessionCookieSecure = previousSecure
		common.SessionCookieTrustedURLs = previousTrustedURLs
	})

	for _, header := range []struct{ key, value string }{
		{"X-Forwarded-Proto", "https"},
		{"X-Forwarded-Protocol", "https"},
		{"Forwarded", "proto=https"},
	} {
		t.Run(header.key, func(t *testing.T) {
			response := runOriginGuardRequest(t, originGuardRequest{
				target:  "http://panel.example.com/api/user/auth/refresh",
				origin:  "http://panel.example.com",
				headers: map[string]string{header.key: header.value},
			})
			assertOriginForbidden(t, response)
		})
	}
}

func TestSessionCookieOriginGuardDevelopmentCompatibility(t *testing.T) {
	previousSecure := common.SessionCookieSecure
	previousTrustedURLs := common.SessionCookieTrustedURLs
	t.Cleanup(func() {
		common.SessionCookieSecure = previousSecure
		common.SessionCookieTrustedURLs = previousTrustedURLs
	})
	common.SessionCookieTrustedURLs = nil

	tests := []struct {
		name     string
		secure   bool
		origin   string
		expected int
	}{
		{name: "insecure mode allows mismatched development origins", origin: "http://localhost:3001", expected: http.StatusNoContent},
		{name: "insecure mode allows missing origin", expected: http.StatusNoContent},
		{name: "secure mode rejects mismatched development origins", secure: true, origin: "http://localhost:3001", expected: http.StatusForbidden},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			common.SessionCookieSecure = test.secure

			response := runOriginGuardRequest(t, originGuardRequest{
				target: "http://localhost:3000/api/user/auth/refresh",
				origin: test.origin,
			})
			assert.Equal(t, test.expected, response.Code)
			assert.Empty(t, response.Header().Get("Access-Control-Allow-Origin"))
			if test.expected == http.StatusForbidden {
				assertOriginForbidden(t, response)
			}
		})
	}
}
