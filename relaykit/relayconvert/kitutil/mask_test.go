package kitutil

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestRedactCredentialsMasksArkEndpointID verifies that Volcano Engine Ark
// Endpoint IDs (ep-xxxx, which may leak through upstream model names) are
// masked in client-facing text.
func TestRedactCredentialsMasksArkEndpointID(t *testing.T) {
	cases := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name:     "plain endpoint id",
			input:    "model ep-20250101-abc123 not found",
			expected: "model ep-*** not found",
		},
		{
			name:     "endpoint inside json payload",
			input:    `{"model":"ep-20250101-abc123","status":"running"}`,
			expected: `{"model":"ep-***","status":"running"}`,
		},
		{
			name:     "endpoint without suffix",
			input:    "ep-20250101",
			expected: "ep-***",
		},
		{
			name:     "endpoint with multi-part suffix",
			input:    "ep-20250101-abc-123-xyz",
			expected: "ep-***",
		},
		{
			name:     "endpoint embedded in a longer word boundary",
			input:    `upstream_model_name = "ep-20250101-abc123"`,
			expected: `upstream_model_name = "ep-***"`,
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.expected, RedactCredentials(tc.input))
		})
	}
}

// TestRedactCredentialsMasksAPIKeys verifies OpenAI-style keys, bearer tokens
// and explicit api_key values are masked.
func TestRedactCredentialsMasksAPIKeys(t *testing.T) {
	cases := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name:     "openai style key",
			input:    "Incorrect API key provided: sk-proj-1234567890abcdef",
			expected: "Incorrect API key provided: sk-***",
		},
		{
			name:     "bearer token",
			input:    "Authorization: Bearer 12345678-1234-1234-1234-123456789012",
			expected: "Authorization: ***",
		},
		{
			name:     "bearer with sk key",
			input:    "Authorization: Bearer sk-abc123def456ghi789",
			expected: "Authorization: ***",
		},
		{
			name:     "explicit api_key field",
			input:    `api_key:AIzaSyAAAaUooTUni8AdaOkSRMda30n_Q4vrV70`,
			expected: `api_key:***`,
		},
		{
			name:     "key followed by punctuation",
			input:    "key is sk-abc123def456ghi789.",
			expected: "key is sk-***.",
		},
		{
			name:     "authorization without bearer keyword",
			input:    `Authorization: Basic dXNlcjpwYXNz`,
			expected: `Authorization: ***`,
		},
		{
			name:     "authorization raw token",
			input:    `authorization: abc123def456ghi789`,
			expected: `Authorization: ***`,
		},
		{
			name:     "apiKey pair",
			input:    `apiKey=abcdef1234567890`,
			expected: `apiKey: ***`,
		},
		{
			name:     "access_token pair",
			input:    `access_token=abcdef1234567890`,
			expected: `access_token: ***`,
		},
		{
			name:     "secret pair",
			input:    `secret=abcdef1234567890`,
			expected: `secret: ***`,
		},
		{
			name:     "token pair with uuid-style value",
			input:    `token=12345678-1234-1234-1234-123456789012`,
			expected: `token: ***`,
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.expected, RedactCredentials(tc.input))
		})
	}
}

// TestRedactCredentialsMasksEndpointVariants verifies Ark Endpoint ID variants
// (date-style, long non-date form, uppercase, underscore) are masked.
func TestRedactCredentialsMasksEndpointVariants(t *testing.T) {
	cases := []struct {
		name     string
		input    string
		expected string
	}{
		{name: "date style", input: "ep-20250101-abc123", expected: "ep-***"},
		{name: "long non-date form", input: "ep-abc123def456", expected: "ep-***"},
		{name: "long mixed form", input: "ep-2025abc123def456", expected: "ep-***"},
		{name: "inside json", input: `"model":"ep-abc123def456"`, expected: `"model":"ep-***"`},
		{name: "uppercase prefix", input: "EP-20250101-ABC123", expected: "ep-***"},
		{name: "uppercase long form", input: "EP-ABC123DEF456", expected: "ep-***"},
		{name: "underscore in suffix", input: "ep-abc_def_123456", expected: "ep-***"},
		{name: "uppercase underscore", input: "EP-ABC_DEF_123456", expected: "ep-***"},
		{name: "uppercase inside json", input: `{"model":"EP-20250101-ABC123"}`, expected: `{"model":"ep-***"}`},
		{name: "underscore inside json", input: `"model":"ep-abc_def_123456"`, expected: `"model":"ep-***"`},
		// False-positive guards.
		{name: "short ep token", input: "ep-abc", expected: "ep-abc"},
		{name: "word starting with episode", input: "episode-1234567890", expected: "episode-1234567890"},
		{name: "uppercase episode word", input: "EPISODE-1234567890", expected: "EPISODE-1234567890"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.expected, RedactCredentials(tc.input))
		})
	}
}

// TestRedactCredentialsMasksQuotedJSON verifies quoted-JSON credential pairs
// ({"apiKey":"...","token":"...","authorization":"..."}) are masked without
// corrupting the JSON structure — the case previously missed by unquoted-only
// patterns.
func TestRedactCredentialsMasksQuotedJSON(t *testing.T) {
	cases := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name:     "apiKey quoted",
			input:    `{"apiKey":"abcdef1234567890"}`,
			expected: `{"apiKey": "***"}`,
		},
		{
			name:     "token quoted with uuid value",
			input:    `{"token":"12345678-1234-1234-1234-123456789012"}`,
			expected: `{"token": "***"}`,
		},
		{
			name:     "access_token quoted",
			input:    `{"access_token":"abcdef1234567890"}`,
			expected: `{"access_token": "***"}`,
		},
		{
			name:     "secret quoted",
			input:    `{"secret":"abcdef1234567890"}`,
			expected: `{"secret": "***"}`,
		},
		{
			name:     "x-api-key quoted",
			input:    `{"x-api-key":"abcdef1234567890"}`,
			expected: `{"x-api-key": "***"}`,
		},
		{
			name:     "authorization quoted raw token",
			input:    `{"authorization": "abcdef1234567890"}`,
			expected: `{"authorization": "***"}`,
		},
		{
			name:     "authorization quoted basic",
			input:    `{"authorization": "Basic dXNlcjpwYXNz"}`,
			expected: `{"authorization": "***"}`,
		},
		{
			name:     "authorization quoted bearer sk",
			input:    `{"authorization": "Bearer sk-abc123def456ghi789"}`,
			expected: `{"authorization": "***"}`,
		},
		{
			name:     "sk key quoted stays masked",
			input:    `{"apiKey":"sk-abc123def456ghi789"}`,
			expected: `{"apiKey":"sk-***"}`,
		},
		{
			name:     "uppercase sk quoted",
			input:    `SK-PROJ-1234567890ABCDEF`,
			expected: `sk-***`,
		},
		{
			name:     "mixed json payload",
			input:    `{"model":"ep-20250101-abc123","apiKey":"abcdef1234567890","authorization":"Bearer 12345678-1234-1234-1234-123456789012","status":"ok"}`,
			expected: `{"model":"ep-***","apiKey": "***","authorization": "***","status":"ok"}`,
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.expected, RedactCredentials(tc.input))
		})
	}
}

// TestRedactCredentialsMasksQuotedJSONShortValuesSafe verifies short values and
// ordinary fields inside quoted JSON are left untouched (no false positives).
func TestRedactCredentialsMasksQuotedJSONShortValuesSafe(t *testing.T) {
	cases := []struct {
		name  string
		input string
	}{
		{name: "short token value", input: `{"token":"abc"}`},
		{name: "token count field", input: `{"prompt_tokens":"128","completion_tokens":"64"}`},
		{name: "model field short", input: `{"model":"gpt-4o-mini"}`},
		{name: "already masked idempotent", input: `{"apiKey": "***","authorization": "***","token": "***"}`},
		{name: "task id field", input: `{"task_id":"task_1234567890abcdef"}`},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.input, RedactCredentials(tc.input))
		})
	}
}

// TestRedactCredentialsLeavesNormalText verifies that ordinary model names,
// URLs, Chinese text and short/ambiguous strings are left untouched, so the
// masking does not affect legitimate business content.
func TestRedactCredentialsLeavesNormalText(t *testing.T) {
	cases := []struct {
		name  string
		input string
	}{
		{name: "ark model name", input: "doubao-seedance-2-0-260128"},
		{name: "openai model name", input: "gpt-4o-mini"},
		{name: "chinese text", input: "任务超时（60分钟）"},
		{name: "empty string", input: ""},
		{name: "url without credentials", input: "https://example.com/v1/videos/abc"},
		{name: "short ep token without digit requirement", input: "ep-123"},
		{name: "ep token without digits", input: "ep-abcdefgh"},
		{name: "short sk token", input: "sk-x"},
		{name: "uuid-like task id", input: "task_1234567890abcdef"},
		{name: "plain numbers", input: "quota=15754"},
		{name: "token count not masked", input: "prompt_tokens=128"},
		{name: "short token value not masked", input: "token=abc"},
		{name: "bearer placeholder idempotent", input: "Bearer ***"},
		{name: "masked pairs idempotent", input: "api_key:*** token: ***"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.input, RedactCredentials(tc.input))
		})
	}
}

// TestMaskSensitiveInfoAlsoMasksCredentials verifies the full MaskSensitiveInfo
// pipeline now also covers credentials (used by the OpenAI/Claude error relay).
func TestMaskSensitiveInfoAlsoMasksCredentials(t *testing.T) {
	input := `The model ep-20250101-abc123 does not exist or you do not have access to it. key: sk-abc123def456`
	require.Equal(t, `The model ep-*** does not exist or you do not have access to it. key: sk-***`, MaskSensitiveInfo(input))
}
