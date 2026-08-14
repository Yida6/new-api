package service

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestResetStatusCode(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name             string
		statusCode       int
		statusCodeConfig string
		expectedCode     int
	}{
		{
			name:             "map string value",
			statusCode:       429,
			statusCodeConfig: `{"429":"503"}`,
			expectedCode:     503,
		},
		{
			name:             "map int value",
			statusCode:       429,
			statusCodeConfig: `{"429":503}`,
			expectedCode:     503,
		},
		{
			name:             "skip invalid string value",
			statusCode:       429,
			statusCodeConfig: `{"429":"bad-code"}`,
			expectedCode:     429,
		},
		{
			name:             "skip status code 200",
			statusCode:       200,
			statusCodeConfig: `{"200":503}`,
			expectedCode:     200,
		},
	}

	for _, tc := range testCases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			newAPIError := &types.NewAPIError{
				StatusCode: tc.statusCode,
			}
			ResetStatusCode(newAPIError, tc.statusCodeConfig)
			require.Equal(t, tc.expectedCode, newAPIError.StatusCode)
		})
	}
}

func TestRelayErrorHandlerTruncatesInvalidJSONBodyInLog(t *testing.T) {
	withDebugEnabled(t, false)

	body := strings.Repeat("b", common.LocalLogContentLimit+256)
	var logBuffer bytes.Buffer

	common.LogWriterMu.Lock()
	oldWriter := gin.DefaultErrorWriter
	gin.DefaultErrorWriter = &logBuffer
	common.LogWriterMu.Unlock()
	t.Cleanup(func() {
		common.LogWriterMu.Lock()
		gin.DefaultErrorWriter = oldWriter
		common.LogWriterMu.Unlock()
	})

	resp := &http.Response{
		StatusCode: http.StatusInternalServerError,
		Body:       io.NopCloser(strings.NewReader(body)),
	}

	newAPIError := RelayErrorHandler(context.Background(), resp, false)

	require.NotNil(t, newAPIError)
	require.Equal(t, "bad response status code 500", newAPIError.Error())
	require.Contains(t, logBuffer.String(), "[truncated")
	require.Contains(t, logBuffer.String(), fmt.Sprintf("original_length=%d", len(body)))
	require.NotContains(t, logBuffer.String(), strings.Repeat("b", common.LocalLogContentLimit+1))
}

func TestRelayErrorHandlerKeepsStructuredErrorMessage(t *testing.T) {
	message := strings.Repeat("c", common.LocalLogContentLimit+256)
	body := `{"message":"` + message + `"}`
	resp := &http.Response{
		StatusCode: http.StatusInternalServerError,
		Body:       io.NopCloser(strings.NewReader(body)),
	}

	newAPIError := RelayErrorHandler(context.Background(), resp, false)

	require.NotNil(t, newAPIError)
	require.Equal(t, message, newAPIError.Error())
}

func TestRelayErrorHandlerKeepsOpenAIErrorMessage(t *testing.T) {
	message := strings.Repeat("d", common.LocalLogContentLimit+256)
	body := `{"error":{"message":"` + message + `","type":"server_error","code":"server_error"}}`
	resp := &http.Response{
		StatusCode: http.StatusInternalServerError,
		Body:       io.NopCloser(strings.NewReader(body)),
	}

	newAPIError := RelayErrorHandler(context.Background(), resp, false)

	require.NotNil(t, newAPIError)
	require.Equal(t, message, newAPIError.Error())
}

func TestRelayErrorHandlerKeepsInvalidJSONBodyInDebugLog(t *testing.T) {
	withDebugEnabled(t, true)

	body := strings.Repeat("e", common.LocalLogContentLimit+256)
	var logBuffer bytes.Buffer

	common.LogWriterMu.Lock()
	oldWriter := gin.DefaultErrorWriter
	gin.DefaultErrorWriter = &logBuffer
	common.LogWriterMu.Unlock()
	t.Cleanup(func() {
		common.LogWriterMu.Lock()
		gin.DefaultErrorWriter = oldWriter
		common.LogWriterMu.Unlock()
	})

	resp := &http.Response{
		StatusCode: http.StatusInternalServerError,
		Body:       io.NopCloser(strings.NewReader(body)),
	}

	newAPIError := RelayErrorHandler(context.Background(), resp, false)

	require.NotNil(t, newAPIError)
	require.NotContains(t, logBuffer.String(), "[truncated")
	require.Contains(t, logBuffer.String(), body)
}

func withDebugEnabled(t *testing.T, enabled bool) {
	t.Helper()

	oldDebug := common.DebugEnabled
	common.DebugEnabled = enabled
	t.Cleanup(func() {
		common.DebugEnabled = oldDebug
	})
}

// TestTaskErrorWrapperRedactsSensitiveInfo verifies that task error messages
// always have credential-like values (Ark Endpoint ID / API key / bearer
// token) masked, including non-network upstream errors that previously passed
// through unmasked.
func TestTaskErrorWrapperRedactsSensitiveInfo(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		message  string
		expected string
	}{
		{
			name:     "ark endpoint id in upstream error",
			message:  "model ep-20250101-abc123 does not exist or you do not have access",
			expected: "model ep-*** does not exist or you do not have access",
		},
		{
			name:     "api key in error text",
			message:  "auth failed: Bearer sk-abc123def456ghi789",
			expected: "auth failed: Bearer ***",
		},
		{
			name:     "network error with url (full masking applied)",
			message:  "Post https://ark.example.com/api/v3/chat/completions: dial tcp",
			expected: "Post https://***.com/***/***/***/*** dial tcp",
		},
		{
			name:     "plain business error unchanged",
			message:  "任务超时（60分钟）",
			expected: "任务超时（60分钟）",
		},
		{
			name:     "model name unchanged",
			message:  "doubao-seedance-2-0-260128 unavailable",
			expected: "doubao-seedance-2-0-260128 unavailable",
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := TaskErrorWrapper(fmt.Errorf("%s", tc.message), "upstream_error", http.StatusBadGateway)
			require.Equal(t, tc.expected, err.Message)
			// The original error is preserved for internal logging.
			require.Equal(t, tc.message, err.Error.Error())
		})
	}
}

// TestTaskErrorFromAPIErrorRedactsSensitiveInfo verifies billing-to-task error
// conversion also masks credentials.
func TestTaskErrorFromAPIErrorRedactsSensitiveInfo(t *testing.T) {
	t.Parallel()

	apiErr := types.NewOpenAIError(fmt.Errorf("insufficient quota for ep-20250101-abc123 with key sk-abc123def456ghi789"),
		types.ErrorCodeInsufficientUserQuota, http.StatusBadRequest)
	taskErr := TaskErrorFromAPIError(apiErr)
	require.NotNil(t, taskErr)
	require.Equal(t, "insufficient quota for ep-*** with key sk-***", taskErr.Message)
}

// TestRelayErrorHandlerRedactsCredentialsInOpenAIError verifies the structured
// OpenAI error message relayed to clients has credentials masked (via the
// ToOpenAIError pipeline which applies MaskSensitiveInfo).
func TestRelayErrorHandlerRedactsCredentialsInOpenAIError(t *testing.T) {
	body := `{"error":{"message":"model ep-20250101-abc123 not found, key sk-abc123def456ghi789","type":"invalid_request_error","code":"model_not_found"}}`
	resp := &http.Response{
		StatusCode: http.StatusBadRequest,
		Body:       io.NopCloser(strings.NewReader(body)),
	}

	newAPIError := RelayErrorHandler(context.Background(), resp, false)
	require.NotNil(t, newAPIError)

	oaiErr := newAPIError.ToOpenAIError()
	require.Equal(t, "model ep-*** not found, key sk-***", oaiErr.Message)
}
