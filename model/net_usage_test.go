package model

import (
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// UpdateUserUsedQuota：只调整 used_quota，不修改 request_count
// ---------------------------------------------------------------------------

func TestUpdateUserUsedQuota_DoesNotTouchRequestCount(t *testing.T) {
	truncateTables(t)
	const userID = 1001
	require.NoError(t, DB.Create(&User{Id: userID, Username: "net-usage", Quota: 10000, Status: common.UserStatusEnabled}).Error)

	// 预置 request_count
	DB.Model(&User{}).Where("id = ?", userID).Update("request_count", 5)

	common.BatchUpdateEnabled = false
	assert.True(t, UpdateUserUsedQuota(userID, 5000))  // 预扣
	assert.True(t, UpdateUserUsedQuota(userID, -4000)) // 差额退款

	var u User
	require.NoError(t, DB.Select("used_quota, request_count").Where("id = ?", userID).First(&u).Error)
	assert.Equal(t, int64(1000), u.UsedQuota)
	assert.Equal(t, 5, u.RequestCount)

	// 0 增量：无副作用
	assert.True(t, UpdateUserUsedQuota(userID, 0))
	require.NoError(t, DB.Select("used_quota").Where("id = ?", userID).First(&u).Error)
	assert.Equal(t, int64(1000), u.UsedQuota)
}

func TestUpdateUserUsedQuota_BatchMode(t *testing.T) {
	truncateTables(t)
	const userID = 1002
	require.NoError(t, DB.Create(&User{Id: userID, Username: "net-usage-batch", Quota: 10000, Status: common.UserStatusEnabled}).Error)
	require.NoError(t, DB.Create(&Channel{Id: 2002, Name: "test_channel", Key: "sk-test", Status: common.ChannelStatusEnabled}).Error)

	common.BatchUpdateEnabled = true
	t.Cleanup(func() { common.BatchUpdateEnabled = false })

	// 提交：预扣 + 请求计数；结算：差额退款（不计数）
	UpdateUserUsedQuotaAndRequestCount(userID, 5000)
	UpdateUserUsedQuota(userID, -4000)
	UpdateChannelUsedQuota(2002, 5000)
	UpdateChannelUsedQuota(2002, -4000)

	batchUpdate() // 批量落库

	var u User
	require.NoError(t, DB.Select("used_quota, request_count").Where("id = ?", userID).First(&u).Error)
	assert.Equal(t, int64(1000), u.UsedQuota)
	assert.Equal(t, 1, u.RequestCount)

	var ch Channel
	require.NoError(t, DB.Select("used_quota").Where("id = ?", 2002).First(&ch).Error)
	assert.EqualValues(t, 1000, ch.UsedQuota)
}

func TestUpdateUserUsedQuota_RefundGuard(t *testing.T) {
	truncateTables(t)
	const userID = 1003
	require.NoError(t, DB.Create(&User{Id: userID, Username: "net-usage-guard", Quota: 10000, UsedQuota: 100, Status: common.UserStatusEnabled}).Error)

	common.BatchUpdateEnabled = false

	// 退款超过累计消耗：非批量模式守卫生效，拒绝冲减并保持原值
	assert.False(t, UpdateUserUsedQuota(userID, -200))

	var u User
	require.NoError(t, DB.Select("used_quota").Where("id = ?", userID).First(&u).Error)
	assert.Equal(t, int64(100), u.UsedQuota)

	// 冲减不超过累计消耗：正常执行
	assert.True(t, UpdateUserUsedQuota(userID, -60))
	require.NoError(t, DB.Select("used_quota").Where("id = ?", userID).First(&u).Error)
	assert.Equal(t, int64(40), u.UsedQuota)
}

// ---------------------------------------------------------------------------
// SumUsedQuota：净消费 = 消费 - 退款；RPM 不统计计费调整日志
// ---------------------------------------------------------------------------

func TestSumUsedQuota_NetAndRpm(t *testing.T) {
	truncateTables(t)
	now := time.Now().Unix()

	// 提交消费日志（真实请求）：预扣 5000
	require.NoError(t, LOG_DB.Create(&Log{
		UserId: 1004, Username: "net", CreatedAt: now, Type: LogTypeConsume,
		ModelName: "seedance-01", Quota: 5000, TokenName: "tok", ChannelId: 2004, Group: "default",
	}).Error)
	// 差额退款日志：4000
	require.NoError(t, LOG_DB.Create(&Log{
		UserId: 1004, Username: "net", CreatedAt: now, Type: LogTypeRefund,
		ModelName: "seedance-01", Quota: 4000, TokenName: "tok", ChannelId: 2004, Group: "default",
		Other: `{"task_id":"t1","pre_consumed_quota":5000}`,
	}).Error)
	// 差额补扣日志（计费调整，不算新请求）：+2000
	require.NoError(t, LOG_DB.Create(&Log{
		UserId: 1004, Username: "net", CreatedAt: now, Type: LogTypeConsume,
		ModelName: "seedance-01", Quota: 2000, TokenName: "tok", ChannelId: 2004, Group: "default",
		Other: `{"task_id":"t2","pre_consumed_quota":1000}`,
	}).Error)

	stat, err := SumUsedQuota(LogTypeUnknown, 0, 0, "seedance-01", "net", "tok", 2004, "default", "", "")
	require.NoError(t, err)
	// 净消费 = 5000 + 2000 - 4000 = 3000
	assert.Equal(t, int64(3000), stat.Quota)
	// RPM 只统计真实请求（提交日志 1 条），补扣/退款不计
	assert.Equal(t, 1, stat.Rpm)
	assert.Equal(t, 0, stat.Tpm)
}

// ---------------------------------------------------------------------------
// quota_data：Adjustment 记录 Count=0、带符号额度
// ---------------------------------------------------------------------------

func clearQuotaDataCache(t *testing.T) {
	t.Helper()
	CacheQuotaDataLock.Lock()
	CacheQuotaData = make(map[string]*QuotaData)
	CacheQuotaDataLock.Unlock()
}

func TestLogQuotaData_AdjustmentCountZero(t *testing.T) {
	truncateTables(t)
	clearQuotaDataCache(t)

	LogQuotaData(QuotaDataLogParams{
		UserID: 1005, Username: "net", ModelName: "seedance-01", Quota: 5000,
		CreatedAt: time.Now().Unix(), UseGroup: "default", ChannelID: 2005,
	})
	LogQuotaData(QuotaDataLogParams{
		UserID: 1005, Username: "net", ModelName: "seedance-01", Quota: -4000,
		CreatedAt: time.Now().Unix(), UseGroup: "default", ChannelID: 2005, Adjustment: true,
	})
	SaveQuotaDataCache()

	rows, err := GetQuotaDataByUserId(1005, 0, time.Now().Add(time.Hour).Unix())
	require.NoError(t, err)
	require.Len(t, rows, 1)
	// 聚合：count = 1 + 0 = 1；quota = 5000 - 4000 = 1000
	assert.Equal(t, 1, rows[0].Count)
	assert.Equal(t, int64(1000), rows[0].Quota)
}

func TestRecordTaskBillingLog_WritesQuotaDataAdjustment(t *testing.T) {
	truncateTables(t)
	clearQuotaDataCache(t)

	oldExport := common.DataExportEnabled
	common.DataExportEnabled = true
	t.Cleanup(func() { common.DataExportEnabled = oldExport })

	require.NoError(t, DB.Create(&User{Id: 1006, Username: "net-task", Quota: 10000, Status: common.UserStatusEnabled}).Error)

	// 差额补扣：Count=0、Quota=+2000
	RecordTaskBillingLog(RecordTaskBillingLogParams{
		UserId: 1006, LogType: LogTypeConsume, Content: "adaptor计费调整",
		ChannelId: 2006, ModelName: "seedance-01", Quota: 2000, Group: "default",
		Other: map[string]interface{}{"task_id": "t1", "pre_consumed_quota": 5000},
	})
	// 差额退款：Count=0、Quota=-4000
	RecordTaskBillingLog(RecordTaskBillingLogParams{
		UserId: 1006, LogType: LogTypeRefund, Content: "",
		ChannelId: 2006, ModelName: "seedance-01", Quota: 4000, Group: "default",
		Other: map[string]interface{}{"task_id": "t2", "pre_consumed_quota": 5000},
	})
	SaveQuotaDataCache()

	rows, err := GetQuotaDataByUserId(1006, 0, time.Now().Add(time.Hour).Unix())
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, 0, rows[0].Count)     // 调整记录不增加请求计数
	assert.Equal(t, int64(-2000), rows[0].Quota) // +2000 - 4000
}

// ---------------------------------------------------------------------------
// ApplyTaskQuotaDelta：资金 + 用户/渠道累计消耗 + 任务额度在同一事务内原子调整
// ---------------------------------------------------------------------------

func TestApplyTaskQuotaDelta_AtomicAndGuards(t *testing.T) {
	truncateTables(t)
	const userID, channelID = 1007, 2007
	require.NoError(t, DB.Create(&User{Id: userID, Username: "net-atomic", Quota: 10000, UsedQuota: 5000, Status: common.UserStatusEnabled}).Error)
	require.NoError(t, DB.Create(&Channel{Id: channelID, Name: "ch-atomic", Key: "sk-atomic", UsedQuota: 5000, Status: common.ChannelStatusEnabled}).Error)

	task := &Task{TaskID: "t-atomic", UserId: userID, ChannelId: channelID, Quota: 5000, Status: TaskStatusInProgress}
	require.NoError(t, DB.Create(task).Error)

	readAll := func() (userQuota, userUsed, channelUsed, taskQuota int64) {
		var u User
		require.NoError(t, DB.Select("quota, used_quota").Where("id = ?", userID).First(&u).Error)
		var ch Channel
		require.NoError(t, DB.Select("used_quota").Where("id = ?", channelID).First(&ch).Error)
		var tk Task
		require.NoError(t, DB.Select("quota").Where("id = ?", task.ID).First(&tk).Error)
		return u.Quota, u.UsedQuota, ch.UsedQuota, tk.Quota
	}

	// 退款：资金 + 用户/渠道累计消耗 + 任务额度原子调整
	assert.True(t, ApplyTaskQuotaDelta(task, -3000, false))
	q, uq, cuq, tq := readAll()
	assert.Equal(t, int64(13000), q)        // 钱包 +3000
	assert.Equal(t, int64(2000), uq)        // 用户累计 5000-3000
	assert.EqualValues(t, 2000, cuq) // 渠道累计 5000-3000
	assert.Equal(t, int64(2000), tq)        // 任务剩余 5000-3000

	// 用户守卫失败：整体回滚（用户累计 2000 不足以冲减 5000）
	assert.False(t, ApplyTaskQuotaDelta(task, -5000, false))
	q, uq, cuq, tq = readAll()
	assert.Equal(t, int64(13000), q)
	assert.Equal(t, int64(2000), uq)
	assert.EqualValues(t, 2000, cuq)
	assert.Equal(t, int64(2000), tq)
}

func TestApplyTaskQuotaDelta_ChannelGuard(t *testing.T) {
	truncateTables(t)
	const userID, channelID = 1009, 2009
	// 用户累计充足，但渠道累计不足
	require.NoError(t, DB.Create(&User{Id: userID, Username: "net-ch-guard", Quota: 10000, UsedQuota: 5000, Status: common.UserStatusEnabled}).Error)
	require.NoError(t, DB.Create(&Channel{Id: channelID, Name: "ch-guard", Key: "sk-guard", UsedQuota: 100, Status: common.ChannelStatusEnabled}).Error)

	task := &Task{TaskID: "t-ch-guard", UserId: userID, ChannelId: channelID, Quota: 5000, Status: TaskStatusInProgress}
	require.NoError(t, DB.Create(task).Error)

	// 退款 5000：用户累计 5000 足够，但渠道累计 100 不足 → 渠道守卫失败，整体回滚
	assert.False(t, ApplyTaskQuotaDelta(task, -5000, false))

	var u User
	require.NoError(t, DB.Select("quota, used_quota").Where("id = ?", userID).First(&u).Error)
	assert.Equal(t, int64(10000), u.Quota)
	assert.Equal(t, int64(5000), u.UsedQuota)
	var ch Channel
	require.NoError(t, DB.Select("used_quota").Where("id = ?", channelID).First(&ch).Error)
	assert.EqualValues(t, 100, ch.UsedQuota)
	var tk Task
	require.NoError(t, DB.Select("quota").Where("id = ?", task.ID).First(&tk).Error)
	assert.Equal(t, int64(5000), tk.Quota)
}
