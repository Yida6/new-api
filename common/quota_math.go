package common

import (
	"fmt"
	"math"

	"github.com/shopspring/decimal"
)

// Quota conversions are centralized here so every billing path shares one
// saturation + logging policy. Quota columns (user/token/log) were historically
// 32-bit integers in the database; they have been upgraded to int64/BIGINT so an
// oversized product must clamp to the int64 range instead of wrapping around and
// turning a charge into a credit.
const (
	MaxQuota = math.MaxInt64
	MinQuota = math.MinInt64
)

// MaxTokenQuota is the business-level ceiling for a single quota amount (e.g.
// an API Key's RemainQuota), expressed in internal quota units. It is:
//   - explicitly configurable in one place (change here to raise/lower the cap);
//   - exactly representable as int64 (and as float64, well below 2^53);
//   - far below both the float64 precision limit and the int64 boundary, leaving
//     headroom for price/ratio/multiplier arithmetic in billing paths.
//
// With the default QuotaPerUnit = 500000 this allows roughly 2,000,000,000 USD
// per key (~1e15 / 5e5), which safely covers the former int32 ceiling of
// ~4294.96 USD by many orders of magnitude.
const MaxTokenQuota int64 = 1_000_000_000_000_000 // 1e15

// QuotaClampKind identifies why a quota conversion had to be saturated.
type QuotaClampKind string

// Clamp kinds reported by QuotaClamp.Kind.
const (
	QuotaClampOverflow  QuotaClampKind = "overflow"
	QuotaClampUnderflow QuotaClampKind = "underflow"
	QuotaClampNaN       QuotaClampKind = "nan"
)

// QuotaClamp describes a single saturation event: a quota conversion whose
// input fell outside the representable int64 range (or was NaN) and was
// therefore clamped. It is surfaced to billing callers so the event can be
// recorded on the related consume/task log for admin auditing.
type QuotaClamp struct {
	Op       string         `json:"op"`       // "QuotaFromFloat" | "QuotaRound" | "QuotaFromDecimal"
	Kind     QuotaClampKind `json:"kind"`     // "overflow" | "underflow" | "nan"
	Original float64        `json:"original"` // best-effort pre-clamp value (decimal -> float64 approx)
	Clamped  int64          `json:"clamped"`  // the saturated result actually used
}

// Error lets the same typed value serve both as the settlement audit marker
// and as the fail-fast error returned by strict pre-consume conversions.
func (c *QuotaClamp) Error() string {
	if c == nil {
		return ""
	}
	return fmt.Sprintf("quota conversion (%s) %s: original=%g, clamped=%d", c.Op, c.Kind, c.Original, c.Clamped)
}

// AuditMap renders the clamp as the marker stored under a log's
// admin_info.quota_saturation. Centralized here so every billing path (consume
// logs, task billing logs, task compensation logs) records the same shape.
func (c *QuotaClamp) AuditMap() map[string]interface{} {
	if c == nil {
		return nil
	}
	return map[string]interface{}{
		"op":       c.Op,
		"kind":     c.Kind,
		"original": c.Original,
		"clamped":  c.Clamped,
	}
}

// floatToInt64 converts a float64 to int64, guarding the conversion boundary:
// Go's float->int conversion is undefined when the value is out of the int64
// range, so saturate first. Value is expected to already be within
// (-2^63, 2^63) after the boundary checks in saturateQuota.
func floatToInt64(value float64) int64 {
	return int64(value)
}

// saturateQuota converts an already-rounded quota value to int64, clamping to
// the int64 range. Whenever clamping (what would otherwise be an integer
// wraparound) or a NaN fallback is triggered it logs a warning, because in
// normal operation a single request never approaches these bounds — hitting
// them signals a bug or an abusive request. `op` names the caller. When a
// clamp occurs it returns a non-nil *QuotaClamp so callers can additionally
// record the event (e.g. on the consume log); the returned pointer is nil for
// in-range values.
func saturateQuota(value float64, op string) (int64, *QuotaClamp) {
	var clamp *QuotaClamp
	switch {
	case math.IsNaN(value):
		clamp = &QuotaClamp{Op: op, Kind: QuotaClampNaN, Original: value, Clamped: 0}
	case value >= float64(MaxQuota): // float64(MaxInt64) == 2^63; clamp anything >= 2^63
		clamp = &QuotaClamp{Op: op, Kind: QuotaClampOverflow, Original: value, Clamped: MaxQuota}
	case value <= float64(MinQuota): // float64(MinInt64) == -2^63; clamp anything <= -2^63
		clamp = &QuotaClamp{Op: op, Kind: QuotaClampUnderflow, Original: value, Clamped: MinQuota}
	default:
		return floatToInt64(value), nil
	}
	SysError(clamp.Error())
	return clamp.Clamped, clamp
}

func strictQuota(quota int64, clamp *QuotaClamp) (int64, error) {
	if clamp != nil {
		return 0, clamp
	}
	return quota, nil
}

// QuotaFromFloat converts a computed quota value to int64, truncating toward
// zero, with saturation. Use for float products of prices, ratios, and
// user-controlled multipliers (image n, video seconds, resolution ratios).
func QuotaFromFloat(value float64) int64 {
	quota, _ := QuotaFromFloatChecked(value)
	return quota
}

// QuotaFromFloatChecked is QuotaFromFloat but also returns a non-nil
// *QuotaClamp when the value was clamped, so billing callers can audit it.
func QuotaFromFloatChecked(value float64) (int64, *QuotaClamp) {
	return saturateQuota(value, "QuotaFromFloat")
}

// QuotaFromFloatStrict converts an in-range value and returns a typed
// *QuotaClamp error instead of allowing a saturated result to reach billing.
func QuotaFromFloatStrict(value float64) (int64, error) {
	return strictQuota(QuotaFromFloatChecked(value))
}

// QuotaRound converts a float64 quota value to int64 using half-away-from-zero
// rounding, with saturation. Every tiered billing path (pre-consume,
// settlement, breakdown validation, log fields) MUST use this to avoid +-1
// discrepancies.
func QuotaRound(value float64) int64 {
	quota, _ := QuotaRoundChecked(value)
	return quota
}

// QuotaRoundChecked is QuotaRound but also returns a non-nil *QuotaClamp when
// the value was clamped, so billing callers can audit it.
func QuotaRoundChecked(value float64) (int64, *QuotaClamp) {
	return saturateQuota(math.Round(value), "QuotaRound")
}

// QuotaRoundStrict rounds an in-range value and returns a typed *QuotaClamp
// error instead of allowing a saturated result to reach billing.
func QuotaRoundStrict(value float64) (int64, error) {
	return strictQuota(QuotaRoundChecked(value))
}

// QuotaFromDecimal converts a computed quota decimal to int64 with saturation.
// The decimal is rounded (half away from zero) before conversion.
func QuotaFromDecimal(d decimal.Decimal) int64 {
	quota, _ := QuotaFromDecimalChecked(d)
	return quota
}

// QuotaFromDecimalChecked is QuotaFromDecimal but also returns a non-nil
// *QuotaClamp when the value was clamped, so billing callers can audit it.
func QuotaFromDecimalChecked(d decimal.Decimal) (int64, *QuotaClamp) {
	f, _ := d.Round(0).Float64()
	return saturateQuota(f, "QuotaFromDecimal")
}

// QuotaFromDecimalStrict converts an in-range decimal quota and rejects a
// value that would otherwise be saturated at the database's int64 boundary.
func QuotaFromDecimalStrict(d decimal.Decimal) (int64, error) {
	return strictQuota(QuotaFromDecimalChecked(d))
}
