package service

import (
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func cleanupSeedanceCostControls(t *testing.T) {
	t.Helper()
	require.NoError(t, model.DB.AutoMigrate(&model.SeedanceCostControl{}))
	require.NoError(t, model.DB.Exec("DELETE FROM seedance_cost_controls").Error)
}

func TestReserveSeedanceDailyCostAlertAndCircuitBreaker(t *testing.T) {
	cleanupSeedanceCostControls(t)
	now := time.Date(2026, 8, 12, 9, 0, 0, 0, time.Local)

	first, err := model.ReserveSeedanceDailyCost(6_000_000, 8_000_000, 10_000_000, now)
	require.NoError(t, err)
	assert.True(t, first.Allowed)
	assert.False(t, first.AlertTriggered)
	assert.Equal(t, int64(6_000_000), first.SpentMicros)

	second, err := model.ReserveSeedanceDailyCost(2_000_000, 8_000_000, 10_000_000, now)
	require.NoError(t, err)
	assert.True(t, second.Allowed)
	assert.True(t, second.AlertTriggered, "crossing the alert threshold must emit one alert")

	rejected, err := model.ReserveSeedanceDailyCost(2_000_001, 8_000_000, 10_000_000, now)
	require.NoError(t, err)
	assert.False(t, rejected.Allowed, "a task that would exceed the hard limit must be blocked before submit")
	assert.True(t, rejected.TripTriggered)
	assert.Equal(t, int64(8_000_000), rejected.SpentMicros, "rejected tasks must not add cost")

	rejectedAgain, err := model.ReserveSeedanceDailyCost(2_000_001, 8_000_000, 10_000_000, now)
	require.NoError(t, err)
	assert.False(t, rejectedAgain.Allowed)
	assert.False(t, rejectedAgain.TripTriggered, "the administrator must only be notified once per day")
}

func TestReleaseSeedanceDailyCostAndNextDayReset(t *testing.T) {
	cleanupSeedanceCostControls(t)
	now := time.Date(2026, 8, 12, 23, 59, 0, 0, time.Local)

	reservation, err := model.ReserveSeedanceDailyCost(9_000_000, 0, 10_000_000, now)
	require.NoError(t, err)
	require.True(t, reservation.Allowed)
	require.NoError(t, model.ReleaseSeedanceDailyCost(reservation.Period, 4_000_000))

	afterRelease, err := model.ReserveSeedanceDailyCost(5_000_000, 0, 10_000_000, now)
	require.NoError(t, err)
	assert.True(t, afterRelease.Allowed, "a confirmed pre-send failure must release its reservation")
	assert.Equal(t, int64(10_000_000), afterRelease.SpentMicros)

	nextDay, err := model.ReserveSeedanceDailyCost(10_000_000, 0, 10_000_000, now.Add(2*time.Minute))
	require.NoError(t, err)
	assert.True(t, nextDay.Allowed, "the circuit breaker must reset on the next local calendar day")
	assert.NotEqual(t, reservation.Period, nextDay.Period)
}

func TestEstimateSeedanceUpstreamCostExcludesGroupMarkup(t *testing.T) {
	originalQuotaPerUnit := common.QuotaPerUnit
	common.QuotaPerUnit = 500_000
	t.Cleanup(func() { common.QuotaPerUnit = originalQuotaPerUnit })

	fixedPrice := types.PriceData{UsePrice: true, ModelPrice: 0.50}
	fixedPrice.AddOtherRatio("duration", 2)
	assert.Equal(t, int64(1_000_000), EstimateSeedanceUpstreamCostMicros(fixedPrice))

	ratioPrice := types.PriceData{
		Quota:          1_000_000,
		GroupRatioInfo: types.GroupRatioInfo{GroupRatio: 2},
	}
	assert.Equal(t, int64(1_000_000), EstimateSeedanceUpstreamCostMicros(ratioPrice),
		"retail group markup must not inflate the upstream-cost guardrail")
}
