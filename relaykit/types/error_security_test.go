package types

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestCountTokenErrorStillRedactsCredentials verifies that count-token errors,
// which previously skipped masking entirely, now at least redact credential-like
// values (Ark Endpoint IDs / API keys / bearer tokens) so they cannot reach
// clients, while keeping the rest of the diagnostic message intact.
func TestCountTokenErrorStillRedactsCredentials(t *testing.T) {
	err := NewError(errors.New("token count failed for ep-20250101-abc123 with key sk-abc123def456ghi789"),
		ErrorCodeCountTokenFailed, ErrOptionWithStatusCode(500))

	// ToOpenAIError / ToClaudeError relay the message to clients.
	require.Equal(t, "token count failed for ep-*** with key sk-***", err.ToOpenAIError().Message)
	require.Equal(t, "token count failed for ep-*** with key sk-***", err.ToClaudeError().Message)
	require.Equal(t, "token count failed for ep-*** with key sk-***", err.MaskSensitiveError())
}

// TestNonCountTokenErrorFullMasking verifies regular errors keep the full
// masking pipeline (URL/domain/IP + credentials).
func TestNonCountTokenErrorFullMasking(t *testing.T) {
	err := NewError(errors.New("dial https://ark.example.com/v1/chat ep-20250101-abc123"),
		ErrorCodeDoRequestFailed, ErrOptionWithStatusCode(502))
	msg := err.ToOpenAIError().Message
	require.NotContains(t, msg, "ep-20250101-abc123")
	require.NotContains(t, msg, "ark.example.com")
}
