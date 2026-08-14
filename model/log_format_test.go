package model

import (
	"encoding/base64"
	"testing"

	"github.com/QuantumNous/new-api/common"

	"github.com/stretchr/testify/require"
)

// TestFormatUserLogsStripsQuotaSaturation verifies the admin-only quota
// saturation marker (nested under other.admin_info) is removed for non-admin
// log views, since formatUserLogs strips the whole admin_info object.
func TestFormatUserLogsStripsQuotaSaturation(t *testing.T) {
	other := common.MapToJsonStr(map[string]interface{}{
		"model_price": 0.004,
		"admin_info": map[string]interface{}{
			"quota_saturation": map[string]interface{}{
				"op":      "QuotaFromDecimal",
				"kind":    "overflow",
				"clamped": common.MaxQuota,
			},
		},
	})
	logs := []*Log{{Other: other}}

	formatUserLogs(logs, 0)

	parsed, err := common.StrToMap(logs[0].Other)
	require.NoError(t, err)
	_, hasAdminInfo := parsed["admin_info"]
	require.False(t, hasAdminInfo, "admin_info (and nested quota_saturation) must be stripped for non-admin views")
	// Non-admin billing fields remain visible.
	require.Contains(t, parsed, "model_price")
}

// TestFormatUserLogsStripsUpstreamModelName verifies that the upstream model
// name (which may carry the Ark Endpoint ID) is removed from user-facing log
// "other" payloads.
func TestFormatUserLogsStripsUpstreamModelName(t *testing.T) {
	other := common.MapToJsonStr(map[string]interface{}{
		"is_task":              true,
		"is_model_mapped":      true,
		"upstream_model_name":  "ep-20250101-abc123",
		"model_price":          0.004,
	})
	logs := []*Log{{Other: other}}

	formatUserLogs(logs, 0)

	parsed, err := common.StrToMap(logs[0].Other)
	require.NoError(t, err)
	_, hasUpstream := parsed["upstream_model_name"]
	require.False(t, hasUpstream, "upstream_model_name (Ark Endpoint ID) must be stripped for user log views")
	// Harmless markers and billing fields remain visible.
	require.Equal(t, true, parsed["is_model_mapped"])
	require.Contains(t, parsed, "model_price")
}

// TestFormatUserLogsRedactsNestedCredentials verifies that credential-like
// values (Ark Endpoint IDs / API keys) inside remaining fields — upstream error
// text in stream_status and param override audit values — are masked for
// non-admin views, while opaque frontend-decoded fields stay untouched.
func TestFormatUserLogsRedactsNestedCredentials(t *testing.T) {
	other := common.MapToJsonStr(map[string]interface{}{
		"stream_status": map[string]interface{}{
			"status":     "error",
			"end_error":  "model ep-20250101-abc123 does not exist",
			"error_count": 1,
			"errors":     []interface{}{"auth failed: Bearer sk-abc123def456ghi789"},
		},
		"po": []interface{}{
			"set_header Authorization = Bearer sk-secretkey12345678",
		},
		"expr_b64": "ZXhwcl8xX3JhdGVfbGltaXQ=", // opaque base64; contains no credentials, so decode→redact→re-encode keeps it byte-identical
	})
	logs := []*Log{{Other: other}}

	formatUserLogs(logs, 0)

	parsed, err := common.StrToMap(logs[0].Other)
	require.NoError(t, err)

	stream, ok := parsed["stream_status"].(map[string]interface{})
	require.True(t, ok)
	require.Equal(t, "model ep-*** does not exist", stream["end_error"])
	errs := stream["errors"].([]interface{})
	require.Equal(t, "auth failed: Bearer ***", errs[0])

	po := parsed["po"].([]interface{})
	require.Equal(t, "set_header Authorization: ***", po[0])

	// The opaque base64 field holds no credentials, so it is byte-identical
	// after decode → redact → re-encode (frontend decoding stays intact).
	require.Equal(t, "ZXhwcl8xX3JhdGVfbGltaXQ=", parsed["expr_b64"])
}

// TestRedactLogOtherMasksModelName verifies the log API never returns an
// unmasked model_name (custom model names may embed credentials).
func TestRedactLogOtherMasksModelName(t *testing.T) {
	log := &Log{
		Content:   "ok",
		ModelName: "custom-ep-20250101-abc123",
		Other:     "",
	}

	redactLogOther(log, false)

	require.Equal(t, "custom-ep-***", log.ModelName)
}

// TestRedactLogOtherRedactsExprB64Credentials verifies reversible base64
// payloads (expr_b64) are decoded, redacted and re-encoded, so credentials
// embedded inside them cannot be recovered by the frontend.
func TestRedactLogOtherRedactsExprB64Credentials(t *testing.T) {
	enc := base64.StdEncoding.EncodeToString([]byte("rate limit exceeded: Bearer sk-abc123def456ghi789"))
	other := common.MapToJsonStr(map[string]interface{}{"expr_b64": enc})
	log := &Log{Content: "ok", Other: other}

	redactLogOther(log, false)

	parsed, err := common.StrToMap(log.Other)
	require.NoError(t, err)
	got, ok := parsed["expr_b64"].(string)
	require.True(t, ok)
	decoded, err := base64.StdEncoding.DecodeString(got)
	require.NoError(t, err)
	require.NotContains(t, string(decoded), "sk-abc123def456ghi789")
	require.Contains(t, string(decoded), "Bearer ***")
}

// TestRedactLogOtherRedactsInvalidExprB64 verifies undecodable (invalid)
// expr_b64 values are redacted at the raw-text level, so malformed payloads
// cannot smuggle plain credentials through the bypass branch.
func TestRedactLogOtherRedactsInvalidExprB64(t *testing.T) {
	other := common.MapToJsonStr(map[string]interface{}{
		"expr_b64": "Bearer sk-abc123def456ghi789 !!!not-base64!!!",
	})
	log := &Log{Content: "ok", Other: other}

	redactLogOther(log, false)

	parsed, err := common.StrToMap(log.Other)
	require.NoError(t, err)
	got, ok := parsed["expr_b64"].(string)
	require.True(t, ok)
	require.NotContains(t, got, "sk-abc123def456ghi789")
	require.Contains(t, got, "Bearer ***")
}

// TestRedactSensitiveMapValuesDeepNestedSlices verifies credential masking
// recurses through arbitrarily deep JSON arrays, so credentials cannot bypass
// masking one level deep inside nested slice structures.
func TestRedactSensitiveMapValuesDeepNestedSlices(t *testing.T) {
	other := common.MapToJsonStr(map[string]interface{}{
		"po": []interface{}{
			[]interface{}{
				"model ep-20250101-abc123",
				[]interface{}{"Bearer sk-abc123def456ghi789"},
				map[string]interface{}{"err": "key sk-abc123def456ghi789"},
			},
		},
	})
	log := &Log{Content: "ok", Other: other}

	redactLogOther(log, false)

	parsed, err := common.StrToMap(log.Other)
	require.NoError(t, err)
	po := parsed["po"].([]interface{})
	inner := po[0].([]interface{})
	require.Equal(t, "model ep-***", inner[0])
	deep := inner[1].([]interface{})
	require.Equal(t, "Bearer ***", deep[0])
	nestedMap := inner[2].(map[string]interface{})
	require.Equal(t, "key sk-***", nestedMap["err"])
}

// TestSanitizeLogForStorage verifies logs are redacted before they hit the
// database: Content / ModelName / credential-like "other" values are masked,
// while troubleshooting fields (upstream_model_name / admin_info) are kept for
// ops and only stripped by the read-view layer.
func TestSanitizeLogForStorage(t *testing.T) {
	log := &Log{
		Content:   "upstream error: model ep-20250101-abc123 failed",
		ModelName: "custom-ep-20250101-abc123",
		Other: common.MapToJsonStr(map[string]interface{}{
			"upstream_model_name": "ep-20250101-abc123",
			"admin_info":          map[string]interface{}{"server_ip": "1.2.3.4"},
			"stream_status":       map[string]interface{}{"end_error": "key sk-abc123def456ghi789"},
		}),
	}

	sanitizeLogForStorage(log)

	require.Equal(t, "upstream error: model ep-*** failed", log.Content)
	require.Equal(t, "custom-ep-***", log.ModelName)
	parsed, err := common.StrToMap(log.Other)
	require.NoError(t, err)
	// 落盘不剥离排障字段，仅掩凭据（剥离属读取视图职责）。
	require.Contains(t, parsed, "upstream_model_name")
	require.Contains(t, parsed, "admin_info")
	stream := parsed["stream_status"].(map[string]interface{})
	require.Equal(t, "key sk-***", stream["end_error"])

	// 畸形 other 兜底脱敏，凭据不得以原始文本落盘。
	log2 := &Log{Content: "ok", Other: `upstream error: ep-20250101-abc123`}
	sanitizeLogForStorage(log2)
	require.Equal(t, `upstream error: ep-***`, log2.Other)

	// nil 安全。
	require.NotPanics(t, func() { sanitizeLogForStorage(nil) })
}

// TestFormatUserLogsHandlesEmptyAndMalformedOther covers boundary cases: empty
// payloads, nil logs and non-JSON text must not panic or corrupt. Unparsable
// payloads keep their original value untouched, while valid JSON has the
// sensitive field stripped.
func TestFormatUserLogsHandlesEmptyAndMalformedOther(t *testing.T) {
	logs := []*Log{
		{Other: ""},
		{Other: "not-json"},
		{Other: common.MapToJsonStr(map[string]interface{}{"upstream_model_name": "ep-20250101-abc123"})},
		{},
	}

	require.NotPanics(t, func() { formatUserLogs(logs, 0) })

	// Unparsable payloads are preserved as-is (no corruption).
	require.Equal(t, "", logs[0].Other)
	require.Equal(t, "not-json", logs[1].Other)
	parsed, err := common.StrToMap(logs[2].Other)
	require.NoError(t, err)
	_, hasUpstream := parsed["upstream_model_name"]
	require.False(t, hasUpstream)
	require.Equal(t, "", logs[3].Other)
}

// TestRedactLogOtherAdminView verifies the admin log view (keepAdmin=true)
// retains admin-only fields but still strips upstream_model_name and masks
// credentials, so the Endpoint ID never leaves via the API.
func TestRedactLogOtherAdminView(t *testing.T) {
	log := &Log{Other: common.MapToJsonStr(map[string]interface{}{
		"upstream_model_name": "ep-20250101-abc123",
		"admin_info":          map[string]interface{}{"use_channel": []interface{}{"ch-1"}},
		"stream_status": map[string]interface{}{
			"end_error": "model ep-20250101-abc123 failed, key sk-abc123def456ghi789",
		},
	})}

	redactLogOther(log, true)

	parsed, err := common.StrToMap(log.Other)
	require.NoError(t, err)
	_, hasUpstream := parsed["upstream_model_name"]
	require.False(t, hasUpstream, "admin log view must also strip upstream_model_name")
	// Admin-only fields stay for admins.
	require.Contains(t, parsed, "admin_info")
	// Credentials inside remaining fields are masked.
	stream := parsed["stream_status"].(map[string]interface{})
	require.Equal(t, "model ep-*** failed, key sk-***", stream["end_error"])
}

// TestRedactLogOtherNilSafe covers nil log inputs (no panic).
func TestRedactLogOtherNilSafe(t *testing.T) {
	require.NotPanics(t, func() { redactLogOther(nil, false) })
	require.NotPanics(t, func() { redactLogOther(nil, true) })
}

// TestRedactLogOtherMasksContent verifies the log body (Content) is redacted
// too, since historical/upstream error text may carry credentials.
func TestRedactLogOtherMasksContent(t *testing.T) {
	log := &Log{
		Content: "构建失败：model ep-20250101-abc123 not found, key sk-abc123def456ghi789",
		Other:   "",
	}

	redactLogOther(log, false)

	require.Equal(t, "构建失败：model ep-*** not found, key sk-***", log.Content)
}

// TestRedactLogOtherMalformedOtherFallback verifies that historical malformed
// "other" payloads (bare strings / arrays that cannot parse to a JSON object)
// are redacted at the raw-text level, so credentials cannot bypass via
// unstructured data.
func TestRedactLogOtherMalformedOtherFallback(t *testing.T) {
	log := &Log{
		Content: "ok",
		Other:   `upstream error: ep-20250101-abc123 failed`,
	}

	redactLogOther(log, false)

	require.Equal(t, `upstream error: ep-*** failed`, log.Other)

	// Array-shaped malformed payload.
	log2 := &Log{Content: "ok", Other: `["model ep-20250101-abc123"]`}
	redactLogOther(log2, false)
	require.Equal(t, `["model ep-***"]`, log2.Other)
}

// TestFormatUserLogsKeepsChannelNameRemoval verifies channel names are hidden
// from user views (existing behavior preserved).
func TestFormatUserLogsKeepsChannelNameRemoval(t *testing.T) {
	logs := []*Log{{ChannelName: "secret-channel", Other: ""}}
	formatUserLogs(logs, 0)
	require.Equal(t, "", logs[0].ChannelName)
}

