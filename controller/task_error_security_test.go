package controller

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	taskdto "github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/model"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// TestRespondTaskErrorRedactsSensitiveInfo verifies the unified task error
// responder masks credential-like values (Ark Endpoint ID / API key) in the
// message sent to clients, and keeps the 429 rewrite behavior.
func TestRespondTaskErrorRedactsSensitiveInfo(t *testing.T) {
	gin.SetMode(gin.TestMode)

	cases := []struct {
		name    string
		taskErr *taskdto.TaskError
		expect  string
	}{
		{
			name:    "ark endpoint id masked",
			taskErr: &taskdto.TaskError{Code: "upstream_error", Message: "model ep-20250101-abc123 not found", StatusCode: http.StatusBadRequest},
			expect:  "model ep-*** not found",
		},
		{
			name:    "api key masked",
			taskErr: &taskdto.TaskError{Code: "auth_error", Message: "auth failed: Bearer sk-abc123def456ghi789", StatusCode: http.StatusUnauthorized},
			expect:  "auth failed: Bearer ***",
		},
		{
			name:    "plain message unchanged",
			taskErr: &taskdto.TaskError{Code: "invalid_request", Message: "video_id is required", StatusCode: http.StatusBadRequest},
			expect:  "video_id is required",
		},
		{
			name:    "429 rewritten to load hint",
			taskErr: &taskdto.TaskError{Code: "rate_limit", Message: "upstream ep-20250101-abc123 overloaded", StatusCode: http.StatusTooManyRequests},
			expect:  "当前分组上游负载已饱和，请稍后再试",
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/video/generations", strings.NewReader(`{}`))

			respondTaskError(c, tc.taskErr)

			require.Equal(t, tc.taskErr.StatusCode, w.Code)
			require.Contains(t, w.Body.String(), tc.expect)
			require.NotContains(t, w.Body.String(), "ep-20250101-abc123")
			require.NotContains(t, w.Body.String(), "sk-abc123def456ghi789")
		})
	}
}

// TestAssociateTaskRecoveryErrorRedacted verifies that a failing upstream
// verification surfaces to the user without the channel URL (which may embed
// sensitive endpoint details).
func TestAssociateTaskRecoveryErrorRedacted(t *testing.T) {
	initRecoveryControllerTestDB(t)
	// A server we immediately close: requests to it fail with dial errors that
	// embed the URL (http://127.0.0.1:<port>).
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	baseURL := strings.TrimPrefix(srv.URL, "http://")
	srv.Close()

	require.NoError(t, model.DB.AutoMigrate(&model.Channel{}))
	ch := &model.Channel{Type: 54, Key: "k", Status: 1, Name: "test", BaseURL: &baseURL}
	require.NoError(t, model.DB.Create(ch).Error)

	rec := &model.TaskSubmitRecovery{
		UserId: 1, Platform: "54", Model: "m",
		ChannelId: ch.Id, ChannelType: 54,
		Outcome: "outcome_unknown", Status: model.TaskRecoveryStatusUnknown,
	}
	require.NoError(t, rec.Insert())

	c, w := newRecoveryCtrlCtx(t, 1, "/api/user/task_recovery/1/associate", `{"upstream_task_id":"cgt-x"}`)
	AssociateTaskRecovery(c)

	require.Equal(t, http.StatusBadRequest, w.Code)
	body := w.Body.String()
	require.Contains(t, body, "无法通过官方查询接口确认")
	// The raw upstream URL/port must not leak to the user.
	require.NotContains(t, body, srv.URL)
	require.NotContains(t, body, "127.0.0.1")
	require.NotContains(t, body, "Bearer")
	require.NotContains(t, body, "sk-")
}
