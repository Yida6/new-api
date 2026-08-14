package controller

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/constant"
	taskdto "github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/model"
	relaycommon "github.com/QuantumNous/new-api/relay/common"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Seedance 并发上限 429 响应体：HTTP 429 + 明确错误码 + 指定文案。
// 前端据此展示提示（含当前运行数/上限），不得自动重复提交。
func TestRespondTaskConcurrencyLimit(t *testing.T) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)

	respondTaskConcurrencyLimit(c, 2, 3)

	require.Equal(t, http.StatusTooManyRequests, w.Code, "必须返回 HTTP 429")
	assert.Equal(t, "application/json; charset=utf-8", w.Header().Get("Content-Type"))

	var body taskdto.TaskError
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	assert.Equal(t, "SEEDANCE_CONCURRENCY_LIMIT_EXCEEDED", body.Code, "必须携带明确错误码")
	// 注：TaskError.StatusCode 带 json:"-" 不参与序列化，HTTP 状态由上面的 w.Code 断言保证。
	assert.Contains(t, body.Message, "你当前已有 2 个 Seedance 任务正在运行")
	assert.Contains(t, body.Message, "最多可同时运行 3 个")
	assert.Contains(t, body.Message, "请等待任务完成或取消任务后再试")
}

// 文案中的 current/limit 随实参变化（确保不是硬编码）。
func TestRespondTaskConcurrencyLimitParams(t *testing.T) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)

	respondTaskConcurrencyLimit(c, 5, 5)

	var body taskdto.TaskError
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	assert.Contains(t, body.Message, "你当前已有 5 个 Seedance 任务正在运行")
	assert.Contains(t, body.Message, "最多可同时运行 5 个")
}

// outcome_unknown 恢复记录必须持有并发名额（上游可能已创建任务）：
// 已预留名额（reservedSlot=true）→ persistOutcomeUnknownRecovery 返回 true 且
// 记录 ConcurrencyReserved=true；未预留（reservedSlot=false）→ 不标记。
func TestPersistOutcomeUnknownRecoveryHoldsSlot(t *testing.T) {
	initRecoveryControllerTestDB(t)
	gin.SetMode(gin.TestMode)

	newCtx := func() *gin.Context {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/videos", strings.NewReader(`{"model":"m","prompt":"p"}`))
		c.Request.Header.Set("Content-Type", "application/json")
		c.Set(string(constant.ContextKeyUserId), 1)
		c.Set("channel_type", 54)
		return c
	}
	info := &relaycommon.RelayInfo{
		UserId:          1,
		OriginModelName: "doubao-seedance-2-0-260128",
		ChannelMeta:     &relaycommon.ChannelMeta{ChannelType: 54},
		TaskRelayInfo:   &relaycommon.TaskRelayInfo{PublicTaskID: "task_x"},
	}
	taskErr := &taskdto.TaskError{Error: errors.New("read tcp 1.2.3.4:443: connection reset")}

	// 已预留名额 → 恢复记录持有名额
	held := persistOutcomeUnknownRecovery(newCtx(), info, "fp", taskErr, true)
	assert.True(t, held, "已预留名额的 outcome_unknown 必须由恢复记录持有名额")
	var recs []model.TaskSubmitRecovery
	require.NoError(t, model.DB.Find(&recs).Error)
	require.Len(t, recs, 1)
	assert.True(t, recs[0].ConcurrencyReserved, "恢复记录必须标记占名额")

	// 未预留名额（如未启用限制）→ 不标记
	held2 := persistOutcomeUnknownRecovery(newCtx(), info, "fp2", taskErr, false)
	assert.False(t, held2)
	require.NoError(t, model.DB.Find(&recs).Error)
	require.Len(t, recs, 2)
	assert.False(t, recs[1].ConcurrencyReserved)
}
