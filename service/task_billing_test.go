package service

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/types"
	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func TestMain(m *testing.M) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		panic("failed to open test db: " + err.Error())
	}
	sqlDB, err := db.DB()
	if err != nil {
		panic("failed to get sql.DB: " + err.Error())
	}
	sqlDB.SetMaxOpenConns(1)

	model.DB = db
	model.LOG_DB = db

	common.SetDatabaseTypes(common.DatabaseTypeSQLite, common.DatabaseTypeSQLite)
	common.RedisEnabled = false
	common.BatchUpdateEnabled = false
	common.LogConsumeEnabled = true

	if err := db.AutoMigrate(
		&model.Task{},
		&model.User{},
		&model.Token{},
		&model.Log{},
		&model.Channel{},
		&model.TopUp{},
		&model.UserSubscription{},
		&model.SystemTask{},
		&model.SystemTaskLock{},
		&model.TaskConcurrencySlot{},
		&model.TaskSubmitRecovery{},
		&model.TaskBillingDebt{},
		&model.TaskBillingDebtAudit{},
		&model.QuotaData{},
	); err != nil {
		panic("failed to migrate: " + err.Error())
	}

	os.Exit(m.Run())
}

// ---------------------------------------------------------------------------
// Seed helpers
// ---------------------------------------------------------------------------

func truncate(t *testing.T) {
	t.Helper()
	t.Cleanup(func() {
		model.DB.Exec("DELETE FROM tasks")
		model.DB.Exec("DELETE FROM users")
		model.DB.Exec("DELETE FROM tokens")
		model.DB.Exec("DELETE FROM logs")
		model.DB.Exec("DELETE FROM channels")
		model.DB.Exec("DELETE FROM top_ups")
		model.DB.Exec("DELETE FROM user_subscriptions")
	model.DB.Exec("DELETE FROM system_task_locks")
	model.DB.Exec("DELETE FROM system_tasks")
	model.DB.Exec("DELETE FROM task_billing_debts")
	model.DB.Exec("DELETE FROM task_billing_debt_audits")
	model.DB.Exec("DELETE FROM quota_data")
		// 清空 quota_data 内存缓存，避免跨测试污染
		model.CacheQuotaDataLock.Lock()
		model.CacheQuotaData = make(map[string]*model.QuotaData)
		model.CacheQuotaDataLock.Unlock()
	})
}

func seedUser(t *testing.T, id int, quota int64) {
	t.Helper()
	user := &model.User{Id: id, Username: "test_user", Quota: quota, Status: common.UserStatusEnabled}
	require.NoError(t, model.DB.Create(user).Error)
}

func seedToken(t *testing.T, id int, userId int, key string, remainQuota int64) {
	t.Helper()
	token := &model.Token{
		Id:          id,
		UserId:      userId,
		Key:         key,
		Name:        "test_token",
		Status:      common.TokenStatusEnabled,
		RemainQuota: remainQuota,
		UsedQuota:   0,
	}
	require.NoError(t, model.DB.Create(token).Error)
}

func seedSubscription(t *testing.T, id int, userId int, amountTotal int64, amountUsed int64) {
	t.Helper()
	sub := &model.UserSubscription{
		Id:          id,
		UserId:      userId,
		AmountTotal: amountTotal,
		AmountUsed:  amountUsed,
		Status:      "active",
		StartTime:   time.Now().Unix(),
		EndTime:     time.Now().Add(30 * 24 * time.Hour).Unix(),
	}
	require.NoError(t, model.DB.Create(sub).Error)
}

func seedChannel(t *testing.T, id int) {
	t.Helper()
	ch := &model.Channel{Id: id, Name: "test_channel", Key: "sk-test", Status: common.ChannelStatusEnabled}
	require.NoError(t, model.DB.Create(ch).Error)
}

// seedUsedQuota 预置用户/渠道的累计消耗，模拟"提交任务已预扣"的前置状态，
// 使退款/差额结算的 used_quota 冲减守卫能够通过（生产路径由 LogTaskConsumption 完成预扣）。
func seedUsedQuota(t *testing.T, userID, channelID int, amount int64) {
	t.Helper()
	require.NoError(t, model.DB.Model(&model.User{}).Where("id = ?", userID).Update("used_quota", amount).Error)
	if channelID > 0 {
		require.NoError(t, model.DB.Model(&model.Channel{}).Where("id = ?", channelID).Update("used_quota", int64(amount)).Error)
	}
}

func makeTask(userId, channelId int, quota int64, tokenId int, billingSource string, subscriptionId int) *model.Task {
	return &model.Task{
		TaskID:    "task_" + time.Now().Format("150405.000"),
		UserId:    userId,
		ChannelId: channelId,
		Quota:     quota,
		Status:    model.TaskStatus(model.TaskStatusInProgress),
		Group:     "default",
		Data:      json.RawMessage(`{}`),
		CreatedAt: time.Now().Unix(),
		UpdatedAt: time.Now().Unix(),
		Properties: model.Properties{
			OriginModelName: "test-model",
		},
		PrivateData: model.TaskPrivateData{
			BillingSource:  billingSource,
			SubscriptionId: subscriptionId,
			TokenId:        tokenId,
			// 默认消费日志已记录（对应 LogConsumeEnabled=true），结算/退款据此写计费调整日志。
			ConsumeLogRecorded: true,
			BillingContext: &model.TaskBillingContext{
				ModelPrice:      0.02,
				GroupRatio:      1.0,
				OriginModelName: "test-model",
			},
		},
	}
}

func TestPriceDataOtherRatiosFilterAndSnapshot(t *testing.T) {
	priceData := types.PriceData{}

	priceData.AddOtherRatio("zero", 0)
	priceData.AddOtherRatio("negative", -0.5)
	priceData.AddOtherRatio("nan", math.NaN())
	priceData.AddOtherRatio("inf", math.Inf(1))
	priceData.AddOtherRatio("one", 1)
	priceData.AddOtherRatio("positive", 2.5)

	ratios := priceData.OtherRatios()
	require.Len(t, ratios, 2)
	assert.Equal(t, 1.0, ratios["one"])
	assert.Equal(t, 2.5, ratios["positive"])
	assert.True(t, priceData.HasOtherRatio("one"))
	assert.False(t, priceData.HasOtherRatio("zero"))

	ratios["positive"] = 99
	ratios["new"] = 3
	nextSnapshot := priceData.OtherRatios()
	assert.Equal(t, 2.5, nextSnapshot["positive"])
	assert.NotContains(t, nextSnapshot, "new")
}

func TestPriceDataReplaceAndApplyOtherRatios(t *testing.T) {
	priceData := types.PriceData{}

	replaced := priceData.ReplaceOtherRatios(map[string]float64{
		"zero":     0,
		"negative": -3,
		"nan":      math.NaN(),
		"inf":      math.Inf(1),
		"one":      1,
		"duration": 2,
		"size":     1.5,
	})

	require.True(t, replaced)
	assert.Equal(t, 3.0, priceData.OtherRatioMultiplier())
	assert.Equal(t, 30.0, priceData.ApplyOtherRatiosToFloat(10))
	assert.Equal(t, 10.0, priceData.RemoveOtherRatiosFromFloat(30))
	assert.True(t, decimal.NewFromInt(30).Equal(priceData.ApplyOtherRatiosToDecimal(decimal.NewFromInt(10))))

	replaced = priceData.ReplaceOtherRatios(map[string]float64{
		"zero": 0,
		"nan":  math.NaN(),
	})

	require.False(t, replaced)
	assert.Nil(t, priceData.OtherRatios())
	assert.Equal(t, 1.0, priceData.OtherRatioMultiplier())
}

func TestTaskBillingOtherFiltersHistoricalOtherRatios(t *testing.T) {
	task := makeTask(1, 1, 100, 0, BillingSourceWallet, 0)
	task.PrivateData.BillingContext.OtherRatios = map[string]float64{
		"seconds":  2,
		"identity": 1,
		"zero":     0,
		"negative": -1,
		"nan":      math.NaN(),
		"inf":      math.Inf(1),
	}

	other := taskBillingOther(task)

	assert.Equal(t, 2.0, other["seconds"])
	assert.Equal(t, 1.0, other["identity"])
	assert.NotContains(t, other, "zero")
	assert.NotContains(t, other, "negative")
	assert.NotContains(t, other, "nan")
	assert.NotContains(t, other, "inf")
}

func TestTaskBillingContextPriceDataFiltersMultiplier(t *testing.T) {
	priceData := taskBillingContextPriceData(&model.TaskBillingContext{
		OtherRatios: map[string]float64{
			"seconds":  2,
			"size":     3,
			"identity": 1,
			"zero":     0,
			"negative": -1,
			"nan":      math.NaN(),
			"inf":      math.Inf(1),
		},
	})

	require.NotNil(t, priceData)
	assert.Equal(t, 6.0, priceData.OtherRatioMultiplier())
	assert.Equal(t, map[string]float64{
		"seconds":  2,
		"size":     3,
		"identity": 1,
	}, priceData.OtherRatios())
}

// ---------------------------------------------------------------------------
// Read-back helpers
// ---------------------------------------------------------------------------

func getUserQuota(t *testing.T, id int) int64 {
	t.Helper()
	var user model.User
	require.NoError(t, model.DB.Select("quota").Where("id = ?", id).First(&user).Error)
	return user.Quota
}

func getTokenRemainQuota(t *testing.T, id int) int64 {
	t.Helper()
	var token model.Token
	require.NoError(t, model.DB.Select("remain_quota").Where("id = ?", id).First(&token).Error)
	return token.RemainQuota
}

func getTokenUsedQuota(t *testing.T, id int) int64 {
	t.Helper()
	var token model.Token
	require.NoError(t, model.DB.Select("used_quota").Where("id = ?", id).First(&token).Error)
	return token.UsedQuota
}

func getSubscriptionUsed(t *testing.T, id int) int64 {
	t.Helper()
	var sub model.UserSubscription
	require.NoError(t, model.DB.Select("amount_used").Where("id = ?", id).First(&sub).Error)
	return sub.AmountUsed
}

func getTaskQuota(t *testing.T, id int64) int64 {
	t.Helper()
	var task model.Task
	require.NoError(t, model.DB.Select("quota").Where("id = ?", id).First(&task).Error)
	return task.Quota
}

func getLastLog(t *testing.T) *model.Log {
	t.Helper()
	var log model.Log
	err := model.LOG_DB.Order("id desc").First(&log).Error
	if err != nil {
		return nil
	}
	return &log
}

func countLogs(t *testing.T) int64 {
	t.Helper()
	var count int64
	model.LOG_DB.Model(&model.Log{}).Count(&count)
	return count
}

// ===========================================================================
// RefundTaskQuota tests
// ===========================================================================

func TestRefundTaskQuota_Wallet(t *testing.T) {
	truncate(t)
	ctx := context.Background()

	const userID, tokenID, channelID = 1, 1, 1
	const initQuota, preConsumed = 10000, 3000
	const tokenRemain = 5000

	seedUser(t, userID, initQuota)
	seedToken(t, tokenID, userID, "sk-test-key", tokenRemain)
	seedChannel(t, channelID)
	seedUsedQuota(t, userID, channelID, preConsumed)

	task := makeTask(userID, channelID, preConsumed, tokenID, BillingSourceWallet, 0)
	require.NoError(t, model.DB.Create(task).Error)

	assert.True(t, RefundTaskQuota(ctx, task, "task failed: upstream error"))

	// User quota should increase by preConsumed
	assert.Equal(t, int64(initQuota+preConsumed), getUserQuota(t, userID))

	// Token remain_quota should increase, used_quota should decrease
	assert.Equal(t, int64(tokenRemain+preConsumed), getTokenRemainQuota(t, tokenID))
	assert.Equal(t, int64(-preConsumed), getTokenUsedQuota(t, tokenID))

	// A refund log should be created
	log := getLastLog(t)
	require.NotNil(t, log)
	assert.Equal(t, model.LogTypeRefund, log.Type)
	assert.Equal(t, int64(preConsumed), log.Quota)
	assert.Equal(t, "test-model", log.ModelName)
	assert.Zero(t, task.Quota)
	assert.Zero(t, getTaskQuota(t, task.ID))
}

func TestRefundTaskQuota_Subscription(t *testing.T) {
	truncate(t)
	ctx := context.Background()

	const userID, tokenID, channelID, subID = 2, 2, 2, 1
	const preConsumed = 2000
	const subTotal, subUsed int64 = 100000, 50000
	const tokenRemain = 8000

	seedUser(t, userID, 0)
	seedToken(t, tokenID, userID, "sk-sub-key", tokenRemain)
	seedChannel(t, channelID)
	seedSubscription(t, subID, userID, subTotal, subUsed)
	seedUsedQuota(t, userID, channelID, preConsumed)

	task := makeTask(userID, channelID, preConsumed, tokenID, BillingSourceSubscription, subID)
	require.NoError(t, model.DB.Create(task).Error)

	assert.True(t, RefundTaskQuota(ctx, task, "subscription task failed"))

	// Subscription used should decrease by preConsumed
	assert.Equal(t, subUsed-int64(preConsumed), getSubscriptionUsed(t, subID))

	// Token should also be refunded
	assert.Equal(t, int64(tokenRemain+preConsumed), getTokenRemainQuota(t, tokenID))

	log := getLastLog(t)
	require.NotNil(t, log)
	assert.Equal(t, model.LogTypeRefund, log.Type)
	assert.Zero(t, getTaskQuota(t, task.ID))
}

func TestRefundTaskQuota_ZeroQuota(t *testing.T) {
	truncate(t)
	ctx := context.Background()

	const userID = 3
	seedUser(t, userID, 5000)

	task := makeTask(userID, 0, 0, 0, BillingSourceWallet, 0)

	assert.True(t, RefundTaskQuota(ctx, task, "zero quota task"))

	// No change to user quota
	assert.Equal(t, int64(5000), getUserQuota(t, userID))

	// No log created
	assert.Equal(t, int64(0), countLogs(t))
}

func TestRefundTaskQuota_NoToken(t *testing.T) {
	truncate(t)
	ctx := context.Background()

	const userID, channelID = 4, 4
	const initQuota, preConsumed = 10000, 1500

	seedUser(t, userID, initQuota)
	seedChannel(t, channelID)
	seedUsedQuota(t, userID, channelID, preConsumed)

	task := makeTask(userID, channelID, preConsumed, 0, BillingSourceWallet, 0) // TokenId=0
	require.NoError(t, model.DB.Create(task).Error)

	assert.True(t, RefundTaskQuota(ctx, task, "no token task failed"))

	// User quota refunded
	assert.Equal(t, int64(initQuota+preConsumed), getUserQuota(t, userID))

	// Log created
	log := getLastLog(t)
	require.NotNil(t, log)
	assert.Equal(t, model.LogTypeRefund, log.Type)
	assert.Zero(t, getTaskQuota(t, task.ID))
}

func TestRefundTaskQuota_FundingFailureKeepsPendingMarker(t *testing.T) {
	truncate(t)
	ctx := context.Background()

	const userID, preConsumed = 5, 1200
	seedUser(t, userID, 5000)
	seedUsedQuota(t, userID, 0, preConsumed)
	task := makeTask(userID, 0, preConsumed, 0, BillingSourceSubscription, 9999)
	task.Status = model.TaskStatusFailure
	require.NoError(t, model.DB.Create(task).Error)

	assert.False(t, RefundTaskQuota(ctx, task, "subscription missing"))
	assert.Equal(t, int64(preConsumed), task.Quota)
	assert.Equal(t, int64(preConsumed), getTaskQuota(t, task.ID))
	assert.Equal(t, int64(0), countLogs(t))
}

// ===========================================================================
// RecalculateTaskQuota tests
// ===========================================================================

func TestRecalculate_PositiveDelta(t *testing.T) {
	truncate(t)
	ctx := context.Background()

	const userID, tokenID, channelID = 10, 10, 10
	const initQuota, preConsumed = 10000, 2000
	const actualQuota = 3000 // under-charged by 1000
	const tokenRemain = 5000

	seedUser(t, userID, initQuota)
	seedToken(t, tokenID, userID, "sk-recalc-pos", tokenRemain)
	seedChannel(t, channelID)

	task := makeTask(userID, channelID, preConsumed, tokenID, BillingSourceWallet, 0)
	require.NoError(t, model.DB.Create(task).Error)

	RecalculateTaskQuota(ctx, task, actualQuota, "adaptor adjustment", 0)

	// User quota should decrease by the delta (1000 additional charge)
	assert.Equal(t, int64(initQuota-(actualQuota-preConsumed)), getUserQuota(t, userID))

	// Token should also be charged the delta
	assert.Equal(t, int64(tokenRemain-(actualQuota-preConsumed)), getTokenRemainQuota(t, tokenID))

	// task.Quota should be updated to actualQuota
	assert.Equal(t, int64(actualQuota), task.Quota)

	// Log type should be Consume (additional charge)
	log := getLastLog(t)
	require.NotNil(t, log)
	assert.Equal(t, model.LogTypeConsume, log.Type)
	assert.Equal(t, int64(actualQuota-preConsumed), log.Quota)
}

func TestRecalculate_NegativeDelta(t *testing.T) {
	truncate(t)
	ctx := context.Background()

	const userID, tokenID, channelID = 11, 11, 11
	const initQuota, preConsumed = 10000, 5000
	const actualQuota = 3000 // over-charged by 2000
	const tokenRemain = 5000

	seedUser(t, userID, initQuota)
	seedToken(t, tokenID, userID, "sk-recalc-neg", tokenRemain)
	seedChannel(t, channelID)
	seedUsedQuota(t, userID, channelID, preConsumed)

	task := makeTask(userID, channelID, preConsumed, tokenID, BillingSourceWallet, 0)
	require.NoError(t, model.DB.Create(task).Error)

	RecalculateTaskQuota(ctx, task, actualQuota, "adaptor adjustment", 0)

	// User quota should increase by abs(delta) = 2000 (refund overpayment)
	assert.Equal(t, int64(initQuota+(preConsumed-actualQuota)), getUserQuota(t, userID))

	// Token should be refunded the difference
	assert.Equal(t, int64(tokenRemain+(preConsumed-actualQuota)), getTokenRemainQuota(t, tokenID))

	// task.Quota updated
	assert.Equal(t, int64(actualQuota), task.Quota)

	// Log type should be Refund
	log := getLastLog(t)
	require.NotNil(t, log)
	assert.Equal(t, model.LogTypeRefund, log.Type)
	assert.Equal(t, int64(preConsumed-actualQuota), log.Quota)
}

func TestRecalculate_ZeroDelta(t *testing.T) {
	truncate(t)
	ctx := context.Background()

	const userID = 12
	const initQuota, preConsumed = 10000, 3000

	seedUser(t, userID, initQuota)

	task := makeTask(userID, 0, preConsumed, 0, BillingSourceWallet, 0)

	RecalculateTaskQuota(ctx, task, preConsumed, "exact match", 0)

	// No change to user quota
	assert.Equal(t, int64(initQuota), getUserQuota(t, userID))

	// No log created (delta is zero)
	assert.Equal(t, int64(0), countLogs(t))
}

func TestRecalculate_ActualQuotaZero(t *testing.T) {
	truncate(t)
	ctx := context.Background()

	const userID = 13
	const initQuota = 10000

	seedUser(t, userID, initQuota)

	task := makeTask(userID, 0, 5000, 0, BillingSourceWallet, 0)

	RecalculateTaskQuota(ctx, task, 0, "zero actual", 0)

	// No change (early return)
	assert.Equal(t, int64(initQuota), getUserQuota(t, userID))
	assert.Equal(t, int64(0), countLogs(t))
}

func TestRecalculate_Subscription_NegativeDelta(t *testing.T) {
	truncate(t)
	ctx := context.Background()

	const userID, tokenID, channelID, subID = 14, 14, 14, 2
	const preConsumed = 5000
	const actualQuota = 2000 // over-charged by 3000
	const subTotal, subUsed int64 = 100000, 50000
	const tokenRemain = 8000

	seedUser(t, userID, 0)
	seedToken(t, tokenID, userID, "sk-sub-recalc", tokenRemain)
	seedChannel(t, channelID)
	seedSubscription(t, subID, userID, subTotal, subUsed)
	seedUsedQuota(t, userID, channelID, preConsumed)

	task := makeTask(userID, channelID, preConsumed, tokenID, BillingSourceSubscription, subID)
	require.NoError(t, model.DB.Create(task).Error)

	RecalculateTaskQuota(ctx, task, actualQuota, "subscription over-charge", 0)

	// Subscription used should decrease by delta (refund 3000)
	assert.Equal(t, subUsed-int64(preConsumed-actualQuota), getSubscriptionUsed(t, subID))

	// Token refunded
	assert.Equal(t, int64(tokenRemain+(preConsumed-actualQuota)), getTokenRemainQuota(t, tokenID))

	assert.Equal(t, int64(actualQuota), task.Quota)

	log := getLastLog(t)
	require.NotNil(t, log)
	assert.Equal(t, model.LogTypeRefund, log.Type)
}

// ===========================================================================
// CAS + Billing integration tests
// Simulates the flow in updateVideoSingleTask (service/task_polling.go)
// ===========================================================================

// simulatePollBilling reproduces the CAS + billing logic from updateVideoSingleTask.
// It takes a persisted task (already in DB), applies the new status, and performs
// the conditional update + billing exactly as the polling loop does.
func simulatePollBilling(ctx context.Context, task *model.Task, newStatus model.TaskStatus, actualQuota int64) {
	snap := task.Snapshot()

	shouldRefund := false
	shouldSettle := false
	quota := task.Quota

	task.Status = newStatus
	switch string(newStatus) {
	case model.TaskStatusSuccess:
		task.Progress = "100%"
		task.FinishTime = 9999
		shouldSettle = true
	case model.TaskStatusFailure:
		task.Progress = "100%"
		task.FinishTime = 9999
		task.FailReason = "upstream error"
		if quota != 0 {
			shouldRefund = true
		}
	default:
		task.Progress = "50%"
	}

	isDone := task.Status == model.TaskStatus(model.TaskStatusSuccess) || task.Status == model.TaskStatus(model.TaskStatusFailure)
	if isDone && snap.Status != task.Status {
		won, err := task.UpdateWithStatus(snap.Status)
		if err != nil {
			shouldRefund = false
			shouldSettle = false
		} else if !won {
			shouldRefund = false
			shouldSettle = false
		}
	} else if !snap.Equal(task.Snapshot()) {
		_, _ = task.UpdateWithStatus(snap.Status)
	}

	if shouldSettle && actualQuota > 0 {
		RecalculateTaskQuota(ctx, task, actualQuota, "test settle", 0)
	}
	if shouldRefund {
		RefundTaskQuota(ctx, task, task.FailReason)
	}
}

func TestCASGuardedRefund_Win(t *testing.T) {
	truncate(t)
	ctx := context.Background()

	const userID, tokenID, channelID = 20, 20, 20
	const initQuota, preConsumed = 10000, 4000
	const tokenRemain = 6000

	seedUser(t, userID, initQuota)
	seedToken(t, tokenID, userID, "sk-cas-refund-win", tokenRemain)
	seedChannel(t, channelID)
	seedUsedQuota(t, userID, channelID, preConsumed)

	task := makeTask(userID, channelID, preConsumed, tokenID, BillingSourceWallet, 0)
	task.Status = model.TaskStatus(model.TaskStatusInProgress)
	require.NoError(t, model.DB.Create(task).Error)

	simulatePollBilling(ctx, task, model.TaskStatus(model.TaskStatusFailure), 0)

	// CAS wins: task in DB should now be FAILURE
	var reloaded model.Task
	require.NoError(t, model.DB.First(&reloaded, task.ID).Error)
	assert.EqualValues(t, model.TaskStatusFailure, reloaded.Status)
	assert.Zero(t, reloaded.Quota)

	// Refund should have happened
	assert.Equal(t, int64(initQuota+preConsumed), getUserQuota(t, userID))
	assert.Equal(t, int64(tokenRemain+preConsumed), getTokenRemainQuota(t, tokenID))

	log := getLastLog(t)
	require.NotNil(t, log)
	assert.Equal(t, model.LogTypeRefund, log.Type)
}

func TestCASGuardedRefund_Lose(t *testing.T) {
	truncate(t)
	ctx := context.Background()

	const userID, tokenID, channelID = 21, 21, 21
	const initQuota, preConsumed = 10000, 4000
	const tokenRemain = 6000

	seedUser(t, userID, initQuota)
	seedToken(t, tokenID, userID, "sk-cas-refund-lose", tokenRemain)
	seedChannel(t, channelID)

	// Create task with IN_PROGRESS in DB
	task := makeTask(userID, channelID, preConsumed, tokenID, BillingSourceWallet, 0)
	task.Status = model.TaskStatus(model.TaskStatusInProgress)
	require.NoError(t, model.DB.Create(task).Error)

	// Simulate another process already transitioning to FAILURE
	model.DB.Model(&model.Task{}).Where("id = ?", task.ID).Update("status", model.TaskStatusFailure)

	// Our process still has the old in-memory state (IN_PROGRESS) and tries to transition
	// task.Status is still IN_PROGRESS in the snapshot
	simulatePollBilling(ctx, task, model.TaskStatus(model.TaskStatusFailure), 0)

	// CAS lost: user quota should NOT change (no double refund)
	assert.Equal(t, int64(initQuota), getUserQuota(t, userID))
	assert.Equal(t, int64(tokenRemain), getTokenRemainQuota(t, tokenID))

	// No billing log should be created
	assert.Equal(t, int64(0), countLogs(t))
}

func TestCASGuardedSettle_Win(t *testing.T) {
	truncate(t)
	ctx := context.Background()

	const userID, tokenID, channelID = 22, 22, 22
	const initQuota, preConsumed = 10000, 5000
	const actualQuota = 3000 // over-charged, should get partial refund
	const tokenRemain = 8000

	seedUser(t, userID, initQuota)
	seedToken(t, tokenID, userID, "sk-cas-settle-win", tokenRemain)
	seedChannel(t, channelID)
	seedUsedQuota(t, userID, channelID, preConsumed)

	task := makeTask(userID, channelID, preConsumed, tokenID, BillingSourceWallet, 0)
	task.Status = model.TaskStatus(model.TaskStatusInProgress)
	require.NoError(t, model.DB.Create(task).Error)

	simulatePollBilling(ctx, task, model.TaskStatus(model.TaskStatusSuccess), actualQuota)

	// CAS wins: task should be SUCCESS
	var reloaded model.Task
	require.NoError(t, model.DB.First(&reloaded, task.ID).Error)
	assert.EqualValues(t, model.TaskStatusSuccess, reloaded.Status)

	// Settlement should refund the over-charge (5000 - 3000 = 2000 back to user)
	assert.Equal(t, int64(initQuota+(preConsumed-actualQuota)), getUserQuota(t, userID))
	assert.Equal(t, int64(tokenRemain+(preConsumed-actualQuota)), getTokenRemainQuota(t, tokenID))

	// task.Quota should be updated to actualQuota
	assert.Equal(t, int64(actualQuota), task.Quota)
}

func TestNonTerminalUpdate_NoBilling(t *testing.T) {
	truncate(t)
	ctx := context.Background()

	const userID, channelID = 23, 23
	const initQuota, preConsumed = 10000, 3000

	seedUser(t, userID, initQuota)
	seedChannel(t, channelID)

	task := makeTask(userID, channelID, preConsumed, 0, BillingSourceWallet, 0)
	task.Status = model.TaskStatus(model.TaskStatusInProgress)
	task.Progress = "20%"
	require.NoError(t, model.DB.Create(task).Error)

	// Simulate a non-terminal poll update (still IN_PROGRESS, progress changed)
	simulatePollBilling(ctx, task, model.TaskStatus(model.TaskStatusInProgress), 0)

	// User quota should NOT change
	assert.Equal(t, int64(initQuota), getUserQuota(t, userID))

	// No billing log
	assert.Equal(t, int64(0), countLogs(t))

	// Task progress should be updated in DB
	var reloaded model.Task
	require.NoError(t, model.DB.First(&reloaded, task.ID).Error)
	assert.Equal(t, "50%", reloaded.Progress)
}

// ===========================================================================
// Mock adaptor for settleTaskBillingOnComplete tests
// ===========================================================================

type mockAdaptor struct {
	adjustReturn int64
}

func (m *mockAdaptor) Init(_ *relaycommon.RelayInfo) {}
func (m *mockAdaptor) FetchTask(string, string, map[string]any, string) (*http.Response, error) {
	return nil, nil
}
func (m *mockAdaptor) ParseTaskResult([]byte) (*relaycommon.TaskInfo, error) { return nil, nil }
func (m *mockAdaptor) AdjustBillingOnComplete(_ *model.Task, _ *relaycommon.TaskInfo) int64 {
	return m.adjustReturn
}

// ===========================================================================
// PerCallBilling tests — settleTaskBillingOnComplete
// ===========================================================================

func TestSettle_PerCallBilling_SkipsAdaptorAdjust(t *testing.T) {
	truncate(t)
	ctx := context.Background()

	const userID, tokenID, channelID = 30, 30, 30
	const initQuota, preConsumed = 10000, 5000
	const tokenRemain = 8000

	seedUser(t, userID, initQuota)
	seedToken(t, tokenID, userID, "sk-percall-adaptor", tokenRemain)
	seedChannel(t, channelID)

	task := makeTask(userID, channelID, preConsumed, tokenID, BillingSourceWallet, 0)
	task.PrivateData.BillingContext.PerCallBilling = true

	adaptor := &mockAdaptor{adjustReturn: 2000}
	taskResult := &relaycommon.TaskInfo{Status: model.TaskStatusSuccess}

	settleTaskBillingOnComplete(ctx, adaptor, task, taskResult)

	// Per-call: no adjustment despite adaptor returning 2000
	assert.Equal(t, int64(initQuota), getUserQuota(t, userID))
	assert.Equal(t, int64(tokenRemain), getTokenRemainQuota(t, tokenID))
	assert.Equal(t, int64(preConsumed), task.Quota)
	assert.Equal(t, int64(0), countLogs(t))
}

func TestSettle_PerCallBilling_SkipsTotalTokens(t *testing.T) {
	truncate(t)
	ctx := context.Background()

	const userID, tokenID, channelID = 31, 31, 31
	const initQuota, preConsumed = 10000, 4000
	const tokenRemain = 7000

	seedUser(t, userID, initQuota)
	seedToken(t, tokenID, userID, "sk-percall-tokens", tokenRemain)
	seedChannel(t, channelID)

	task := makeTask(userID, channelID, preConsumed, tokenID, BillingSourceWallet, 0)
	task.PrivateData.BillingContext.PerCallBilling = true

	adaptor := &mockAdaptor{adjustReturn: 0}
	taskResult := &relaycommon.TaskInfo{Status: model.TaskStatusSuccess, TotalTokens: 9999}

	settleTaskBillingOnComplete(ctx, adaptor, task, taskResult)

	// Per-call: no recalculation by tokens
	assert.Equal(t, int64(initQuota), getUserQuota(t, userID))
	assert.Equal(t, int64(tokenRemain), getTokenRemainQuota(t, tokenID))
	assert.Equal(t, int64(preConsumed), task.Quota)
	assert.Equal(t, int64(0), countLogs(t))
}

func TestSettle_NonPerCallBilling_AppliesAdaptorAdjustment(t *testing.T) {
	truncate(t)
	ctx := context.Background()

	const userID, tokenID, channelID = 32, 32, 32
	const initQuota, preConsumed = 10000, 5000
	const adaptorQuota = 3000
	const tokenRemain = 8000

	seedUser(t, userID, initQuota)
	seedToken(t, tokenID, userID, "sk-nonpercall-adj", tokenRemain)
	seedChannel(t, channelID)
	seedUsedQuota(t, userID, channelID, preConsumed)

	task := makeTask(userID, channelID, preConsumed, tokenID, BillingSourceWallet, 0)
	require.NoError(t, model.DB.Create(task).Error)
	// PerCallBilling defaults to false

	adaptor := &mockAdaptor{adjustReturn: adaptorQuota}
	taskResult := &relaycommon.TaskInfo{Status: model.TaskStatusSuccess}

	settleTaskBillingOnComplete(ctx, adaptor, task, taskResult)

	// Non-per-call: adaptor adjustment applies (refund 2000)
	assert.Equal(t, int64(initQuota+(preConsumed-adaptorQuota)), getUserQuota(t, userID))
	assert.Equal(t, int64(tokenRemain+(preConsumed-adaptorQuota)), getTokenRemainQuota(t, tokenID))
	assert.Equal(t, int64(adaptorQuota), task.Quota)

	log := getLastLog(t)
	require.NotNil(t, log)
	assert.Equal(t, model.LogTypeRefund, log.Type)
}

// ===========================================================================
// 净消费统计测试：used_quota / request_count / quota_data / SumUsedQuota 一致性
// ===========================================================================

func getUserUsedQuota(t *testing.T, id int) int64 {
	t.Helper()
	var u model.User
	require.NoError(t, model.DB.Select("used_quota, request_count").Where("id = ?", id).First(&u).Error)
	return u.UsedQuota
}

func getUserRequestCount(t *testing.T, id int) int {
	t.Helper()
	var u model.User
	require.NoError(t, model.DB.Select("used_quota, request_count").Where("id = ?", id).First(&u).Error)
	return u.RequestCount
}

func getChannelUsedQuota(t *testing.T, id int) int64 {
	t.Helper()
	var ch model.Channel
	require.NoError(t, model.DB.Select("used_quota").Where("id = ?", id).First(&ch).Error)
	return ch.UsedQuota
}

// testGinContext 构造带可写 Keys 的 gin.Context，供 RecordConsumeLog 使用。
func testGinContext(username string) *gin.Context {
	c := &gin.Context{Keys: make(map[string]any)}
	c.Set("username", username)
	return c
}

// simulateTaskSubmit 模拟 controller/relay.go 的任务提交统计口径：
// 钱包预扣 + 消费日志（预扣额）+ used_quota += 预扣额 + request_count += 1 + channel.used_quota += 预扣额。
func simulateTaskSubmit(t *testing.T, userID, channelID, tokenID int, preConsumed int64) *model.Task {
	t.Helper()
	require.NoError(t, model.DecreaseUserQuota(userID, preConsumed, false))
	c := testGinContext("test_user")
	model.UpdateUserUsedQuotaAndRequestCount(userID, preConsumed)
	model.UpdateChannelUsedQuota(channelID, preConsumed)
	model.RecordConsumeLog(c, userID, model.RecordConsumeLogParams{
		ChannelId: channelID,
		ModelName: "test-model",
		TokenName: "test_token",
		Quota:     preConsumed,
		Content:   "操作 seedance",
		TokenId:   tokenID,
		Group:     "default",
	})
	task := makeTask(userID, channelID, preConsumed, tokenID, BillingSourceWallet, 0)
	require.NoError(t, model.DB.Create(task).Error)
	return task
}

// simulateTaskSettle 模拟任务完成后的差额结算。
func simulateTaskSettle(ctx context.Context, task *model.Task, actualQuota int64) {
	RecalculateTaskQuota(ctx, task, actualQuota, "adaptor计费调整", 0)
}

func enableDataExport(t *testing.T) {
	t.Helper()
	old := common.DataExportEnabled
	common.DataExportEnabled = true
	t.Cleanup(func() { common.DataExportEnabled = old })
}

// flushQuotaData 将内存缓存中的 quota_data 落库并清空缓存。
func flushQuotaData(t *testing.T) {
	t.Helper()
	model.SaveQuotaDataCache()
}

func sumQuotaDataByUser(t *testing.T, userID int) (count int, quota int64) {
	t.Helper()
	rows, err := model.GetQuotaDataByUserId(userID, 0, time.Now().Add(time.Hour).Unix())
	require.NoError(t, err)
	for _, r := range rows {
		count += r.Count
		quota += r.Quota
	}
	return count, quota
}

// 场景1：预扣 5000、实际 1000（Seedance 差额退款场景）
// 最终用户/渠道 used_quota = 1000，请求数 = 1，趋势额度 = 1000。
func TestTaskNetUsage_Overcharged(t *testing.T) {
	truncate(t)
	enableDataExport(t)
	ctx := context.Background()

	const userID, tokenID, channelID = 50, 50, 50
	const initQuota, preConsumed, actualQuota = 100000, 5000, 1000

	seedUser(t, userID, initQuota)
	seedToken(t, tokenID, userID, "sk-net-over", 100000)
	seedChannel(t, channelID)

	task := simulateTaskSubmit(t, userID, channelID, tokenID, preConsumed)
	simulateTaskSettle(ctx, task, actualQuota)

	// 用户/渠道累计消耗为净消费 1000
	assert.Equal(t, int64(actualQuota), getUserUsedQuota(t, userID))
	assert.Equal(t, 1, getUserRequestCount(t, userID))
	assert.EqualValues(t, actualQuota, getChannelUsedQuota(t, channelID))

	// 趋势额度（quota_data）为净消费 1000、请求计数 1
	flushQuotaData(t)
	count, quota := sumQuotaDataByUser(t, userID)
	assert.Equal(t, 1, count)
	assert.Equal(t, int64(actualQuota), quota)

	// 统计接口返回净值：消费 5000 - 退款 4000 = 1000
	stat, err := model.SumUsedQuota(0, 0, 0, "", "test_user", "test_token", 0, "", "", "")
	require.NoError(t, err)
	assert.Equal(t, int64(actualQuota), stat.Quota)

	// 使用日志同时存在消费(5000)与退款(4000)两条记录
	var consumeLogs, refundLogs int64
	model.LOG_DB.Model(&model.Log{}).Where("user_id = ? AND type = ?", userID, model.LogTypeConsume).Count(&consumeLogs)
	model.LOG_DB.Model(&model.Log{}).Where("user_id = ? AND type = ?", userID, model.LogTypeRefund).Count(&refundLogs)
	assert.EqualValues(t, 1, consumeLogs)
	assert.EqualValues(t, 1, refundLogs)

	// 钱包余额按净消费结算（预扣 5000 后剩 95000，退款 4000 后 99000）
	assert.Equal(t, int64(initQuota-actualQuota), getUserQuota(t, userID))
}

// 场景2：预扣 1000、实际 3000（补扣场景）
// 最终累计消耗 = 3000，请求数仍为 1。
func TestTaskNetUsage_Undercharged(t *testing.T) {
	truncate(t)
	enableDataExport(t)
	ctx := context.Background()

	const userID, tokenID, channelID = 51, 51, 51
	const initQuota, preConsumed, actualQuota = 100000, 1000, 3000

	seedUser(t, userID, initQuota)
	seedToken(t, tokenID, userID, "sk-net-under", 100000)
	seedChannel(t, channelID)

	task := simulateTaskSubmit(t, userID, channelID, tokenID, preConsumed)
	simulateTaskSettle(ctx, task, actualQuota)

	assert.Equal(t, int64(actualQuota), getUserUsedQuota(t, userID))
	assert.Equal(t, 1, getUserRequestCount(t, userID))
	assert.EqualValues(t, actualQuota, getChannelUsedQuota(t, channelID))

	flushQuotaData(t)
	count, quota := sumQuotaDataByUser(t, userID)
	assert.Equal(t, 1, count) // 补扣不计请求
	assert.Equal(t, int64(actualQuota), quota)

	stat, err := model.SumUsedQuota(0, 0, 0, "", "test_user", "test_token", 0, "", "", "")
	require.NoError(t, err)
	assert.Equal(t, int64(actualQuota), stat.Quota)
	assert.Equal(t, 1, stat.Rpm) // 补扣/退款日志不计为一次新请求

	assert.Equal(t, int64(initQuota-actualQuota), getUserQuota(t, userID))
}

// 场景3：任务失败全额退款，最终净消费为 0，请求数仍为 1。
func TestTaskNetUsage_FullRefund(t *testing.T) {
	truncate(t)
	enableDataExport(t)
	ctx := context.Background()

	const userID, tokenID, channelID = 52, 52, 52
	const initQuota, preConsumed = 100000, 5000

	seedUser(t, userID, initQuota)
	seedToken(t, tokenID, userID, "sk-net-refund", 100000)
	seedChannel(t, channelID)

	task := simulateTaskSubmit(t, userID, channelID, tokenID, preConsumed)
	assert.True(t, RefundTaskQuota(ctx, task, "upstream failed"))

	assert.Equal(t, int64(0), getUserUsedQuota(t, userID))
	assert.Equal(t, 1, getUserRequestCount(t, userID))
	assert.EqualValues(t, 0, getChannelUsedQuota(t, channelID))

	flushQuotaData(t)
	count, quota := sumQuotaDataByUser(t, userID)
	assert.Equal(t, 1, count)
	assert.Equal(t, int64(0), quota)

	stat, err := model.SumUsedQuota(0, 0, 0, "", "test_user", "test_token", 0, "", "", "")
	require.NoError(t, err)
	assert.Equal(t, int64(0), stat.Quota)

	// 钱包全额退回
	assert.Equal(t, int64(initQuota), getUserQuota(t, userID))
}

// 场景4：重复执行结算/退款不产生二次冲减、二次退款或二次请求计数。
func TestTaskBilling_Idempotent_RepeatSettleAndRefund(t *testing.T) {
	truncate(t)
	enableDataExport(t)
	ctx := context.Background()

	const userID, tokenID, channelID = 53, 53, 53
	const initQuota, preConsumed, actualQuota = 100000, 5000, 1000

	seedUser(t, userID, initQuota)
	seedToken(t, tokenID, userID, "sk-net-idem", 100000)
	seedChannel(t, channelID)

	task := simulateTaskSubmit(t, userID, channelID, tokenID, preConsumed)
	simulateTaskSettle(ctx, task, actualQuota)

	// 重复结算：task.Quota 已是 actualQuota，delta=0，直接返回
	beforeUsed := getUserUsedQuota(t, userID)
	beforeReq := getUserRequestCount(t, userID)
	beforeLogs := countLogs(t)
	simulateTaskSettle(ctx, task, actualQuota)
	assert.Equal(t, beforeUsed, getUserUsedQuota(t, userID))
	assert.Equal(t, beforeReq, getUserRequestCount(t, userID))
	assert.Equal(t, beforeLogs, countLogs(t))

	// 全额退款后重复退款：task.Quota=0，直接返回，不重复冲减
	// （此时累计消耗 = task1 净消费 1000 + task2 预扣 5000 = 6000）
	task2 := simulateTaskSubmit(t, userID, channelID, tokenID, preConsumed)
	assert.Equal(t, int64(preConsumed+actualQuota), getUserUsedQuota(t, userID))
	assert.True(t, RefundTaskQuota(ctx, task2, "failed"))
	// task2 全额退款后只剩 task1 的净消费 1000
	assert.Equal(t, int64(actualQuota), getUserUsedQuota(t, userID))
	assert.Equal(t, 2, getUserRequestCount(t, userID))
	assert.True(t, RefundTaskQuota(ctx, task2, "failed again"))
	assert.Equal(t, int64(actualQuota), getUserUsedQuota(t, userID)) // 未二次冲减
	assert.Equal(t, 2, getUserRequestCount(t, userID))
}

// 场景5：BatchUpdateEnabled=true 时，差额结算/退款只入队不直写，
// 批量落库后口径与直写一致（模型层 TestUpdateUserUsedQuota_BatchMode 验证落库结果）。
func TestTaskBilling_BatchEnabled_EnqueuesDeltas(t *testing.T) {
	truncate(t)
	ctx := context.Background()

	old := common.BatchUpdateEnabled
	common.BatchUpdateEnabled = true
	t.Cleanup(func() { common.BatchUpdateEnabled = old })

	const userID, tokenID, channelID = 54, 54, 54
	const initQuota, preConsumed, actualQuota = 100000, 5000, 1000

	seedUser(t, userID, initQuota)
	seedToken(t, tokenID, userID, "sk-net-batch", 100000)
	seedChannel(t, channelID)

	task := simulateTaskSubmit(t, userID, channelID, tokenID, preConsumed)
	simulateTaskSettle(ctx, task, actualQuota)

	// 批量模式下差额结算只入队：used_quota 尚未落库
	assert.Equal(t, int64(0), getUserUsedQuota(t, userID))
	assert.Equal(t, 0, getUserRequestCount(t, userID))
}

// ===========================================================================
// ApplyTaskQuotaDelta 内存污染回归测试
// ===========================================================================

// 修复点：事务闭包内旧实现直接 task.Quota += delta，第 1~3 步成功但第 4 步
// SQL/提交失败时，数据库整体回滚而 Go 对象已被污染；随后恢复路径（sweep/轮询
// 退款失败回退状态）调用 task.Update() 会把错误额度写回数据库。
// 本测试用 SQLite 触发器精确注入"第 4 步 UPDATE 失败"，验证内存 Quota 保持原值。
func TestApplyTaskQuotaDelta_Step4FailureKeepsInMemoryQuota(t *testing.T) {
	truncate(t)
	const userID, preConsumed = 60, 1200
	seedUser(t, userID, 5000)
	seedUsedQuota(t, userID, 0, preConsumed) // 让第 1~3 步全部成功，故障点精确落在第 4 步

	task := makeTask(userID, 0, preConsumed, 0, BillingSourceWallet, 0)
	task.Status = model.TaskStatus(model.TaskStatusInProgress)
	require.NoError(t, model.DB.Create(task).Error)

	// 注入：任务额度 UPDATE 强制失败（模拟第 4 步 SQL 失败或最终提交失败）
	require.NoError(t, model.DB.Exec(`CREATE TRIGGER fail_task_quota_update
		BEFORE UPDATE OF quota ON tasks
		BEGIN
			SELECT RAISE(ABORT, 'injected quota update failure');
		END`).Error)
	t.Cleanup(func() {
		_ = model.DB.Exec("DROP TRIGGER IF EXISTS fail_task_quota_update").Error
	})

	assert.False(t, model.ApplyTaskQuotaDelta(task, -preConsumed, false))
	// 内存对象必须保持原值（旧实现此处已被污染为 0）
	assert.Equal(t, int64(preConsumed), task.Quota, "事务失败后内存 task.Quota 必须保持原值")
	// 数据库事务整体回滚，额度未变
	assert.Equal(t, int64(preConsumed), getTaskQuota(t, task.ID))

	// 移除注入后模拟"状态恢复路径"：回退状态并 task.Update()，不得写回错误额度
	require.NoError(t, model.DB.Exec("DROP TRIGGER fail_task_quota_update").Error)
	task.Progress = "0%"
	require.NoError(t, task.Update())
	assert.Equal(t, int64(preConsumed), getTaskQuota(t, task.ID), "恢复路径不得把污染额度写回数据库")
}

// 成功路径的对照：事务提交成功后内存 Quota 与数据库一致地更新为新值。
func TestApplyTaskQuotaDelta_SuccessUpdatesMemoryQuota(t *testing.T) {
	truncate(t)
	const userID, preConsumed = 61, 1200
	seedUser(t, userID, 5000)
	seedUsedQuota(t, userID, 0, preConsumed)

	task := makeTask(userID, 0, preConsumed, 0, BillingSourceWallet, 0)
	task.Status = model.TaskStatus(model.TaskStatusInProgress)
	require.NoError(t, model.DB.Create(task).Error)

	assert.True(t, model.ApplyTaskQuotaDelta(task, -preConsumed, false))
	assert.Equal(t, int64(0), task.Quota, "事务成功后内存 task.Quota 应与数据库一致（清零）")
	assert.Equal(t, int64(0), getTaskQuota(t, task.ID))
}

// ===========================================================================
// 统计写入失败（BillingStatsFailed）机制测试
// ===========================================================================

// Fix P2#4：ApplyPreConsumeUsedQuota 必须校验行存在性——用户/渠道不存在时
// GORM UPDATE 无错误但影响 0 行，若放行会插入任务但累计值缺失，退款守卫永久失败。
func TestApplyPreConsumeUsedQuota_MissingUserReturnsError(t *testing.T) {
	truncate(t)
	err := model.ApplyPreConsumeUsedQuota(999999, 0, 100)
	require.Error(t, err, "用户不存在时预扣累计消耗必须报错")
	assert.Contains(t, err.Error(), "user not found")
}

func TestApplyPreConsumeUsedQuota_MissingChannelReturnsError(t *testing.T) {
	truncate(t)
	seedUser(t, 81, 1000)
	err := model.ApplyPreConsumeUsedQuota(81, 8888, 100) // 渠道 8888 不存在
	require.Error(t, err, "渠道不存在时预扣累计消耗必须报错")
	assert.Contains(t, err.Error(), "channel not found")
	// 整体回滚：用户累计值未写入
	assert.Equal(t, int64(0), getUserUsedQuota(t, 81))
}

// Fix P1#2：统计写入失败的任务（BillingStatsFailed=true）退款方向必须跳过
// 累计消耗冲减——used_quota 从未累加，否则 used_quota >= refund 守卫永久卡死。
func TestApplyTaskQuotaDelta_BillingStatsFailedSkipsUsageRefund(t *testing.T) {
	truncate(t)
	const userID, preConsumed = 82, 1200
	seedUser(t, userID, 5000)
	// 不 seed used_quota：模拟统计从未写入

	task := makeTask(userID, 0, preConsumed, 0, BillingSourceWallet, 0)
	task.PrivateData.BillingStatsFailed = true
	require.NoError(t, model.DB.Create(task).Error)

	assert.True(t, model.ApplyTaskQuotaDelta(task, -preConsumed, false))
	assert.Equal(t, int64(0), task.Quota, "退款后任务额度清零")
	assert.Equal(t, int64(5000+preConsumed), getUserQuota(t, userID), "资金必须退还")
	assert.Equal(t, int64(0), getUserUsedQuota(t, userID), "统计未写入的任务退款不得冲减 used_quota")
}

// 对照：统计正常写入（BillingStatsFailed=false）且 used_quota 不足时，退款必须
// 被守卫拒绝（保留原额度可重试），不能绕过守卫。
func TestApplyTaskQuotaDelta_StatsRecordedRequiresUsedQuota(t *testing.T) {
	truncate(t)
	const userID, preConsumed = 83, 1200
	seedUser(t, userID, 5000)

	task := makeTask(userID, 0, preConsumed, 0, BillingSourceWallet, 0)
	require.NoError(t, model.DB.Create(task).Error)

	assert.False(t, model.ApplyTaskQuotaDelta(task, -preConsumed, false), "统计已写入的任务退款必须受 used_quota 守卫约束")
	assert.Equal(t, int64(preConsumed), task.Quota, "守卫失败保留原额度")
	assert.Equal(t, int64(5000), getUserQuota(t, userID), "资金未退还")
}

// Fix P1#3：Midjourney 统计写入失败的任务（statsRecorded=false）退款必须跳过
// 累计消耗冲减，资金照常退还。
func TestApplyWalletRefundUsedQuota_SkipsStatsWhenNotRecorded(t *testing.T) {
	truncate(t)
	const userID, channelID, refund = 84, 84, 1200
	seedUser(t, userID, 5000)
	seedChannel(t, channelID)
	// 不 seed used_quota：statsRecorded=false → 跳过冲减

	assert.True(t, model.ApplyWalletRefundUsedQuota(userID, channelID, refund, false))
	assert.Equal(t, int64(5000+refund), getUserQuota(t, userID), "资金必须退还")
	assert.Equal(t, int64(0), getUserUsedQuota(t, userID), "统计未写入时不得冲减 used_quota")

	// 对照：statsRecorded=true 且 used_quota 不足 → 守卫失败，资金不退还
	// （手工建用户：seedUser 的 aff_code 为空，重复建第二个用户会撞唯一约束）
	require.NoError(t, model.DB.Create(&model.User{Id: 85, Username: "test_user_85", Quota: 5000, Status: common.UserStatusEnabled, AffCode: "aff-85"}).Error)
	assert.False(t, model.ApplyWalletRefundUsedQuota(85, 0, refund, true))
	assert.Equal(t, int64(5000), getUserQuota(t, 85))
}

// Fix：BillingStatsFailed=true 且实际额度大于预扣额度（补扣方向 delta>0）时，
// 统计从未累加——补扣不能把差额写进 used_quota（会产生错误残值），资金照常补扣。
func TestApplyTaskQuotaDelta_BillingStatsFailedSkipsTopUp(t *testing.T) {
	truncate(t)
	const userID, preConsumed, actualQuota = 86, 1000, 1500
	seedUser(t, userID, 5000)

	task := makeTask(userID, 0, preConsumed, 0, BillingSourceWallet, 0)
	task.PrivateData.BillingStatsFailed = true
	require.NoError(t, model.DB.Create(task).Error)

	delta := int64(actualQuota) - int64(preConsumed) // +500 补扣
	assert.True(t, model.ApplyTaskQuotaDelta(task, delta, false))
	assert.Equal(t, int64(actualQuota), task.Quota, "任务额度更新为实际额度")
	assert.Equal(t, 5000-delta, getUserQuota(t, userID), "资金按差额补扣")
	assert.Equal(t, int64(0), getUserUsedQuota(t, userID), "统计未写入的任务补扣不得写 used_quota")
}
