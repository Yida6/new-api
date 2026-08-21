package relay

import (
	"net/http/httptest"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	relaycommon "github.com/QuantumNous/new-api/relay/common"

	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// Fix P2#5：Midjourney 任务的 ConsumeLogRecorded 必须反映 RecordConsumeLog 的
// 实际写入结果，而不是全局开关 common.LogConsumeEnabled。
func TestConsumeMjTaskBilling_LogMarkingReflectsReality(t *testing.T) {
	origDB, origLOGDB := model.DB, model.LOG_DB
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	model.DB = db
	model.LOG_DB = db
	// 默认 RedisEnabled=true 且 RDB 为 nil，测试必须显式关闭并恢复
	oldRedisEnabled := common.RedisEnabled
	common.RedisEnabled = false
	t.Cleanup(func() {
		model.DB, model.LOG_DB = origDB, origLOGDB
		common.RedisEnabled = oldRedisEnabled
	})
	require.NoError(t, db.AutoMigrate(&model.User{}, &model.Log{}))

	user := &model.User{Id: 900, Username: "mjuser", Quota: 10000, Status: common.UserStatusEnabled}
	require.NoError(t, model.DB.Create(user).Error)

	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Set("username", "mjuser")
	c.Set("token_name", "tk")

	info := &relaycommon.RelayInfo{
		UserId:       900,
		IsPlayground: true, // 跳过令牌额度操作
		UsingGroup:   "default",
		ChannelMeta:  &relaycommon.ChannelMeta{ChannelId: 0}, // ChannelId=0：不维护渠道累计消耗
	}

	oldLogEnabled := common.LogConsumeEnabled
	t.Cleanup(func() { common.LogConsumeEnabled = oldLogEnabled })

	// 日志开关开启 + 日志库正常 → 日志实际写入，返回 true
	common.LogConsumeEnabled = true
	recorded, err := consumeMjTaskBilling(c, info, 500, "log content", map[string]interface{}{"k": "v"}, "mj-model")
	require.NoError(t, err)
	assert.True(t, recorded, "日志写入成功时 ConsumeLogRecorded 必须为 true")

	var reloaded model.User
	require.NoError(t, model.DB.First(&reloaded, 900).Error)
	assert.Equal(t, int64(9500), reloaded.Quota, "钱包扣款 500")
	assert.Equal(t, int64(500), reloaded.UsedQuota, "累计消耗写入")
	assert.Equal(t, 1, reloaded.RequestCount)

	// 日志开关关闭 → 日志未写入，返回 false（任务行的标记必须跟随真实结果）
	common.LogConsumeEnabled = false
	recorded, err = consumeMjTaskBilling(c, info, 300, "log2", nil, "mj-model")
	require.NoError(t, err)
	assert.False(t, recorded, "日志未写入时 ConsumeLogRecorded 必须为 false")

	require.NoError(t, model.DB.First(&reloaded, 900).Error)
	assert.Equal(t, int64(9200), reloaded.Quota)
	assert.Equal(t, int64(800), reloaded.UsedQuota, "累计消耗与日志开关无关，仍按扣款累计")
	assert.Equal(t, 2, reloaded.RequestCount)
}

// Fix（补充场景）：SwapFace 与 RelayMidjourneySubmit 共用的标记逻辑——统计写入
// 失败必须置 BillingStatsFailed=true（此前 SwapFace 分支漏置位，导致该任务失败
// 退款时 used_quota 守卫死锁）。本测试直接驱动 applyMjBillingAndMark 验证两条
// 分支：用户不存在 → 统计失败 → 置位；用户存在 → 统计正常 → 不置位。
func TestApplyMjBillingAndMark_SetsBillingStatsFailedOnStatsError(t *testing.T) {
	origDB, origLOGDB := model.DB, model.LOG_DB
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	model.DB = db
	model.LOG_DB = db
	oldRedisEnabled := common.RedisEnabled
	common.RedisEnabled = false
	t.Cleanup(func() {
		model.DB, model.LOG_DB = origDB, origLOGDB
		common.RedisEnabled = oldRedisEnabled
	})
	require.NoError(t, db.AutoMigrate(&model.User{}, &model.Log{}, &model.Midjourney{}))

	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Set("username", "mjuser")
	c.Set("token_name", "tk")

	oldLogEnabled := common.LogConsumeEnabled
	common.LogConsumeEnabled = true
	t.Cleanup(func() { common.LogConsumeEnabled = oldLogEnabled })

	// 统计失败分支：用户不存在 → ApplyPreConsumeUsedQuota 报错 → 标记置位
	info := &relaycommon.RelayInfo{
		UserId:       777777,
		IsPlayground: true,
		UsingGroup:   "default",
		ChannelMeta:  &relaycommon.ChannelMeta{ChannelId: 0},
	}
	task := &model.Midjourney{UserId: 777777, Quota: 100}
	applyMjBillingAndMark(c, info, task, 100, "log", nil, "mj-model")
	assert.True(t, task.BillingStatsFailed, "统计写入失败必须置 BillingStatsFailed=true")

	// 统计正常分支：用户存在 → 不置位，消费日志实际写入
	user := &model.User{Id: 888, Username: "mjuser", Quota: 10000, Status: common.UserStatusEnabled}
	require.NoError(t, model.DB.Create(user).Error)
	okInfo := &relaycommon.RelayInfo{
		UserId:       888,
		IsPlayground: true,
		UsingGroup:   "default",
		ChannelMeta:  &relaycommon.ChannelMeta{ChannelId: 0},
	}
	task2 := &model.Midjourney{UserId: 888, Quota: 100}
	applyMjBillingAndMark(c, okInfo, task2, 100, "log2", nil, "mj-model")
	assert.False(t, task2.BillingStatsFailed, "统计正常时不得置位")
	assert.True(t, task2.ConsumeLogRecorded, "日志写入成功时标记必须为 true")

	// 落库后再读回，标记持久化
	require.NoError(t, model.DB.Create(task).Error)
	var reloaded model.Midjourney
	require.NoError(t, model.DB.First(&reloaded, task.Id).Error)
	assert.True(t, reloaded.BillingStatsFailed, "标记必须持久化到数据库")
}
