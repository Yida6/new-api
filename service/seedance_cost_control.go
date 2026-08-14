package service

import (
	"context"
	"fmt"
	"math"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/logger"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/types"
)

const usdMicros = 1_000_000.0

func seedanceUSDToMicros(value float64) int64 {
	if value <= 0 || math.IsNaN(value) || math.IsInf(value, 0) {
		return 0
	}
	if value >= float64(math.MaxInt64)/usdMicros {
		return math.MaxInt64
	}
	return int64(math.Round(value * usdMicros))
}

// EstimateSeedanceUpstreamCostMicros removes the user-group markup from task
// quota so the guardrail tracks projected upstream cost rather than revenue.
func EstimateSeedanceUpstreamCostMicros(price types.PriceData) int64 {
	if price.UsePrice && price.ModelPrice >= 0 {
		return seedanceUSDToMicros(price.ModelPrice * price.OtherRatioMultiplier())
	}
	groupRatio := price.GroupRatioInfo.GroupRatio
	if groupRatio <= 0 || common.QuotaPerUnit <= 0 {
		return 0
	}
	return seedanceUSDToMicros(float64(price.Quota) / groupRatio / common.QuotaPerUnit)
}

func ReserveSeedanceCost(costMicros int64) (model.SeedanceCostReservation, error) {
	alertMicros := seedanceUSDToMicros(constant.SeedanceDailyCostAlertUSD)
	limitMicros := seedanceUSDToMicros(constant.SeedanceDailyCostLimitUSD)
	reservation, err := model.ReserveSeedanceDailyCost(costMicros, alertMicros, limitMicros, time.Now())
	if err != nil {
		return reservation, err
	}

	spentUSD := float64(reservation.SpentMicros) / usdMicros
	if reservation.AlertTriggered {
		message := fmt.Sprintf("Seedance 当日预计上游成本已达到 $%.2f，告警阈值为 $%.2f，硬熔断阈值为 $%.2f。", spentUSD, constant.SeedanceDailyCostAlertUSD, constant.SeedanceDailyCostLimitUSD)
		logger.LogWarn(context.Background(), message)
		go NotifyRootUser(dto.NotifyTypeQuotaExceed, "Seedance 成本告警", message)
	}
	if reservation.TripTriggered {
		message := fmt.Sprintf("Seedance 成本熔断已开启：当日预计上游成本 $%.2f，新任务将被阻止，已提交任务继续运行；硬阈值为 $%.2f。", spentUSD, constant.SeedanceDailyCostLimitUSD)
		logger.LogWarn(context.Background(), message)
		go NotifyRootUser(dto.NotifyTypeQuotaExceed, "Seedance 成本熔断", message)
	}
	return reservation, nil
}

func ReleaseSeedanceCost(period string, costMicros int64) error {
	return model.ReleaseSeedanceDailyCost(period, costMicros)
}
