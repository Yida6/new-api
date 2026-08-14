package service

import (
	"sync"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// Seedance 单用户并发任务数限制 — 单元/集成测试
//
// 覆盖需求：低于上限、刚好达到上限、任务结束后释放名额、并发创建、
// 不同用户互不影响；以及兜底对账（异常退出清理、计数偏低回补）。
// ---------------------------------------------------------------------------

// concurrencyTestUserIDs 测试专用用户 ID（避开其他测试数据）。
var concurrencyTestUserIDs = []int{900001, 900002, 900003, 900004}

func cleanupConcurrencyTestData(t *testing.T) {
	t.Helper()
	require.NoError(t, model.DB.Exec("DELETE FROM task_concurrency_slots").Error)
	require.NoError(t, model.DB.Where("user_id IN ?", concurrencyTestUserIDs).Delete(&model.Task{}).Error)
	require.NoError(t, model.DB.Where("user_id IN ?", concurrencyTestUserIDs).Delete(&model.TaskSubmitRecovery{}).Error)
}

// 低于上限：limit=3，连续预留 3 次均成功，计数依次递增。
func TestReserveTaskConcurrencySlotBelowLimit(t *testing.T) {
	cleanupConcurrencyTestData(t)
	const uid, limit = 900001, 3

	ok1, cur1, err := model.ReserveTaskConcurrencySlot(uid, limit)
	require.NoError(t, err)
	assert.True(t, ok1)
	assert.Equal(t, 1, cur1)

	ok2, cur2, err := model.ReserveTaskConcurrencySlot(uid, limit)
	require.NoError(t, err)
	assert.True(t, ok2)
	assert.Equal(t, 2, cur2)

	ok3, cur3, err := model.ReserveTaskConcurrencySlot(uid, limit)
	require.NoError(t, err)
	assert.True(t, ok3)
	assert.Equal(t, 3, cur3)

	count, err := model.GetRunningCountForUser(uid)
	require.NoError(t, err)
	assert.Equal(t, 3, count)
}

// 刚好达到上限：第 4 次预留被拒绝，返回当前运行数 3。
func TestReserveTaskConcurrencySlotAtLimit(t *testing.T) {
	cleanupConcurrencyTestData(t)
	const uid, limit = 900001, 3

	for i := 0; i < limit; i++ {
		ok, _, err := model.ReserveTaskConcurrencySlot(uid, limit)
		require.NoError(t, err)
		require.True(t, ok)
	}

	ok, current, err := model.ReserveTaskConcurrencySlot(uid, limit)
	require.NoError(t, err)
	assert.False(t, ok, "达到上限后必须拒绝创建")
	assert.Equal(t, limit, current, "拒绝时应返回当前运行数供 429 文案使用")
}

// 任务结束后释放名额：释放后计数下降，可再次预留；重复释放不会低于 0。
func TestReleaseTaskConcurrencySlotFreesSlot(t *testing.T) {
	cleanupConcurrencyTestData(t)
	const uid, limit = 900001, 3

	for i := 0; i < limit; i++ {
		_, _, _ = model.ReserveTaskConcurrencySlot(uid, limit)
	}

	// 释放一个名额 → 计数 2，可再预留一个
	require.NoError(t, model.ReleaseTaskConcurrencySlot(uid))
	ok, _, err := model.ReserveTaskConcurrencySlot(uid, limit)
	require.NoError(t, err)
	assert.True(t, ok, "释放名额后应能再次创建")

	// 全部释放 → 计数 0
	require.NoError(t, model.ReleaseTaskConcurrencySlot(uid))
	require.NoError(t, model.ReleaseTaskConcurrencySlot(uid))
	require.NoError(t, model.ReleaseTaskConcurrencySlot(uid))
	require.NoError(t, model.ReleaseTaskConcurrencySlot(uid)) // 多释放一次 → no-op
	count, err := model.GetRunningCountForUser(uid)
	require.NoError(t, err)
	assert.Equal(t, 0, count, "重复释放不得把计数降到 0 以下")

	// 未预留过的用户释放 → no-op 不报错
	require.NoError(t, model.ReleaseTaskConcurrencySlot(900004))
}

// 并发创建：limit=3，10 个 goroutine 同时预留，成功数必须恰好等于上限，
// 多余请求全部被拒绝——防并发绕过。所有预留都必须无错误返回。
func TestReserveTaskConcurrencySlotConcurrent(t *testing.T) {
	cleanupConcurrencyTestData(t)
	const uid, limit = 900001, 3
	const goroutines = 10

	results := make([]bool, goroutines)
	currents := make([]int, goroutines)
	errs := make([]error, goroutines)
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func(idx int) {
			defer wg.Done()
			ok, cur, err := model.ReserveTaskConcurrencySlot(uid, limit)
			results[idx] = ok
			currents[idx] = cur
			errs[idx] = err
		}(i)
	}
	wg.Wait()

	success := 0
	for i, ok := range results {
		require.NoError(t, errs[i], "并发预留不得返回错误")
		if ok {
			success++
			continue
		}
		assert.Equal(t, limit, currents[i], "被拒绝的请求应看到当前运行数=上限")
	}
	assert.Equal(t, limit, success, "并发创建时成功数必须严格等于上限，不允许绕过")
	count, err := model.GetRunningCountForUser(uid)
	require.NoError(t, err)
	assert.Equal(t, limit, count)
}

// 不同用户互不影响：用户 A 已达上限，用户 B 仍可正常创建。
func TestReserveTaskConcurrencySlotDifferentUsersIsolated(t *testing.T) {
	cleanupConcurrencyTestData(t)
	const limit = 3

	for i := 0; i < limit; i++ {
		ok, _, err := model.ReserveTaskConcurrencySlot(900001, limit)
		require.NoError(t, err)
		require.True(t, ok)
	}

	// 用户 A 已达上限
	okA, _, err := model.ReserveTaskConcurrencySlot(900001, limit)
	require.NoError(t, err)
	assert.False(t, okA)

	// 用户 B 不受影响
	okB, curB, err := model.ReserveTaskConcurrencySlot(900002, limit)
	require.NoError(t, err)
	assert.True(t, okB)
	assert.Equal(t, 1, curB)
}

// 未启用限制（limit<=0）：直接放行且不落库计数。
func TestReserveTaskConcurrencySlotDisabledWhenLimitNonPositive(t *testing.T) {
	cleanupConcurrencyTestData(t)

	ok, _, err := model.ReserveTaskConcurrencySlot(900001, 0)
	require.NoError(t, err)
	assert.True(t, ok, "limit<=0 表示不限制")

	count, err := model.GetRunningCountForUser(900001)
	require.NoError(t, err)
	assert.Equal(t, 0, count, "不限制时不应产生计数行")
}

// 兜底对账 1：进程异常退出留下的泄漏名额（计数 > 真实运行数且行已 stale）被清理。
func TestReconcileHealsLeakedReservation(t *testing.T) {
	cleanupConcurrencyTestData(t)
	const uid, limit = 900001, 3

	// 预留 2 个名额但从未创建任务行、也未释放（模拟进程崩溃）
	_, _, _ = model.ReserveTaskConcurrencySlot(uid, limit)
	_, _, _ = model.ReserveTaskConcurrencySlot(uid, limit)

	// 让行变 stale（1 小时前更新）
	require.NoError(t, model.DB.Model(&model.TaskConcurrencySlot{}).
		Where("user_id = ?", uid).
		Update("updated_at", time.Now().Unix()-3600).Error)

	fixed, err := model.ReconcileTaskConcurrencySlots(30 * time.Minute)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, fixed, 1)
	count, err := model.GetRunningCountForUser(uid)
	require.NoError(t, err)
	assert.Equal(t, 0, count, "泄漏名额必须被清理")
}

// 兜底对账 2：在途预留（行新鲜）不会被误清。
func TestReconcileKeepsFreshReservation(t *testing.T) {
	cleanupConcurrencyTestData(t)
	const uid, limit = 900001, 3

	// 刚预留（updated_at 新鲜），任务行尚未创建
	_, _, _ = model.ReserveTaskConcurrencySlot(uid, limit)

	fixed, err := model.ReconcileTaskConcurrencySlots(30 * time.Minute)
	require.NoError(t, err)
	assert.Equal(t, 0, fixed, "新鲜的在途预留不得被对账误清")
	count, err := model.GetRunningCountForUser(uid)
	require.NoError(t, err)
	assert.Equal(t, 1, count)
}

// 兜底对账 3：计数偏低（重复释放）被回补；缺计数行的存量运行任务被补建。
func TestReconcileRestoresUnderCountAndBackfills(t *testing.T) {
	cleanupConcurrencyTestData(t)

	// 用户 A：重复释放导致计数(0) < 真实运行数(1)
	_, _, _ = model.ReserveTaskConcurrencySlot(900001, 3)
	require.NoError(t, model.ReleaseTaskConcurrencySlot(900001))
	require.NoError(t, model.ReleaseTaskConcurrencySlot(900001)) // 重复释放 → 0
	require.NoError(t, model.DB.Create(&model.Task{
		TaskID:   "seed_under_a",
		UserId:   900001,
		Platform: constant.TaskPlatform("54"), // doubao 视频（Seedance 家族）
		Status:   model.TaskStatusQueued,
	}).Error)

	// 用户 B：有运行任务但从未预留过（无计数行，如功能上线前存量任务）
	require.NoError(t, model.DB.Create(&model.Task{
		TaskID:   "seed_back_b",
		UserId:   900002,
		Platform: constant.TaskPlatform("45"), // 火山方舟（Seedance 家族）
		Status:   model.TaskStatusInProgress,
	}).Error)

	fixed, err := model.ReconcileTaskConcurrencySlots(30 * time.Minute)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, fixed, 2)

	countA, err := model.GetRunningCountForUser(900001)
	require.NoError(t, err)
	assert.Equal(t, 1, countA, "计数偏低必须回补为真实运行数")

	countB, err := model.GetRunningCountForUser(900002)
	require.NoError(t, err)
	assert.Equal(t, 1, countB, "缺计数行的存量运行任务必须补建计数")

	// 终态任务不计入：再插入一个 SUCCESS 任务，计数不变
	require.NoError(t, model.DB.Create(&model.Task{
		TaskID:   "seed_done_b",
		UserId:   900002,
		Platform: constant.TaskPlatform("45"),
		Status:   model.TaskStatusSuccess,
	}).Error)
	actual, err := model.CountRunningSeedanceTasks(900002)
	require.NoError(t, err)
	assert.EqualValues(t, 1, actual, "completed 任务不占用名额")
}

// 平台判定：doubao 视频(54)/火山方舟(45) 属 Seedance 家族；其他渠道不是。
func TestIsSeedanceChannelTypeAndPlatform(t *testing.T) {
	assert.True(t, IsSeedanceChannelType(constant.ChannelTypeDoubaoVideo))
	assert.True(t, IsSeedanceChannelType(constant.ChannelTypeVolcEngine))
	assert.False(t, IsSeedanceChannelType(constant.ChannelTypeOpenAI))
	assert.False(t, IsSeedanceChannelType(constant.ChannelTypeKling))

	assert.True(t, model.IsSeedanceTaskPlatform(constant.TaskPlatform("54")))
	assert.True(t, model.IsSeedanceTaskPlatform(constant.TaskPlatform("45")))
	assert.False(t, model.IsSeedanceTaskPlatform(constant.TaskPlatform("99")))
	assert.False(t, model.IsSeedanceTaskPlatform(constant.TaskPlatformSuno))
}

// ReleaseTaskSlotIfSeedance：仅 Seedance 任务触发释放；其他平台 no-op。
func TestReleaseTaskSlotIfSeedance(t *testing.T) {
	cleanupConcurrencyTestData(t)
	const limit = 3

	_, _, _ = model.ReserveTaskConcurrencySlot(900001, limit)

	// Seedance 任务 → 释放名额
	task := &model.Task{UserId: 900001, Platform: constant.TaskPlatform("54")}
	require.NoError(t, model.DB.Create(task).Error)
	ReleaseTaskSlotIfSeedance(task)
	count, err := model.GetRunningCountForUser(900001)
	require.NoError(t, err)
	assert.Equal(t, 0, count)

	// 非 Seedance 任务 → no-op
	_, _, _ = model.ReserveTaskConcurrencySlot(900001, limit)
	ReleaseTaskSlotIfSeedance(&model.Task{UserId: 900001, Platform: constant.TaskPlatformSuno})
	count, _ = model.GetRunningCountForUser(900001)
	assert.Equal(t, 1, count, "非 Seedance 任务不得释放 Seedance 名额")
}

// 释放幂等性：同一任务被多个终态路径处理时，只有第一次真正递减计数。
// 若不做任务级标记，重复释放会让计数虚低，用户可借此超过上限。
func TestReleaseTaskSlotIfSeedanceIdempotent(t *testing.T) {
	cleanupConcurrencyTestData(t)
	const uid, limit = 900001, 3

	// 3 个预留对应 3 个任务
	for i := 0; i < limit; i++ {
		ok, _, err := model.ReserveTaskConcurrencySlot(uid, limit)
		require.NoError(t, err)
		require.True(t, ok)
	}
	tasks := make([]*model.Task, 0, limit)
	for i := 0; i < limit; i++ {
		tk := &model.Task{TaskID: "seed_idem", UserId: uid, Platform: constant.TaskPlatform("54"), Status: model.TaskStatusQueued}
		require.NoError(t, model.DB.Create(tk).Error)
		tasks = append(tasks, tk)
	}

	// 同一任务被两个终态路径"并发"处理（sweep + 单任务轮询 / 批量失败）：
	// 释放两次，只允许减一次 → 计数 2（而非 1）。
	ReleaseTaskSlotIfSeedance(tasks[0])
	ReleaseTaskSlotIfSeedance(tasks[0])
	// 另外两个任务正常终态释放
	ReleaseTaskSlotIfSeedance(tasks[1])
	ReleaseTaskSlotIfSeedance(tasks[2])

	count, err := model.GetRunningCountForUser(uid)
	require.NoError(t, err)
	assert.Equal(t, 0, count, "重复释放同一任务不得重复递减计数")

	// 标记落库：任务已标记为释放
	var reloaded model.Task
	require.NoError(t, model.DB.First(&reloaded, tasks[0].ID).Error)
	assert.True(t, reloaded.ConcurrencyReleased)
}

// 预留→创建任务→终态释放 全生命周期：名额转移给任务后由终态路径释放。
func TestSeedanceSlotLifecycle(t *testing.T) {
	cleanupConcurrencyTestData(t)
	const uid, limit = 900001, 3

	// ① 创建请求预留名额（RelayTask 重试循环内）
	ok, cur, err := model.ReserveTaskConcurrencySlot(uid, limit)
	require.NoError(t, err)
	assert.True(t, ok)
	assert.Equal(t, 1, cur)

	// ② 上游成功，任务行落库（controller 成功分支，concurrencySlotTransferred=true）
	task := &model.Task{
		TaskID:   "seed_lifecycle",
		UserId:   uid,
		Platform: constant.TaskPlatform("54"),
		Status:   model.TaskStatusQueued,
		Data:     []byte(`{}`),
	}
	require.NoError(t, model.DB.Create(task).Error)

	// 此时名额已转移给任务：defer 不应再释放（模拟 controller defer 的行存在检测）
	count, _ := model.GetRunningCountForUser(uid)
	assert.Equal(t, 1, count, "任务行已创建，名额不得被请求级 defer 释放")

	// ③ 轮询器发现任务成功：先 CAS 持久化终态（真实链路），再释放名额
	task.Status = model.TaskStatusSuccess
	task.Progress = "100%"
	won, err := task.UpdateWithStatus(model.TaskStatusQueued)
	require.NoError(t, err)
	require.True(t, won, "CAS 从 QUEUED → SUCCESS 必须胜出")
	var reloaded model.Task
	require.NoError(t, model.DB.First(&reloaded, task.ID).Error)
	// TaskStatusSuccess 为 untyped string 常量，须用 EqualValues 与 TaskStatus 比较
	assert.EqualValues(t, model.TaskStatusSuccess, reloaded.Status, "终态必须真实持久化到数据库")
	ReleaseTaskSlotIfSeedance(task)
	count, _ = model.GetRunningCountForUser(uid)
	assert.Equal(t, 0, count, "终态释放后名额归还")
}

// 上线存量任务漏算修复：首次预留即在锁内补齐存量运行任务数，硬限制即时生效，
// 不依赖五分钟对账。存量 2 个 + 上限 3：只允许再创建 1 个。
func TestReserveBackfillsExistingRunningTasks(t *testing.T) {
	cleanupConcurrencyTestData(t)
	const uid, limit = 900001, 3

	// 上线前已存在的 2 个运行中 Seedance 任务（无计数行）
	for i := 0; i < 2; i++ {
		require.NoError(t, model.DB.Create(&model.Task{
			TaskID:   "seed_existing",
			UserId:   uid,
			Platform: constant.TaskPlatform("54"),
			Status:   model.TaskStatusInProgress,
		}).Error)
	}

	// 第一次新预留：补齐到 2 再 +1 = 3（恰好达到上限）
	ok, cur, err := model.ReserveTaskConcurrencySlot(uid, limit)
	require.NoError(t, err)
	assert.True(t, ok, "存量 2 + 新增 1 = 3 = 上限，应允许")
	assert.Equal(t, 3, cur, "补齐后计数应含存量任务")

	// 第二次新预留：必须拒绝（硬限制即时生效，无需等待对账）
	ok2, cur2, err := model.ReserveTaskConcurrencySlot(uid, limit)
	require.NoError(t, err)
	assert.False(t, ok2, "存量任务必须占用名额")
	assert.Equal(t, 3, cur2)
}

// 429 重试不得刷新 updated_at：泄漏名额的行保持 stale，对账才能清理，
// 否则用户每次重试都让行"新鲜"，会被永久锁死。
func TestRejectedReservationDoesNotRefreshUpdatedAt(t *testing.T) {
	cleanupConcurrencyTestData(t)
	const uid, limit = 900001, 3

	// 3 次预留全部成功（无任务行 → 模拟 3 个泄漏/在途预留）
	for i := 0; i < limit; i++ {
		ok, _, err := model.ReserveTaskConcurrencySlot(uid, limit)
		require.NoError(t, err)
		require.True(t, ok)
	}
	var before model.TaskConcurrencySlot
	require.NoError(t, model.DB.Where("user_id = ?", uid).First(&before).Error)

	// 连续 5 次 429（拒绝预留）：updated_at 必须保持不变
	time.Sleep(2 * time.Millisecond) // 确保时间刻度可区分
	for i := 0; i < 5; i++ {
		ok, _, err := model.ReserveTaskConcurrencySlot(uid, limit)
		require.NoError(t, err)
		require.False(t, ok)
	}
	var after model.TaskConcurrencySlot
	require.NoError(t, model.DB.Where("user_id = ?", uid).First(&after).Error)
	assert.Equal(t, before.UpdatedAt, after.UpdatedAt, "被 429 拒绝的预留不得刷新 updated_at（否则泄漏永久化）")

	// 行变为 stale 后，对账可清理泄漏名额 → 用户解锁
	require.NoError(t, model.DB.Model(&model.TaskConcurrencySlot{}).
		Where("user_id = ?", uid).Update("updated_at", time.Now().Unix()-3600).Error)
	fixed, err := model.ReconcileTaskConcurrencySlots(30 * time.Minute)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, fixed, 1)
	count, _ := model.GetRunningCountForUser(uid)
	assert.Equal(t, 0, count, "泄漏名额清理后用户应能重新创建")
}

// 对账不得覆盖并发预留：预留后行新鲜（updated_at 被成功预留刷新），
// 即使计数高于真实运行数（在途预留），对账也必须跳过。
func TestReconcileDoesNotOverwriteFreshReservation(t *testing.T) {
	cleanupConcurrencyTestData(t)
	const uid, limit = 900001, 10

	// 制造"计数虚高但新鲜"：先预留（刷新 updated_at），再人为改 stale，
	// 随后再次成功预留（updated_at 又变新鲜）
	_, _, _ = model.ReserveTaskConcurrencySlot(uid, limit)
	require.NoError(t, model.DB.Model(&model.TaskConcurrencySlot{}).
		Where("user_id = ?", uid).Update("updated_at", time.Now().Unix()-3600).Error)
	_, _, _ = model.ReserveTaskConcurrencySlot(uid, limit) // 成功 → updated_at 刷新为新鲜

	fixed, err := model.ReconcileTaskConcurrencySlots(30 * time.Minute)
	require.NoError(t, err)
	assert.Equal(t, 0, fixed, "新鲜的在途预留不得被对账覆盖（丢失更新）")
	count, _ := model.GetRunningCountForUser(uid)
	assert.Equal(t, 2, count)
}

// 恢复记录持有名额（outcome_unknown / 落库失败场景）：上游可能/确定已创建任务，
// 名额不能随请求结束释放；对账在恢复记录超时后清理——即使计数行被用户其他
// 活跃预留刷新为"新鲜"，超时恢复记录也必须独立触发清理（否则泄漏被永久保活）。
func TestRecoveryRecordHoldsSlotUntilExpiry(t *testing.T) {
	cleanupConcurrencyTestData(t)
	const uid, limit = 900001, 3

	// 预留 1 个名额 + 创建占名额恢复记录（模拟 persistOutcomeUnknownRecovery）
	ok, _, err := model.ReserveTaskConcurrencySlot(uid, limit)
	require.NoError(t, err)
	require.True(t, ok)
	rec := &model.TaskSubmitRecovery{
		UserId:             uid,
		Platform:           "54",
		Outcome:            "outcome_unknown",
		Status:             model.TaskRecoveryStatusUnknown,
		ConcurrencyReserved: true,
	}
	require.NoError(t, rec.Insert())

	// 恢复记录未超时：计数(1) == 已知占用(1)，对账 no-op，名额保持
	fixed, err := model.ReconcileTaskConcurrencySlots(30 * time.Minute)
	require.NoError(t, err)
	assert.Equal(t, 0, fixed, "未超时的恢复记录名额不得被清理")
	count, _ := model.GetRunningCountForUser(uid)
	assert.Equal(t, 1, count)

	// 恢复记录超时（CreatedAt 改为 1 小时前）
	require.NoError(t, model.DB.Model(&model.TaskSubmitRecovery{}).
		Where("id = ?", rec.ID).Update("created_at", time.Now().Unix()-3600).Error)

	// 模拟"活跃用户"：恢复记录占用期间继续创建成功（计数 1→2），
	// 该成功预留把计数行 updated_at 刷新为新鲜 → 行不 stale。
	ok2, _, err := model.ReserveTaskConcurrencySlot(uid, limit)
	require.NoError(t, err)
	require.True(t, ok2)

	// 对账必须只释放过期恢复记录占用的名额：known = 任务行 0 + 未超时恢复记录 0 = 0，
	// 消费过期记录 1 条 → newCount = max(2-1, 0) = 1 → 新鲜在途预留被保留。
	fixed2, err := model.ReconcileTaskConcurrencySlots(30 * time.Minute)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, fixed2, 1, "过期恢复记录必须被消费")
	count2, _ := model.GetRunningCountForUser(uid)
	assert.Equal(t, 1, count2, "必须只释放过期恢复记录的名额，保留新鲜在途预留")
}

// 审查场景：过期恢复记录 + 新鲜在途预留（已预留但任务行尚未落库）。
// 对账只能消费过期恢复记录并释放其 1 个名额，绝不能把新鲜预留一起覆盖。
func TestReconcileExpiredRecoveryKeepsFreshReservation(t *testing.T) {
	cleanupConcurrencyTestData(t)
	const uid, limit = 900001, 3

	// 正常流程构造：预留名额 → 恢复记录接管（名额在计数里）→ 人为使其过期
	ok, _, err := model.ReserveTaskConcurrencySlot(uid, limit) // count=1
	require.NoError(t, err)
	require.True(t, ok)
	rec := &model.TaskSubmitRecovery{
		UserId:             uid,
		Platform:           "54",
		Outcome:            "outcome_unknown",
		Status:             model.TaskRecoveryStatusUnknown,
		ConcurrencyReserved: true,
	}
	require.NoError(t, rec.Insert())
	require.NoError(t, model.DB.Model(&model.TaskSubmitRecovery{}).
		Where("id = ?", rec.ID).Update("created_at", time.Now().Unix()-3600).Error)

	// 新请求预留成功（新鲜在途预留，任务行尚未落库）→ count=2
	ok2, _, err := model.ReserveTaskConcurrencySlot(uid, limit)
	require.NoError(t, err)
	require.True(t, ok2)

	// 对账：只能释放过期恢复记录的 1 个名额，保留新鲜预留
	fixed, err := model.ReconcileTaskConcurrencySlots(30 * time.Minute)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, fixed, 1)
	count, _ := model.GetRunningCountForUser(uid)
	assert.Equal(t, 1, count, "新鲜在途预留必须被保留，不得被过期恢复记录触发整体覆盖")

	// 过期恢复记录标记已被原子清除：后续对账不再重复触发清理
	var recs []model.TaskSubmitRecovery
	require.NoError(t, model.DB.Where("user_id = ?", uid).Find(&recs).Error)
	require.Len(t, recs, 1)
	assert.False(t, recs[0].ConcurrencyReserved, "过期恢复记录的占名额标记必须被清除")

	// 再次对账：expiredCleared=0，count=1 为新鲜预留，无变化
	fixed2, err := model.ReconcileTaskConcurrencySlots(30 * time.Minute)
	require.NoError(t, err)
	assert.Equal(t, 0, fixed2, "标记清除后不得再次触发清理")
	count2, _ := model.GetRunningCountForUser(uid)
	assert.Equal(t, 1, count2)
}

// 无计数行但有占名额恢复记录的用户：对账补建计数行。
func TestReconcileBackfillsRecoveryReservedSlot(t *testing.T) {
	cleanupConcurrencyTestData(t)
	const uid = 900001

	rec := &model.TaskSubmitRecovery{
		UserId:             uid,
		Platform:           "54",
		Outcome:            "outcome_unknown",
		Status:             model.TaskRecoveryStatusUnknown,
		ConcurrencyReserved: true,
	}
	require.NoError(t, rec.Insert())

	fixed, err := model.ReconcileTaskConcurrencySlots(30 * time.Minute)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, fixed, 1)
	count, _ := model.GetRunningCountForUser(uid)
	assert.Equal(t, 1, count, "占名额恢复记录应被补建进计数")
}

// 原子释放：MarkTaskSlotReleasedAndDecrement 返回 released 语义——
// 首次释放 true，重复释放 false 且不再递减。
func TestMarkTaskSlotReleasedAndDecrement(t *testing.T) {
	cleanupConcurrencyTestData(t)
	const uid, limit = 900001, 3

	_, _, _ = model.ReserveTaskConcurrencySlot(uid, limit)
	task := &model.Task{TaskID: "seed_atomic", UserId: uid, Platform: constant.TaskPlatform("54"), Status: model.TaskStatusQueued}
	require.NoError(t, model.DB.Create(task).Error)

	released1, err := model.MarkTaskSlotReleasedAndDecrement(task)
	require.NoError(t, err)
	assert.True(t, released1)
	count, _ := model.GetRunningCountForUser(uid)
	assert.Equal(t, 0, count)

	// 重复释放：标记已置 true → no-op
	released2, err := model.MarkTaskSlotReleasedAndDecrement(task)
	require.NoError(t, err)
	assert.False(t, released2, "重复释放必须 no-op")
	count, _ = model.GetRunningCountForUser(uid)
	assert.Equal(t, 0, count, "重复释放不得再次递减")
}

// 配置读取与测试注入：默认取常量值；注入覆盖后返回覆盖值。
func TestMaxConcurrentSeedanceTasks(t *testing.T) {
	orig := constant.SeedanceMaxConcurrentTasks
	constant.SeedanceMaxConcurrentTasks = 7
	defer func() { constant.SeedanceMaxConcurrentTasks = orig }()

	setMaxConcurrentTasksForTest(0)
	assert.Equal(t, 7, MaxConcurrentSeedanceTasks())

	setMaxConcurrentTasksForTest(5)
	assert.Equal(t, 5, MaxConcurrentSeedanceTasks())

	setMaxConcurrentTasksForTest(0)
	assert.Equal(t, 7, MaxConcurrentSeedanceTasks())
}
