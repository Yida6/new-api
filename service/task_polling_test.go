package service

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	taskdto "github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/model"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/bytedance/gopkg/util/gopool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type taskPollingFetchAdaptor struct {
	mu           sync.Mutex
	taskIDs      []string
	fetched      chan string
	blockTaskID  string
	blockStarted chan struct{}
	releaseBlock chan struct{}
	blockOnce    sync.Once
}

type sunoFailurePollingAdaptor struct {
	failReason string
}

func (a *sunoFailurePollingAdaptor) Init(_ *relaycommon.RelayInfo) {}

func (a *sunoFailurePollingAdaptor) FetchTask(_ string, _ string, body map[string]any, _ string) (*http.Response, error) {
	taskIDs, _ := body["ids"].([]string)
	items := make([]taskdto.SunoDataResponse, 0, len(taskIDs))
	for _, taskID := range taskIDs {
		items = append(items, taskdto.SunoDataResponse{
			TaskID:     taskID,
			Status:     string(model.TaskStatusFailure),
			FailReason: a.failReason,
			FinishTime: time.Now().Unix(),
		})
	}

	responseBody, err := common.Marshal(taskdto.TaskResponse[[]taskdto.SunoDataResponse]{
		Code: taskdto.TaskSuccessCode,
		Data: items,
	})
	if err != nil {
		return nil, err
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(bytes.NewReader(responseBody)),
	}, nil
}

func (a *sunoFailurePollingAdaptor) ParseTaskResult([]byte) (*relaycommon.TaskInfo, error) {
	return nil, nil
}

func (a *sunoFailurePollingAdaptor) AdjustBillingOnComplete(_ *model.Task, _ *relaycommon.TaskInfo) int64 {
	return 0
}

func (a *taskPollingFetchAdaptor) Init(_ *relaycommon.RelayInfo) {}

func (a *taskPollingFetchAdaptor) FetchTask(_ string, _ string, body map[string]any, _ string) (*http.Response, error) {
	taskID, _ := body["task_id"].(string)
	if taskID == a.blockTaskID && a.releaseBlock != nil {
		a.blockOnce.Do(func() {
			if a.blockStarted != nil {
				close(a.blockStarted)
			}
		})
		<-a.releaseBlock
	}

	a.mu.Lock()
	a.taskIDs = append(a.taskIDs, taskID)
	a.mu.Unlock()
	if a.fetched != nil {
		select {
		case a.fetched <- taskID:
		default:
		}
	}

	response := taskdto.TaskResponse[model.Task]{
		Code: taskdto.TaskSuccessCode,
		Data: model.Task{
			TaskID:   taskID,
			Status:   model.TaskStatusInProgress,
			Progress: "30%",
		},
	}
	responseBody, err := common.Marshal(response)
	if err != nil {
		return nil, err
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(bytes.NewReader(responseBody)),
	}, nil
}

func (a *taskPollingFetchAdaptor) ParseTaskResult([]byte) (*relaycommon.TaskInfo, error) {
	return &relaycommon.TaskInfo{Status: model.TaskStatusInProgress}, nil
}

func (a *taskPollingFetchAdaptor) AdjustBillingOnComplete(_ *model.Task, _ *relaycommon.TaskInfo) int64 {
	return 0
}

func (a *taskPollingFetchAdaptor) fetchCount() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.taskIDs)
}

func (a *taskPollingFetchAdaptor) fetchedTaskIDs() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.taskIDs...)
}

func seedTaskPollingChannel(t *testing.T, id int, disableSleep bool) {
	t.Helper()
	ch := &model.Channel{
		Id:     id,
		Type:   constant.ChannelTypeKling,
		Name:   "polling_channel",
		Key:    "sk-test",
		Status: common.ChannelStatusEnabled,
	}
	if disableSleep {
		ch.SetOtherSettings(dto.ChannelOtherSettings{DisableTaskPollingSleep: true})
	}
	require.NoError(t, model.DB.Create(ch).Error)
}

func seedPollingTask(t *testing.T, channelID int, publicID string, upstreamID string) *model.Task {
	t.Helper()
	task := &model.Task{
		TaskID:    publicID,
		Platform:  constant.TaskPlatform("kling"),
		UserId:    1,
		ChannelId: channelID,
		Action:    constant.TaskActionGenerate,
		Status:    model.TaskStatusInProgress,
		Progress:  "30%",
		CreatedAt: time.Now().Unix(),
		UpdatedAt: time.Now().Unix(),
		PrivateData: model.TaskPrivateData{
			UpstreamTaskID: upstreamID,
		},
	}
	require.NoError(t, model.DB.Create(task).Error)
	return task
}

func TestUpdateVideoTasksDefaultSleepWaitsBetweenTasks(t *testing.T) {
	truncate(t)

	const channelID = 101
	seedTaskPollingChannel(t, channelID, false)
	first := seedPollingTask(t, channelID, "task_public_1", "upstream_1")
	second := seedPollingTask(t, channelID, "task_public_2", "upstream_2")

	adaptor := &taskPollingFetchAdaptor{}
	previousFactory := GetTaskAdaptorFunc
	GetTaskAdaptorFunc = func(constant.TaskPlatform) TaskPollingAdaptor { return adaptor }
	t.Cleanup(func() { GetTaskAdaptorFunc = previousFactory })

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	err := UpdateVideoTasks(ctx, constant.TaskPlatform("kling"), map[int][]string{
		channelID: {
			first.GetUpstreamTaskID(),
			second.GetUpstreamTaskID(),
		},
	}, map[string]*model.Task{
		first.GetUpstreamTaskID():  first,
		second.GetUpstreamTaskID(): second,
	})

	require.ErrorIs(t, err, context.DeadlineExceeded)
	assert.Equal(t, 1, adaptor.fetchCount())
}

func TestUpdateVideoTasksCanSkipPollingSleepPerChannel(t *testing.T) {
	truncate(t)

	const channelID = 102
	seedTaskPollingChannel(t, channelID, true)
	first := seedPollingTask(t, channelID, "task_public_3", "upstream_3")
	second := seedPollingTask(t, channelID, "task_public_4", "upstream_4")

	adaptor := &taskPollingFetchAdaptor{}
	previousFactory := GetTaskAdaptorFunc
	GetTaskAdaptorFunc = func(constant.TaskPlatform) TaskPollingAdaptor { return adaptor }
	t.Cleanup(func() { GetTaskAdaptorFunc = previousFactory })

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	err := UpdateVideoTasks(ctx, constant.TaskPlatform("kling"), map[int][]string{
		channelID: {
			first.GetUpstreamTaskID(),
			second.GetUpstreamTaskID(),
		},
	}, map[string]*model.Task{
		first.GetUpstreamTaskID():  first,
		second.GetUpstreamTaskID(): second,
	})

	require.NoError(t, err)
	assert.Equal(t, 2, adaptor.fetchCount())
}

func TestUpdateVideoTasksDefaultSleepDoesNotBlockOtherChannels(t *testing.T) {
	truncate(t)

	const firstChannelID = 201
	const secondChannelID = 202
	seedTaskPollingChannel(t, firstChannelID, false)
	seedTaskPollingChannel(t, secondChannelID, false)
	firstChannelFirst := seedPollingTask(t, firstChannelID, "task_public_5", "upstream_a_1")
	firstChannelSecond := seedPollingTask(t, firstChannelID, "task_public_6", "upstream_a_2")
	secondChannelFirst := seedPollingTask(t, secondChannelID, "task_public_7", "upstream_b_1")
	secondChannelSecond := seedPollingTask(t, secondChannelID, "task_public_8", "upstream_b_2")

	adaptor := &taskPollingFetchAdaptor{}
	previousFactory := GetTaskAdaptorFunc
	GetTaskAdaptorFunc = func(constant.TaskPlatform) TaskPollingAdaptor { return adaptor }
	t.Cleanup(func() { GetTaskAdaptorFunc = previousFactory })

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	err := UpdateVideoTasks(ctx, constant.TaskPlatform("kling"), map[int][]string{
		firstChannelID: {
			firstChannelFirst.GetUpstreamTaskID(),
			firstChannelSecond.GetUpstreamTaskID(),
		},
		secondChannelID: {
			secondChannelFirst.GetUpstreamTaskID(),
			secondChannelSecond.GetUpstreamTaskID(),
		},
	}, map[string]*model.Task{
		firstChannelFirst.GetUpstreamTaskID():   firstChannelFirst,
		firstChannelSecond.GetUpstreamTaskID():  firstChannelSecond,
		secondChannelFirst.GetUpstreamTaskID():  secondChannelFirst,
		secondChannelSecond.GetUpstreamTaskID(): secondChannelSecond,
	})

	require.ErrorIs(t, err, context.DeadlineExceeded)
	assert.ElementsMatch(t, []string{"upstream_a_1", "upstream_b_1"}, adaptor.fetchedTaskIDs())
}

func TestUpdateVideoTasksSlowChannelDoesNotBlockOtherChannels(t *testing.T) {
	truncate(t)

	const slowChannelID = 251
	const fastChannelID = 252
	seedTaskPollingChannel(t, slowChannelID, false)
	seedTaskPollingChannel(t, fastChannelID, true)
	slowTask := seedPollingTask(t, slowChannelID, "task_public_slow", "upstream_slow_1")
	fastFirst := seedPollingTask(t, fastChannelID, "task_public_fast_1", "upstream_fast_parallel_1")
	fastSecond := seedPollingTask(t, fastChannelID, "task_public_fast_2", "upstream_fast_parallel_2")

	adaptor := &taskPollingFetchAdaptor{
		fetched:      make(chan string, 4),
		blockTaskID:  slowTask.GetUpstreamTaskID(),
		blockStarted: make(chan struct{}),
		releaseBlock: make(chan struct{}),
	}
	var releaseOnce sync.Once
	releaseBlockedTask := func() {
		releaseOnce.Do(func() {
			close(adaptor.releaseBlock)
		})
	}
	t.Cleanup(releaseBlockedTask)
	previousFactory := GetTaskAdaptorFunc
	GetTaskAdaptorFunc = func(constant.TaskPlatform) TaskPollingAdaptor { return adaptor }
	t.Cleanup(func() { GetTaskAdaptorFunc = previousFactory })

	errCh := make(chan error, 1)
	gopool.Go(func() {
		errCh <- UpdateVideoTasks(context.Background(), constant.TaskPlatform("kling"), map[int][]string{
			slowChannelID: {
				slowTask.GetUpstreamTaskID(),
			},
			fastChannelID: {
				fastFirst.GetUpstreamTaskID(),
				fastSecond.GetUpstreamTaskID(),
			},
		}, map[string]*model.Task{
			slowTask.GetUpstreamTaskID():   slowTask,
			fastFirst.GetUpstreamTaskID():  fastFirst,
			fastSecond.GetUpstreamTaskID(): fastSecond,
		})
	})

	select {
	case <-adaptor.blockStarted:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("slow channel did not start blocking")
	}

	require.Eventually(t, func() bool {
		fetchedTaskIDs := adaptor.fetchedTaskIDs()
		return len(fetchedTaskIDs) == 2 &&
			fetchedTaskIDs[0] == fastFirst.GetUpstreamTaskID() &&
			fetchedTaskIDs[1] == fastSecond.GetUpstreamTaskID()
	}, 500*time.Millisecond, 10*time.Millisecond)

	releaseBlockedTask()
	require.NoError(t, <-errCh)
	assert.ElementsMatch(t, []string{
		slowTask.GetUpstreamTaskID(),
		fastFirst.GetUpstreamTaskID(),
		fastSecond.GetUpstreamTaskID(),
	}, adaptor.fetchedTaskIDs())
}

func TestUpdateVideoTasksMixedChannelSleepSettings(t *testing.T) {
	truncate(t)

	const sleepyChannelID = 301
	const fastChannelID = 302
	seedTaskPollingChannel(t, sleepyChannelID, false)
	seedTaskPollingChannel(t, fastChannelID, true)
	sleepyFirst := seedPollingTask(t, sleepyChannelID, "task_public_9", "upstream_sleepy_1")
	sleepySecond := seedPollingTask(t, sleepyChannelID, "task_public_10", "upstream_sleepy_2")
	fastFirst := seedPollingTask(t, fastChannelID, "task_public_11", "upstream_fast_1")
	fastSecond := seedPollingTask(t, fastChannelID, "task_public_12", "upstream_fast_2")

	adaptor := &taskPollingFetchAdaptor{}
	previousFactory := GetTaskAdaptorFunc
	GetTaskAdaptorFunc = func(constant.TaskPlatform) TaskPollingAdaptor { return adaptor }
	t.Cleanup(func() { GetTaskAdaptorFunc = previousFactory })

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	err := UpdateVideoTasks(ctx, constant.TaskPlatform("kling"), map[int][]string{
		sleepyChannelID: {
			sleepyFirst.GetUpstreamTaskID(),
			sleepySecond.GetUpstreamTaskID(),
		},
		fastChannelID: {
			fastFirst.GetUpstreamTaskID(),
			fastSecond.GetUpstreamTaskID(),
		},
	}, map[string]*model.Task{
		sleepyFirst.GetUpstreamTaskID():  sleepyFirst,
		sleepySecond.GetUpstreamTaskID(): sleepySecond,
		fastFirst.GetUpstreamTaskID():    fastFirst,
		fastSecond.GetUpstreamTaskID():   fastSecond,
	})

	require.ErrorIs(t, err, context.DeadlineExceeded)
	assert.ElementsMatch(t, []string{"upstream_sleepy_1", "upstream_fast_1", "upstream_fast_2"}, adaptor.fetchedTaskIDs())
}

func TestUpdateSunoTasksStalePollsRefundExactlyOnce(t *testing.T) {
	truncate(t)

	const userID, tokenID, channelID = 401, 401, 401
	const initialUserQuota, initialTokenQuota, taskQuota = 10_000, 6_000, 2_500
	const publicTaskID, upstreamTaskID = "suno_public_refund_once", "suno_upstream_refund_once"

	seedUser(t, userID, initialUserQuota)
	seedToken(t, tokenID, userID, "sk-suno-refund-once", initialTokenQuota)
	baseURL := "https://suno.invalid"
	require.NoError(t, model.DB.Create(&model.Channel{
		Id:      channelID,
		Type:    constant.ChannelTypeSunoAPI,
		Name:    "suno_refund_once",
		Key:     "sk-suno-channel",
		Status:  common.ChannelStatusEnabled,
		BaseURL: &baseURL,
	}).Error)
	// 预置累计消耗：模拟"提交任务已预扣"的前置状态，使退款守卫（used_quota >= refund）通过
	seedUsedQuota(t, userID, channelID, taskQuota)

	task := makeTask(userID, channelID, taskQuota, tokenID, BillingSourceWallet, 0)
	task.TaskID = publicTaskID
	task.Platform = constant.TaskPlatformSuno
	task.Status = model.TaskStatusInProgress
	task.Progress = "50%"
	task.SubmitTime = time.Now().Unix()
	task.PrivateData.UpstreamTaskID = upstreamTaskID
	require.NoError(t, model.DB.Create(task).Error)

	var firstPollTask model.Task
	var staleSecondPollTask model.Task
	require.NoError(t, model.DB.First(&firstPollTask, task.ID).Error)
	require.NoError(t, model.DB.First(&staleSecondPollTask, task.ID).Error)

	adaptor := &sunoFailurePollingAdaptor{failReason: "upstream failed"}
	previousFactory := GetTaskAdaptorFunc
	GetTaskAdaptorFunc = func(constant.TaskPlatform) TaskPollingAdaptor { return adaptor }
	t.Cleanup(func() { GetTaskAdaptorFunc = previousFactory })

	require.NoError(t, updateSunoTasks(context.Background(), channelID, []string{upstreamTaskID}, map[string]*model.Task{
		upstreamTaskID: &firstPollTask,
	}))
	require.NoError(t, updateSunoTasks(context.Background(), channelID, []string{upstreamTaskID}, map[string]*model.Task{
		upstreamTaskID: &staleSecondPollTask,
	}))

	var reloaded model.Task
	require.NoError(t, model.DB.First(&reloaded, task.ID).Error)
	assert.EqualValues(t, model.TaskStatusFailure, reloaded.Status)
	assert.Zero(t, reloaded.Quota)
	assert.Equal(t, int64(initialUserQuota+taskQuota), getUserQuota(t, userID))
	assert.Equal(t, int64(initialTokenQuota+taskQuota), getTokenRemainQuota(t, tokenID))
	assert.Equal(t, int64(1), countLogs(t))
}

func TestRunTaskPollingOnceDoesNotRefundHistoricalFailedTask(t *testing.T) {
	truncate(t)

	const userID, initialQuota, taskQuota = 402, 10_000, 1_200
	seedUser(t, userID, initialQuota)

	task := makeTask(userID, 0, taskQuota, 0, BillingSourceWallet, 0)
	task.TaskID = "historical_failed_already_refunded"
	task.Status = model.TaskStatusFailure
	task.Progress = "100%"
	task.SubmitTime = time.Now().Add(-90 * 24 * time.Hour).Unix()
	task.UpdatedAt = time.Now().Add(-time.Minute).Unix()
	require.NoError(t, model.DB.Create(task).Error)

	previousFactory := GetTaskAdaptorFunc
	GetTaskAdaptorFunc = func(constant.TaskPlatform) TaskPollingAdaptor {
		return &taskPollingFetchAdaptor{}
	}
	t.Cleanup(func() { GetTaskAdaptorFunc = previousFactory })

	summary := RunTaskPollingOnce(context.Background(), nil)

	assert.Zero(t, summary.UnfinishedTasks)
	assert.Equal(t, int64(initialQuota), getUserQuota(t, userID))
	assert.Equal(t, int64(taskQuota), getTaskQuota(t, task.ID))
	assert.Equal(t, int64(0), countLogs(t))
}

func TestSweepTimedOutTasksHonorsRefundRolloutBoundary(t *testing.T) {
	truncate(t)

	const (
		userID          = 403
		initialQuota    = 10_000
		legacyTaskQuota = 1_800
		modernTaskQuota = 1_200
	)
	seedUser(t, userID, initialQuota)
	// 预置累计消耗：模拟"提交任务已预扣"的前置状态，使 modern 任务退款守卫
	// （used_quota >= refund）通过（生产路径由 LogTaskConsumption 完成预扣）
	seedUsedQuota(t, userID, 0, modernTaskQuota)

	legacyTask := makeTask(userID, 0, legacyTaskQuota, 0, BillingSourceWallet, 0)
	legacyTask.TaskID = "legacy_timeout_without_refund"
	legacyTask.Progress = "50%"
	legacyTask.SubmitTime = 1771718399 // 2026-02-21 23:59:59 UTC
	require.NoError(t, model.DB.Create(legacyTask).Error)

	modernTask := makeTask(userID, 0, modernTaskQuota, 0, BillingSourceWallet, 0)
	modernTask.TaskID = "modern_timeout_with_refund"
	modernTask.Progress = "50%"
	modernTask.SubmitTime = 1771718400 // 2026-02-22 00:00:00 UTC
	require.NoError(t, model.DB.Create(modernTask).Error)

	previousTimeout := constant.TaskTimeoutMinutes
	constant.TaskTimeoutMinutes = 1
	t.Cleanup(func() { constant.TaskTimeoutMinutes = previousTimeout })

	sweepTimedOutTasks(context.Background())

	var reloadedLegacy model.Task
	var reloadedModern model.Task
	require.NoError(t, model.DB.First(&reloadedLegacy, legacyTask.ID).Error)
	require.NoError(t, model.DB.First(&reloadedModern, modernTask.ID).Error)
	assert.EqualValues(t, model.TaskStatusFailure, reloadedLegacy.Status)
	assert.EqualValues(t, model.TaskStatusFailure, reloadedModern.Status)
	assert.Zero(t, reloadedLegacy.Quota)
	assert.Zero(t, reloadedModern.Quota)
	assert.Contains(t, reloadedLegacy.FailReason, "旧系统遗留任务")
	assert.Contains(t, reloadedModern.FailReason, "任务超时")
	assert.Equal(t, int64(initialQuota+modernTaskQuota), getUserQuota(t, userID))
	assert.Equal(t, int64(1), countLogs(t))
}

// ===========================================================================
// 并发名额释放时机回归测试（Fix：计费失败回退非终态前不得提前释放名额）
//
// 旧实现：CAS 胜出转终态后立即 ReleaseTaskSlotIfSeedance，随后计费（退款/差额
// 结算）失败回退非终态时名额已释放且 ConcurrencyReleased 已置位 → 任务重跑期间
// 不再计入并发限制，最终成功时也无法再释放，计数失真。
// 新实现：只有"终态确立且计费成功"才释放；计费失败回退非终态时名额继续占用。
// ===========================================================================

// wipeConcurrencySlots 清理所有并发名额行（truncate 不覆盖该表，避免跨测试污染）。
func wipeConcurrencySlots(t *testing.T) {
	t.Helper()
	t.Cleanup(func() {
		_ = model.DB.Exec("DELETE FROM task_concurrency_slots").Error
	})
}

// sweepTimedOutTasks：退款失败 → 回退非终态且名额保留；补足累计消耗后再次
// sweep → 终态确立且退款成功 → 释放名额。
func TestSweepTimedOutTasks_RefundFailureKeepsConcurrencySlot(t *testing.T) {
	truncate(t)
	cleanupConcurrencyTestData(t)
	wipeConcurrencySlots(t)

	oldTimeout := constant.TaskTimeoutMinutes
	constant.TaskTimeoutMinutes = 120
	t.Cleanup(func() { constant.TaskTimeoutMinutes = oldTimeout })

	const userID, preConsumed = 70, 1200
	seedUser(t, userID, 5000)
	// 不 seed used_quota → RefundTaskQuota 的累计消耗守卫失败

	task := &model.Task{
		TaskID:     "sweep_slot_task",
		UserId:     userID,
		Platform:   constant.TaskPlatform("54"),
		Quota:      preConsumed,
		Status:     model.TaskStatus(model.TaskStatusQueued),
		Group:      "default",
		Data:       json.RawMessage(`{}`),
		SubmitTime: time.Now().Unix() - 3*3600,
		CreatedAt:  time.Now().Unix(),
		UpdatedAt:  time.Now().Unix(),
		PrivateData: model.TaskPrivateData{
			ConsumeLogRecorded: true,
		},
	}
	// 与生产顺序一致：先预留名额，再创建任务行（预留时的存量补齐因此不会计入本任务）
	ok, _, err := model.ReserveTaskConcurrencySlot(userID, 3)
	require.NoError(t, err)
	require.True(t, ok)
	require.NoError(t, model.DB.Create(task).Error)

	sweepTimedOutTasks(context.Background())

	// 退款失败 → 回退非终态，名额必须保留
	count, err := model.GetRunningCountForUser(userID)
	require.NoError(t, err)
	assert.Equal(t, 1, count, "退款失败回退非终态，名额不得释放")
	var reloaded model.Task
	require.NoError(t, model.DB.First(&reloaded, task.ID).Error)
	assert.EqualValues(t, model.TaskStatusQueued, reloaded.Status, "退款失败应回退到非终态")
	assert.False(t, reloaded.ConcurrencyReleased, "回退非终态后 ConcurrencyReleased 必须保持 false")
	assert.Equal(t, int64(preConsumed), reloaded.Quota)

	// 补上累计消耗后再次 sweep：退款成功 + 终态确立 → 释放名额
	seedUsedQuota(t, userID, 0, preConsumed)
	sweepTimedOutTasks(context.Background())
	count, err = model.GetRunningCountForUser(userID)
	require.NoError(t, err)
	assert.Equal(t, 0, count, "退款成功且终态确立后释放名额")
	require.NoError(t, model.DB.First(&reloaded, task.ID).Error)
	assert.EqualValues(t, model.TaskStatusFailure, reloaded.Status)
	assert.True(t, reloaded.ConcurrencyReleased)
	assert.Zero(t, reloaded.Quota)
}

// 旧系统遗留任务（isLegacy）不退款、quota 清零 → 终态确立即释放名额。
func TestSweepTimedOutTasks_LegacyTaskReleasesSlot(t *testing.T) {
	truncate(t)
	cleanupConcurrencyTestData(t)
	wipeConcurrencySlots(t)

	oldTimeout := constant.TaskTimeoutMinutes
	constant.TaskTimeoutMinutes = 120
	t.Cleanup(func() { constant.TaskTimeoutMinutes = oldTimeout })

	const userID, preConsumed = 71, 1200
	seedUser(t, userID, 5000)

	task := &model.Task{
		TaskID:     "sweep_legacy_task",
		UserId:     userID,
		Platform:   constant.TaskPlatform("54"),
		Quota:      preConsumed,
		Status:     model.TaskStatus(model.TaskStatusQueued),
		Group:      "default",
		Data:       json.RawMessage(`{}`),
		SubmitTime: model.TaskRefundLegacyCutoff - 3600, // 早于遗留任务分界
		CreatedAt:  time.Now().Unix(),
		UpdatedAt:  time.Now().Unix(),
	}
	// 与生产顺序一致：先预留名额，再创建任务行
	ok, _, err := model.ReserveTaskConcurrencySlot(userID, 3)
	require.NoError(t, err)
	require.True(t, ok)
	require.NoError(t, model.DB.Create(task).Error)

	sweepTimedOutTasks(context.Background())

	count, err := model.GetRunningCountForUser(userID)
	require.NoError(t, err)
	assert.Equal(t, 0, count, "遗留任务终态确立（不退款）后释放名额")
	var reloaded model.Task
	require.NoError(t, model.DB.First(&reloaded, task.ID).Error)
	assert.EqualValues(t, model.TaskStatusFailure, reloaded.Status)
	assert.True(t, reloaded.ConcurrencyReleased)
	assert.Zero(t, reloaded.Quota)
}

// failingRefundStubAdaptor 返回确定失败的任务结果，供 updateVideoSingleTask 驱动
// 退款路径（累计消耗守卫失败 → RefundTaskQuota 返回 false）。
type failingRefundStubAdaptor struct{}

func (failingRefundStubAdaptor) Init(_ *relaycommon.RelayInfo) {}
func (failingRefundStubAdaptor) FetchTask(_ string, _ string, _ map[string]any, _ string) (*http.Response, error) {
	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader(`{"status":"FAILURE","reason":"boom"}`)),
	}, nil
}
func (failingRefundStubAdaptor) ParseTaskResult(body []byte) (*relaycommon.TaskInfo, error) {
	return &relaycommon.TaskInfo{Status: "FAILURE", Reason: "boom"}, nil
}
func (failingRefundStubAdaptor) AdjustBillingOnComplete(_ *model.Task, _ *relaycommon.TaskInfo) int64 {
	return 0
}

// updateVideoSingleTask：退款失败 → 回退非终态且名额保留；补足累计消耗后再次
// 轮询 → 终态确立且退款成功 → 释放名额。
func TestUpdateVideoSingleTask_RefundFailureKeepsConcurrencySlot(t *testing.T) {
	truncate(t)
	cleanupConcurrencyTestData(t)
	wipeConcurrencySlots(t)
	ctx := context.Background()

	const userID, channelID, preConsumed = 72, 72, 1200
	seedUser(t, userID, 5000)
	seedChannel(t, channelID)
	// 不 seed used_quota → RefundTaskQuota 的累计消耗守卫失败

	task := &model.Task{
		TaskID:    "upstream1",
		UserId:    userID,
		Platform:  constant.TaskPlatform("54"),
		ChannelId: channelID,
		Quota:     preConsumed,
		Status:    model.TaskStatus(model.TaskStatusQueued),
		Group:     "default",
		Data:      json.RawMessage(`{}`),
		CreatedAt: time.Now().Unix(),
		UpdatedAt: time.Now().Unix(),
		PrivateData: model.TaskPrivateData{
			ConsumeLogRecorded: true,
		},
	}
	// 与生产顺序一致：先预留名额，再创建任务行
	ok, _, err := model.ReserveTaskConcurrencySlot(userID, 3)
	require.NoError(t, err)
	require.True(t, ok)
	require.NoError(t, model.DB.Create(task).Error)
	ch := &model.Channel{Id: channelID, Type: constant.ChannelTypeDoubaoVideo, Name: "t", Key: "sk-t", Status: common.ChannelStatusEnabled}

	taskM := map[string]*model.Task{task.TaskID: task}
	require.NoError(t, updateVideoSingleTask(ctx, failingRefundStubAdaptor{}, ch, task.TaskID, taskM))

	count, err := model.GetRunningCountForUser(userID)
	require.NoError(t, err)
	assert.Equal(t, 1, count, "退款失败回退非终态，名额不得释放")
	var reloaded model.Task
	require.NoError(t, model.DB.First(&reloaded, task.ID).Error)
	assert.EqualValues(t, model.TaskStatusQueued, reloaded.Status, "退款失败应回退到非终态")
	assert.False(t, reloaded.ConcurrencyReleased, "回退非终态后 ConcurrencyReleased 必须保持 false")
	assert.Equal(t, int64(preConsumed), reloaded.Quota)

	// 补上累计消耗后再次轮询：退款成功 + 终态确立 → 释放名额
	seedUsedQuota(t, userID, channelID, preConsumed)
	var fresh model.Task
	require.NoError(t, model.DB.First(&fresh, task.ID).Error)
	freshM := map[string]*model.Task{fresh.TaskID: &fresh}
	require.NoError(t, updateVideoSingleTask(ctx, failingRefundStubAdaptor{}, ch, fresh.TaskID, freshM))

	count, err = model.GetRunningCountForUser(userID)
	require.NoError(t, err)
	assert.Equal(t, 0, count, "退款成功且终态确立后释放名额")
	require.NoError(t, model.DB.First(&reloaded, task.ID).Error)
	assert.EqualValues(t, model.TaskStatusFailure, reloaded.Status)
	assert.True(t, reloaded.ConcurrencyReleased)
	assert.Zero(t, reloaded.Quota)
}

// 对照：CAS 未胜出（其他路径已转终态）时，本路径不释放名额也不计费。
func TestUpdateVideoSingleTask_CASLostNoRelease(t *testing.T) {
	truncate(t)
	cleanupConcurrencyTestData(t)
	wipeConcurrencySlots(t)
	ctx := context.Background()

	const userID, channelID, preConsumed = 73, 73, 1200
	seedUser(t, userID, 5000)
	seedChannel(t, channelID)

	task := &model.Task{
		TaskID:    "upstream_cas_lost",
		UserId:    userID,
		Platform:  constant.TaskPlatform("54"),
		ChannelId: channelID,
		Quota:     preConsumed,
		Status:    model.TaskStatus(model.TaskStatusQueued),
		Group:     "default",
		Data:      json.RawMessage(`{}`),
		CreatedAt: time.Now().Unix(),
		UpdatedAt: time.Now().Unix(),
	}
	// 与生产顺序一致：先预留名额，再创建任务行
	ok, _, err := model.ReserveTaskConcurrencySlot(userID, 3)
	require.NoError(t, err)
	require.True(t, ok)
	require.NoError(t, model.DB.Create(task).Error)
	ch := &model.Channel{Id: channelID, Type: constant.ChannelTypeDoubaoVideo, Name: "t", Key: "sk-t", Status: common.ChannelStatusEnabled}

	// 模拟另一路径已把任务推进为 FAILURE（CAS 将失败）
	require.NoError(t, model.DB.Model(&model.Task{}).Where("id = ?", task.ID).Update("status", model.TaskStatusFailure).Error)

	taskM := map[string]*model.Task{task.TaskID: task}
	require.NoError(t, updateVideoSingleTask(ctx, failingRefundStubAdaptor{}, ch, task.TaskID, taskM))

	// 本路径 CAS 失败 → 不释放（名额由胜出路径释放）
	count, err := model.GetRunningCountForUser(userID)
	require.NoError(t, err)
	assert.Equal(t, 1, count, "CAS 未胜出不得释放名额")
	var reloaded model.Task
	require.NoError(t, model.DB.First(&reloaded, task.ID).Error)
	assert.False(t, reloaded.ConcurrencyReleased)
}
