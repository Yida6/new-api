package model

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestPropertiesSanitizedRemovesUpstreamModelName verifies that the
// client-safe copy of Properties drops the upstream model name (which may carry
// the Volcano Engine Ark Endpoint ID) while keeping the user-visible fields.
func TestPropertiesSanitizedRemovesUpstreamModelName(t *testing.T) {
	props := Properties{
		Input:             "a video prompt",
		UpstreamModelName: "ep-20250101-abc123",
		OriginModelName:   "doubao-seedance-2-0-260128",
	}

	sanitized := props.Sanitized()

	require.Equal(t, "", sanitized.UpstreamModelName)
	require.Equal(t, props.Input, sanitized.Input)
	require.Equal(t, props.OriginModelName, sanitized.OriginModelName)
	// The original value must be preserved for internal flows (e.g.
	// ResolveOriginTask) and DB storage.
	require.Equal(t, "ep-20250101-abc123", props.UpstreamModelName)
}

// TestSanitizedPropertiesJSONOmitsUpstreamModelName verifies the JSON emitted
// for the sanitized Properties never contains upstream_model_name / the Endpoint
// ID, while the raw struct still serializes it (DB storage depends on it).
func TestSanitizedPropertiesJSONOmitsUpstreamModelName(t *testing.T) {
	props := Properties{
		Input:             "prompt",
		UpstreamModelName: "ep-20250101-abc123",
		OriginModelName:   "doubao-seedance-2-0-260128",
	}

	sanitizedJSON, err := json.Marshal(props.Sanitized())
	require.NoError(t, err)
	require.NotContains(t, string(sanitizedJSON), "upstream_model_name")
	require.NotContains(t, string(sanitizedJSON), "ep-20250101-abc123")
	require.Contains(t, string(sanitizedJSON), "doubao-seedance-2-0-260128")

	rawJSON, err := json.Marshal(props)
	require.NoError(t, err)
	require.Contains(t, string(rawJSON), "upstream_model_name")
	require.Contains(t, string(rawJSON), "ep-20250101-abc123")
}

// TestSanitizedPropertiesHandlesEdgeCases covers empty and unmapped properties.
func TestSanitizedPropertiesHandlesEdgeCases(t *testing.T) {
	// Empty properties: sanitizing must be a no-op and marshal to {}.
	empty := Properties{}
	require.Equal(t, "", empty.Sanitized().UpstreamModelName)
	emptyJSON, err := json.Marshal(empty.Sanitized())
	require.NoError(t, err)
	require.NotContains(t, string(emptyJSON), "upstream_model_name")

	// Unmapped channel (no upstream model name): sanitized equals original.
	unmapped := Properties{Input: "x", OriginModelName: "m"}
	require.Equal(t, unmapped, unmapped.Sanitized())
}
