package model

import (
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// SeedanceCostControl stores the site-wide Seedance cost reserved for one
// local-calendar day. Cost is stored in millionths of a USD to avoid floating
// point comparisons in the circuit breaker.
type SeedanceCostControl struct {
	Period      string `json:"period" gorm:"primaryKey;type:varchar(10)"`
	SpentMicros int64  `json:"spent_micros" gorm:"not null;default:0"`
	AlertedAt   int64  `json:"alerted_at" gorm:"not null;default:0"`
	TrippedAt   int64  `json:"tripped_at" gorm:"not null;default:0"`
	UpdatedAt   int64  `json:"updated_at" gorm:"index"`
}

func (SeedanceCostControl) TableName() string { return "seedance_cost_controls" }

type SeedanceCostReservation struct {
	Allowed        bool
	Period         string
	SpentMicros    int64
	AlertTriggered bool
	TripTriggered  bool
}

// ReserveSeedanceDailyCost atomically checks the hard limit and reserves the
// projected upstream cost before a new task is sent. Existing task polling does
// not call this function and is therefore never interrupted by the breaker.
func ReserveSeedanceDailyCost(costMicros, alertMicros, limitMicros int64, now time.Time) (SeedanceCostReservation, error) {
	period := now.In(time.Local).Format("2006-01-02")
	result := SeedanceCostReservation{Allowed: true, Period: period}
	if costMicros <= 0 || (alertMicros <= 0 && limitMicros <= 0) {
		return result, nil
	}

	tx := DB.Begin()
	if tx.Error != nil {
		return result, tx.Error
	}
	defer func() { _ = tx.Rollback() }()

	timestamp := now.Unix()
	if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&SeedanceCostControl{
		Period: period, UpdatedAt: timestamp,
	}).Error; err != nil {
		return result, err
	}

	var control SeedanceCostControl
	if err := lockForUpdate(tx).Where("period = ?", period).First(&control).Error; err != nil {
		return result, err
	}

	if limitMicros > 0 && (control.SpentMicros >= limitMicros || costMicros > limitMicros-control.SpentMicros) {
		result.Allowed = false
		result.SpentMicros = control.SpentMicros
		if control.TrippedAt == 0 {
			result.TripTriggered = true
			if err := tx.Model(&SeedanceCostControl{}).Where("period = ?", period).
				Updates(map[string]any{"tripped_at": timestamp, "updated_at": timestamp}).Error; err != nil {
				return result, err
			}
		}
		return result, tx.Commit().Error
	}

	control.SpentMicros += costMicros
	updates := map[string]any{"spent_micros": control.SpentMicros, "updated_at": timestamp}
	if alertMicros > 0 && control.SpentMicros >= alertMicros && control.AlertedAt == 0 {
		updates["alerted_at"] = timestamp
		result.AlertTriggered = true
	}
	if err := tx.Model(&SeedanceCostControl{}).Where("period = ?", period).Updates(updates).Error; err != nil {
		return result, err
	}
	result.SpentMicros = control.SpentMicros
	return result, tx.Commit().Error
}

// ReleaseSeedanceDailyCost releases a reservation when it is certain that the
// upstream did not create a task. Unknown outcomes deliberately keep the cost.
func ReleaseSeedanceDailyCost(period string, costMicros int64) error {
	if period == "" || costMicros <= 0 {
		return nil
	}
	return DB.Transaction(func(tx *gorm.DB) error {
		var control SeedanceCostControl
		err := lockForUpdate(tx).Where("period = ?", period).First(&control).Error
		if err == gorm.ErrRecordNotFound {
			return nil
		}
		if err != nil {
			return err
		}
		spent := control.SpentMicros - costMicros
		if spent < 0 {
			spent = 0
		}
		return tx.Model(&SeedanceCostControl{}).Where("period = ?", period).
			Updates(map[string]any{"spent_micros": spent, "updated_at": time.Now().Unix()}).Error
	})
}
