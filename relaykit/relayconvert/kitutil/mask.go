package kitutil

import (
	"net/url"
	"regexp"
	"strings"
)

var (
	maskURLPattern    = regexp.MustCompile(`(http|https)://[^\s/$.?#].[^\s]*`)
	maskDomainPattern = regexp.MustCompile(`\b(?:[a-zA-Z0-9](?:[a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?\.)+[a-zA-Z]{2,}\b`)
	maskIPPattern     = regexp.MustCompile(`\b(?:\d{1,3}\.){3}\d{1,3}\b`)
	// maskApiKeyPattern matches patterns like 'api_key:xxx' or "api_key:xxx" to mask the API key value
	maskApiKeyPattern = regexp.MustCompile(`(['"]?)api_key:([^\s'"]+)(['"]?)`)
	// maskArkEndpointPattern masks Volcano Engine Ark Endpoint IDs (e.g.
	// "ep-20250101-abc123" / "EP-20250101-ABC123" / "ep-abc_def_123456").
	// Case-insensitive and underscore-tolerant: Endpoint IDs may be
	// uppercased by tooling or contain underscores. The 8+ digit date anchor
	// avoids false positives on ordinary text.
	maskArkEndpointPattern = regexp.MustCompile(`(?i)\bep-[0-9]{8,}(?:-[A-Za-z0-9_-]+)?\b`)
	// maskArkEndpointLongPattern covers Endpoint-ID variants without a
	// date-style prefix (long "ep-" + alphanumeric/underscore runs); the
	// 12+ char length keeps false positives on ordinary text negligible.
	maskArkEndpointLongPattern = regexp.MustCompile(`(?i)\bep-[A-Za-z0-9_-]{12,}\b`)
	// maskOpenAIKeyPattern masks OpenAI-style API keys ("sk-" + 8+ chars).
	maskOpenAIKeyPattern = regexp.MustCompile(`(?i)\bsk-[A-Za-z0-9_-]{8,}\b`)
	// maskBearerPattern masks bearer tokens in "Authorization: Bearer <token>"
	// style text without touching the surrounding message.
	maskBearerPattern = regexp.MustCompile(`(?i)\bbearer\s+[A-Za-z0-9._~+/=-]+`)
	// maskAuthorizationPattern masks Authorization headers that carry no
	// "Bearer" keyword (e.g. "Authorization: Basic <b64>", raw tokens).
	maskAuthorizationPattern = regexp.MustCompile(`(?i)\bauthorization\s*[:=]\s*[^\s,;'"]+(\s+[^\s,;'"]+)*`)
	// maskJsonAuthorizationPattern masks quoted JSON Authorization headers,
	// e.g. {"authorization": "Basic dXNlcjpwYXNz"}.
	maskJsonAuthorizationPattern = regexp.MustCompile(`(?i)"authorization"\s*:\s*"[^"]+"`)
	// maskCredentialPairPattern masks "key:value"/"key=value" credential pairs
	// (x-api-key, api_key, apikey, access_key, access_token, auth_token, token,
	// secret). The value must be 8+ chars, so token counts like "token=128"
	// are left alone.
	maskCredentialPairPattern = regexp.MustCompile(`(?i)\b((?:x-api-key|api[_-]?key|access[_-]?key|access[_-]?token|auth[_-]?token|token|secret))\s*[:=]\s*[^\s,;'"]{8,}`)
	// maskJsonCredentialPairPattern masks quoted JSON credential pairs,
	// e.g. {"apiKey":"abcdef1234567890","token":"..."}. Keys must be quoted
	// and values must be 8+ chars to avoid corrupting short/ordinary fields.
	maskJsonCredentialPairPattern = regexp.MustCompile(`(?i)"(x-api-key|api[_-]?key|access[_-]?key|access[_-]?token|auth[_-]?token|token|secret)"\s*:\s*"[^"]{8,}"`)
)

// maskHostTail returns the tail parts of a domain/host that should be preserved.
// It keeps 2 parts for likely country-code TLDs (e.g., co.uk, com.cn), otherwise keeps only the TLD.
func maskHostTail(parts []string) []string {
	if len(parts) < 2 {
		return parts
	}
	lastPart := parts[len(parts)-1]
	secondLastPart := parts[len(parts)-2]
	if len(lastPart) == 2 && len(secondLastPart) <= 3 {
		// Likely country code TLD like co.uk, com.cn
		return []string{secondLastPart, lastPart}
	}
	return []string{lastPart}
}

// maskHostForURL collapses subdomains and keeps only masked prefix + preserved tail.
// Example: api.openai.com -> ***.com, sub.domain.co.uk -> ***.co.uk
func maskHostForURL(host string) string {
	parts := strings.Split(host, ".")
	if len(parts) < 2 {
		return "***"
	}
	tail := maskHostTail(parts)
	return "***." + strings.Join(tail, ".")
}

// maskHostForPlainDomain masks a plain domain and reflects subdomain depth with multiple ***.
// Example: openai.com -> ***.com, api.openai.com -> ***.***.com, sub.domain.co.uk -> ***.***.co.uk
func maskHostForPlainDomain(domain string) string {
	parts := strings.Split(domain, ".")
	if len(parts) < 2 {
		return domain
	}
	tail := maskHostTail(parts)
	numStars := len(parts) - len(tail)
	if numStars < 1 {
		numStars = 1
	}
	stars := strings.TrimSuffix(strings.Repeat("***.", numStars), ".")
	return stars + "." + strings.Join(tail, ".")
}

// RedactCredentials masks credential-like values that may leak into
// client-facing output: Volcano Engine Ark Endpoint IDs (ep-xxxx), OpenAI-style
// API keys (sk-xxx), bearer tokens, Authorization headers and explicit
// api_key/token/secret pairs. Unlike MaskSensitiveInfo it leaves
// URLs/domains/IPs untouched, so it is safe to apply to upstream error text or
// stored payloads without corrupting their meaning.
func RedactCredentials(str string) string {
	// Bearer tokens first: "Bearer sk-xxx" is fully replaced so the sk- pattern
	// below cannot double-mask the token.
	str = maskBearerPattern.ReplaceAllString(str, "Bearer ***")
	str = maskAuthorizationPattern.ReplaceAllString(str, "Authorization: ***")
	str = maskJsonAuthorizationPattern.ReplaceAllString(str, `"authorization": "***"`)
	str = maskArkEndpointPattern.ReplaceAllString(str, "ep-***")
	str = maskArkEndpointLongPattern.ReplaceAllString(str, "ep-***")
	str = maskOpenAIKeyPattern.ReplaceAllString(str, "sk-***")
	// Explicit "api_key:<value>" keeps its compact form.
	str = maskApiKeyPattern.ReplaceAllString(str, "${1}api_key:***${3}")
	// Generic credential pairs ("key:value"/"key=value") and quoted JSON forms.
	str = maskCredentialPairPattern.ReplaceAllString(str, "$1: ***")
	str = maskJsonCredentialPairPattern.ReplaceAllString(str, `"$1": "***"`)
	return str
}

// MaskSensitiveInfo masks sensitive information like URLs, IPs, and domain names in a string
// Example:
// http://example.com -> http://***.com
// https://api.test.org/v1/users/123?key=secret -> https://***.org/***/***/?key=***
// https://sub.domain.co.uk/path/to/resource -> https://***.co.uk/***/***
// 192.168.1.1 -> ***.***.***.***
// openai.com -> ***.com
// www.openai.com -> ***.***.com
// api.openai.com -> ***.***.com
func MaskSensitiveInfo(str string) string {
	// Mask URLs
	str = maskURLPattern.ReplaceAllStringFunc(str, func(urlStr string) string {
		u, err := url.Parse(urlStr)
		if err != nil {
			return urlStr
		}

		host := u.Host
		if host == "" {
			return urlStr
		}

		// Mask host with unified logic
		maskedHost := maskHostForURL(host)

		result := u.Scheme + "://" + maskedHost

		// Mask path
		if u.Path != "" && u.Path != "/" {
			pathParts := strings.Split(strings.Trim(u.Path, "/"), "/")
			maskedPathParts := make([]string, len(pathParts))
			for i := range pathParts {
				if pathParts[i] != "" {
					maskedPathParts[i] = "***"
				}
			}
			if len(maskedPathParts) > 0 {
				result += "/" + strings.Join(maskedPathParts, "/")
			}
		} else if u.Path == "/" {
			result += "/"
		}

		// Mask query parameters
		if u.RawQuery != "" {
			values, err := url.ParseQuery(u.RawQuery)
			if err != nil {
				// If can't parse query, just mask the whole query string
				result += "?***"
			} else {
				maskedParams := make([]string, 0, len(values))
				for key := range values {
					maskedParams = append(maskedParams, key+"=***")
				}
				if len(maskedParams) > 0 {
					result += "?" + strings.Join(maskedParams, "&")
				}
			}
		}

		return result
	})

	// Mask domain names without protocol (like openai.com, www.openai.com)
	str = maskDomainPattern.ReplaceAllStringFunc(str, func(domain string) string {
		return maskHostForPlainDomain(domain)
	})

	// Mask IP addresses
	str = maskIPPattern.ReplaceAllString(str, "***.***.***.***")

	// Mask API keys (e.g., "api_key:AIzaSyAAAaUooTUni8AdaOkSRMda30n_Q4vrV70" -> "api_key:***")
	str = maskApiKeyPattern.ReplaceAllString(str, "${1}api_key:***${3}")

	// Mask credential-like values (Ark Endpoint IDs, sk- keys, bearer tokens).
	return RedactCredentials(str)
}
