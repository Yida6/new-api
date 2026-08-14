package controller

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/constant"
	taskdto "github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/relay"
	relaycommon "github.com/QuantumNous/new-api/relay/common"

	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func initRecoveryControllerTestDB(t *testing.T) {
	t.Helper()
	origDB := model.DB
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	model.DB = db
	t.Cleanup(func() { model.DB = origDB })
	require.NoError(t, db.AutoMigrate(&model.TaskSubmitRecovery{}, &model.TaskSubmitLockRow{}))
}

func newRecoveryCtrlCtx(t *testing.T, userID int, path, body string) (*gin.Context, *httptest.ResponseRecorder) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Set(string(constant.ContextKeyUserId), userID)
	c.Params = gin.Params{{Key: "id", Value: "1"}}
	return c, w
}

func insertRecoveryForTest(t *testing.T, userID int, status string) int64 {
	t.Helper()
	rec := &model.TaskSubmitRecovery{
		UserId:      userID,
		Platform:    "54",
		Model:       "m",
		Outcome:     "outcome_unknown",
		Status:      status,
		UpstreamTaskID: "",
	}
	require.NoError(t, rec.Insert())
	return rec.ID
}

// 人工重试必须显式确认（confirm=true），否则拒绝。
func TestRecreateRequiresExplicitConfirm(t *testing.T) {
	initRecoveryControllerTestDB(t)
	insertRecoveryForTest(t, 1, model.TaskRecoveryStatusUnknown)

	c, w := newRecoveryCtrlCtx(t, 1, "/api/user/task_recovery/1/recreate", `{"confirm":false,"model":"m","prompt":"p"}`)
	RecreateTaskRecovery(c)

	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "confirm")
	assert.Contains(t, w.Body.String(), "重复任务")
}

// 已关联上游任务的记录不允许重新创建（必然产生重复任务）。
func TestRecreateRejectedWhenAssociated(t *testing.T) {
	initRecoveryControllerTestDB(t)
	insertRecoveryForTest(t, 1, model.TaskRecoveryStatusAssociated)

	c, w := newRecoveryCtrlCtx(t, 1, "/api/user/task_recovery/1/recreate", `{"confirm":true,"model":"m","prompt":"p"}`)
	RecreateTaskRecovery(c)

	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "关联")
}

// 已被其他请求原子占位（recreated）后，再次人工重试必须拒绝（并发/双击只有一个能进入创建流程）。
func TestRecreateRejectedWhenAlreadyClaimed(t *testing.T) {
	initRecoveryControllerTestDB(t)
	insertRecoveryForTest(t, 1, model.TaskRecoveryStatusRecreated)

	c, w := newRecoveryCtrlCtx(t, 1, "/api/user/task_recovery/1/recreate", `{"confirm":true,"model":"m","prompt":"p"}`)
	RecreateTaskRecovery(c)

	assert.Equal(t, http.StatusConflict, w.Code)
	assert.Contains(t, w.Body.String(), "请勿重复操作")
}

// 人工重试即使后续失败（此处模拟渠道选择失败），父恢复记录也必须被回填，
// recreated 状态不会永久悬空；且失败且无子记录时记录被重新打开为 unknown，
// 保留可执行的恢复路径（可再次关联/候选发现/重试）。
func TestRecreateBackfillsParentOnFailure(t *testing.T) {
	initRecoveryControllerTestDB(t)
	id := insertRecoveryForTest(t, 1, model.TaskRecoveryStatusUnknown)

	c, w := newRecoveryCtrlCtx(t, 1, "/api/user/task_recovery/1/recreate", `{"confirm":true,"model":"m","prompt":"p"}`)
	c.Set("token_group", "default")
	RecreateTaskRecovery(c)

	// RelayTask 会进入创建流程但渠道选择失败 → 500；父记录必须被回填并重新打开
	got, err := model.GetTaskSubmitRecoveryByID(id, 1)
	require.NoError(t, err)
	assert.Equal(t, model.TaskRecoveryStatusUnknown, got.Status,
		"明确失败且无子记录时，父记录必须重新打开为 unknown（可执行的恢复路径）")
	assert.Contains(t, got.Note, "重新打开", "回填备注必须说明可再次操作")
	_ = w
}

// 锁不可用（503 早退）时，父记录同样被回填并重新打开，不悬空。
func TestRecreateBackfillsParentWhenLockUnavailable(t *testing.T) {
	initRecoveryControllerTestDB(t)
	id := insertRecoveryForTest(t, 1, model.TaskRecoveryStatusUnknown)

	c, w := newRecoveryCtrlCtx(t, 1, "/api/user/task_recovery/1/recreate", `{"confirm":true,"model":"m","prompt":"p"}`)
	c.Set("token_group", "default")

	// 使提交锁后端故障（fail-closed 503）：删除锁表模拟后端不可用
	require.NoError(t, model.DB.Migrator().DropTable(&model.TaskSubmitLockRow{}))
	RecreateTaskRecovery(c)

	got, err := model.GetTaskSubmitRecoveryByID(id, 1)
	require.NoError(t, err)
	assert.Equal(t, model.TaskRecoveryStatusUnknown, got.Status, "锁 503 早退后父记录必须重新打开，不悬空")
	assert.Contains(t, got.Note, "重新打开")
	_ = w
}

// 关联验证期间被并发 recreate 占位：只允许一种操作成功（关联失败 409，记录保持 recreated）。
func TestAssociateRejectedWhenConcurrentlyRecreated(t *testing.T) {
	initRecoveryControllerTestDB(t)
	// 验证接口需要渠道 + mock 上游
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"cgt-x","status":"queued","content":{"video_url":""}}`)
	}))
	defer srv.Close()
	baseURL := strings.TrimSuffix(srv.URL, "/")
	require.NoError(t, model.DB.AutoMigrate(&model.Channel{}))
	ch := &model.Channel{Type: 54, Key: "k", Status: 1, Name: "test", BaseURL: &baseURL}
	require.NoError(t, model.DB.Create(ch).Error)

	rec := &model.TaskSubmitRecovery{
		UserId: 1, Platform: "54", Model: "m",
		ChannelId: ch.Id, ChannelType: 54,
		Outcome: "outcome_unknown", Status: model.TaskRecoveryStatusUnknown,
	}
	require.NoError(t, rec.Insert())

	// 模拟并发 recreate 已在验证期间原子占位
	claimed, err := model.MarkRecoveryRecreated(rec.ID, 1, "concurrent recreate claimed")
	require.NoError(t, err)
	require.True(t, claimed)

	c, w := newRecoveryCtrlCtx(t, 1, "/api/user/task_recovery/1/associate", `{"upstream_task_id":"cgt-x"}`)
	AssociateTaskRecovery(c)

	assert.Equal(t, http.StatusConflict, w.Code, "验证期间被并发 recreate 占位后，关联必须失败")
	got, err := model.GetTaskSubmitRecoveryByID(rec.ID, 1)
	require.NoError(t, err)
	assert.Equal(t, model.TaskRecoveryStatusRecreated, got.Status, "关联不得覆盖并发 recreate 写下的状态")
	assert.Empty(t, got.UpstreamTaskID, "关联失败时不得写入上游 task_id")
}

// 不同用户的记录不可见。
func TestGetTaskRecoveriesUserScoped(t *testing.T) {
	initRecoveryControllerTestDB(t)
	insertRecoveryForTest(t, 1, model.TaskRecoveryStatusUnknown)
	insertRecoveryForTest(t, 2, model.TaskRecoveryStatusUnknown)

	c, w := newRecoveryCtrlCtx(t, 1, "/api/user/task_recovery", "")
	c.Request.Method = http.MethodGet
	GetTaskRecoveries(c)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), `"total":1`)
	assert.NotContains(t, w.Body.String(), `"user_id":2`)
}

// GET /task_recovery 必须走专用 DTO：不暴露 ChannelBaseURL / 指纹 / 幂等键 /
// X-Client-Request-Id / UpstreamModelName（方舟 Endpoint ID）等内部字段，
// 只暴露恢复所需字段。
func TestGetTaskRecoveriesDTOExcludesInternalFields(t *testing.T) {
	initRecoveryControllerTestDB(t)
	rec := &model.TaskSubmitRecovery{
		UserId:             1,
		Platform:           "54",
		Model:              "doubao-seedance-2-0-260128",
		UpstreamModelName:  "ep-20250101-abc123",
		ChannelId:          42,
		ChannelType:        54,
		ChannelBaseURL:     "https://ark.example.com",
		IdempotencyKey:     "11111111-1111-4111-8111-111111111111",
		ClientRequestID:    "22222222-2222-4222-8222-222222222222",
		Fingerprint:        "9:8:deadbeef",
		ContentFingerprint: "cafebabe",
		Outcome:            "outcome_unknown",
		Status:             model.TaskRecoveryStatusUnknown,
		Note:               "outcome_unknown: timeout",
	}
	require.NoError(t, rec.Insert())

	c, w := newRecoveryCtrlCtx(t, 1, "/api/user/task_recovery", "")
	c.Request.Method = http.MethodGet
	GetTaskRecoveries(c)

	assert.Equal(t, http.StatusOK, w.Code)
	body := w.Body.String()
	// 恢复所需字段可见
	assert.Contains(t, body, `"model":"doubao-seedance-2-0-260128"`)
	assert.Contains(t, body, `"status":"unknown"`)
	assert.Contains(t, body, `"channel_type":54`)
	// 内部字段不得暴露
	assert.NotContains(t, body, "channel_base_url")
	assert.NotContains(t, body, "ark.example.com")
	assert.NotContains(t, body, "fingerprint")
	assert.NotContains(t, body, "deadbeef")
	assert.NotContains(t, body, "idempotency_key")
	assert.NotContains(t, body, "client_request_id")
	assert.NotContains(t, body, `"channel_id"`)
	// 敏感信息不得暴露：upstream_model_name 为方舟 Endpoint ID
	assert.NotContains(t, body, "upstream_model_name")
	assert.NotContains(t, body, "ep-20250101-abc123")
}

// 恢复记录对外响应（列表与单条）在任何情况下都不得携带 upstream_model_name，
// 且候选列表中的上游 model（可能回显方舟 Endpoint ID）必须脱敏。
func TestRecoveryDTONeverExposesUpstreamModelName(t *testing.T) {
	initRecoveryControllerTestDB(t)
	rec := &model.TaskSubmitRecovery{
		UserId:            1,
		Platform:          "54",
		Model:             "m",
		UpstreamModelName: "ep-20250101-abc123",
		Outcome:           "outcome_unknown",
		Status:            model.TaskRecoveryStatusUnknown,
		UpstreamTaskID:    "cgt-x",
		Note:              "n",
		Candidates:        `[{"upstream_task_id":"cgt-1","model":"ep-20250101-abc123","status":"queued","created_at":1700000000}]`,
	}
	require.NoError(t, rec.Insert())

	// 列表接口
	c, w := newRecoveryCtrlCtx(t, 1, "/api/user/task_recovery", "")
	c.Request.Method = http.MethodGet
	GetTaskRecoveries(c)
	assert.Equal(t, http.StatusOK, w.Code)
	assert.NotContains(t, w.Body.String(), "upstream_model_name")
	assert.NotContains(t, w.Body.String(), "ep-20250101-abc123")
	// 候选列表仍可见（脱敏后），业务可继续展示
	assert.Contains(t, w.Body.String(), "cgt-1")
	assert.Contains(t, w.Body.String(), "ep-***")

	// toRecoveryDTO 单元级验证（关联等接口复用的同一 DTO）
	dto := toRecoveryDTO(rec)
	raw, err := json.Marshal(dto)
	require.NoError(t, err)
	assert.NotContains(t, string(raw), "upstream_model_name")
	assert.NotContains(t, string(raw), "ep-20250101-abc123")
	// 恢复流程所需字段仍可见
	assert.Contains(t, string(raw), `"upstream_task_id":"cgt-x"`)
	assert.Contains(t, string(raw), `"model":"m"`)
}

// ---------------------------------------------------------------------------
// backfillRecoveryParent 全分支单元测试（恢复路径不变量）
// ---------------------------------------------------------------------------

func newBackfillInfo(userID int, outcome relaycommon.TaskSubmitOutcome) *relaycommon.RelayInfo {
	return &relaycommon.RelayInfo{
		UserId: userID,
		TaskRelayInfo: &relaycommon.TaskRelayInfo{
			PublicTaskID:  "task_new_1",
			SubmitOutcome: outcome,
		},
	}
}

func backfillTestCtx(t *testing.T) *gin.Context {
	t.Helper()
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Set(string(constant.ContextKeyUserId), 1)
	return c
}

// 场景 1-4：成功 / 结果未知(有子记录) / 结果未知但子记录落库失败 / 明确失败。
func TestBackfillRecoveryParentScenarios(t *testing.T) {
	initRecoveryControllerTestDB(t)
	c := backfillTestCtx(t)

	// 1) 成功创建任务 → 父保持 recreated，回填任务 ID
	p1 := insertRecoveryForTest(t, 1, model.TaskRecoveryStatusRecreated)
	backfillRecoveryParent(c, newBackfillInfo(1, relaycommon.TaskSubmitOutcomeConfirmedSuccess), p1,
		&relay.TaskSubmitResult{UpstreamTaskID: "cgt-ok"}, nil)
	g1, _ := model.GetTaskSubmitRecoveryByID(p1, 1)
	assert.Equal(t, model.TaskRecoveryStatusRecreated, g1.Status, "成功时父保持终态 recreated")
	assert.Contains(t, g1.Note, "cgt-ok")

	// 2) 结果未知 + 子记录已创建 → 父保持 recreated，恢复路径指向子记录
	p2 := insertRecoveryForTest(t, 1, model.TaskRecoveryStatusRecreated)
	child := &model.TaskSubmitRecovery{UserId: 1, Platform: "54", Model: "m", ParentID: p2, Outcome: "outcome_unknown", Status: model.TaskRecoveryStatusUnknown}
	require.NoError(t, child.Insert())
	backfillRecoveryParent(c, newBackfillInfo(1, relaycommon.TaskSubmitOutcomeOutcomeUnknown), p2, nil,
		&taskdto.TaskError{Code: "task_submit_outcome_unknown", StatusCode: 502})
	g2, _ := model.GetTaskSubmitRecoveryByID(p2, 1)
	assert.Equal(t, model.TaskRecoveryStatusRecreated, g2.Status, "有子记录时父保持 recreated")
	assert.Contains(t, g2.Note, fmt.Sprintf("%d", child.ID))

	// 3) 结果未知但子记录落库失败（无子记录）→ 父必须保持 recreated，绝不恢复为 unknown
	p3 := insertRecoveryForTest(t, 1, model.TaskRecoveryStatusRecreated)
	backfillRecoveryParent(c, newBackfillInfo(1, relaycommon.TaskSubmitOutcomeOutcomeUnknown), p3, nil,
		&taskdto.TaskError{Code: "task_submit_outcome_unknown", StatusCode: 502})
	g3, _ := model.GetTaskSubmitRecoveryByID(p3, 1)
	assert.Equal(t, model.TaskRecoveryStatusRecreated, g3.Status,
		"结果未知且子记录落库失败时，父记录不得恢复为 unknown（否则误导可安全重试）")
	assert.Contains(t, g3.Note, "落库失败", "备注必须提示子记录落库失败")
	assert.Contains(t, g3.Note, "人工确认", "备注必须提示人工查询确认")

	// 4) 明确失败且无子记录 → 父重新打开为 unknown（可执行的恢复路径）
	p4 := insertRecoveryForTest(t, 1, model.TaskRecoveryStatusRecreated)
	backfillRecoveryParent(c, newBackfillInfo(1, relaycommon.TaskSubmitOutcomeConfirmedFailure), p4, nil,
		&taskdto.TaskError{Code: "fail_to_fetch_task", StatusCode: 400})
	g4, _ := model.GetTaskSubmitRecoveryByID(p4, 1)
	assert.Equal(t, model.TaskRecoveryStatusUnknown, g4.Status, "明确失败必须重新打开为 unknown")
	assert.Contains(t, g4.Note, "重新打开")

	// 5) 未进入创建流程（taskErr=nil result=nil，如锁 503/409 早退）→ 重新打开为 unknown
	p5 := insertRecoveryForTest(t, 1, model.TaskRecoveryStatusRecreated)
	backfillRecoveryParent(c, newBackfillInfo(1, relaycommon.TaskSubmitOutcomeUnset), p5, nil, nil)
	g5, _ := model.GetTaskSubmitRecoveryByID(p5, 1)
	assert.Equal(t, model.TaskRecoveryStatusUnknown, g5.Status, "未进入创建流程必须重新打开为 unknown")
}

// GenRelayInfo 初始化失败早退：父记录必须恢复为可操作状态（unknown），不悬空。
func TestBackfillRecoveryParentNoteResetsToActionable(t *testing.T) {
	initRecoveryControllerTestDB(t)
	p := insertRecoveryForTest(t, 1, model.TaskRecoveryStatusRecreated)

	c := backfillTestCtx(t)
	backfillRecoveryParentNote(c, p, "人工重试未进入创建流程：请求初始化失败，记录已重新打开，可再次尝试")

	got, err := model.GetTaskSubmitRecoveryByID(p, 1)
	require.NoError(t, err)
	assert.Equal(t, model.TaskRecoveryStatusUnknown, got.Status,
		"初始化失败早退（请求从未发出）后父记录必须恢复为可操作状态")
	assert.Contains(t, got.Note, "重新打开")
}
