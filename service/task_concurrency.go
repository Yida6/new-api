package service

import (
	"strconv"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
)

// ---------------------------------------------------------------------------
// 单用户 Seedance 任务并发名额限制（service 层门面）
//
// 职责：
//   - 读取限制配置（环境变量 SEEDANCE_MAX_CONCURRENT_TASKS，默认 3；0 为不限制）；
//   - 判定渠道/任务是否属于 Seedance 家族（doubao / volcengine 视频渠道）；
//   - 封装"创建前预留 + 终态释放 + 异常退出兜底对账"的对外入口。
//
// 原子性与多实例说明见 model/task_concurrency.go：预留/释放均基于共享数据库
// 行锁（SELECT ... FOR UPDATE），多实例部署下同一用户的并发创建被串行化，
// 不会绕过限制。
// ---------------------------------------------------------------------------

// MaxConcurrentSeedanceTasks 返回单个用户同时运行 Seedance 任务的上限。
// <= 0 表示不限制。测试可通过 setMaxConcurrentTasksForTest 覆盖。
func MaxConcurrentSeedanceTasks() int {
	if maxConcurrentTasksOverride > 0 {
		return maxConcurrentTasksOverride
	}
	return constant.SeedanceMaxConcurrentTasks
}

var maxConcurrentTasksOverride int

// setMaxConcurrentTasksForTest 测试注入专用（传 0 恢复配置值）。
func setMaxConcurrentTasksForTest(v int) {
	maxConcurrentTasksOverride = v
}

// IsSeedanceChannelType 判断渠道类型是否为 Seedance 家族
// （doubao 视频 / 火山方舟，二者共用 doubao 任务适配器）。
func IsSeedanceChannelType(channelType int) bool {
	return channelType == constant.ChannelTypeDoubaoVideo || channelType == constant.ChannelTypeVolcEngine
}

// IsSeedanceTask 判断任务记录是否属于 Seedance 家族（按 Task.Platform 判定）。
func IsSeedanceTask(task *model.Task) bool {
	if task == nil {
		return false
	}
	return model.IsSeedanceTaskPlatform(task.Platform)
}

// ReleaseTaskSlotIfSeedance 任务到达终态时释放并发名额（仅 Seedance 任务）。
//
// 幂等 + 原子保证：同一任务可能被多个终态处理路径（单任务轮询 CAS 胜出、超时
// 清理、批量失败）并发处理。释放委托给 model.MarkTaskSlotReleasedAndDecrement——
// "抢占任务级标记（concurrency_released false→true）+ 递减计数"在同一数据库
// 事务内一次性提交：标记抢占失败（RowsAffected==0）说明其他路径已释放，本路径
// no-op；若递减失败或进程中途退出，标记与计数一并回滚，后续路径可重试，
// 不会出现"标记已置但计数未减"的半释放态。
func ReleaseTaskSlotIfSeedance(task *model.Task) {
	if task == nil || task.ID == 0 || !IsSeedanceTask(task) {
		return
	}
	if _, err := model.MarkTaskSlotReleasedAndDecrement(task); err != nil {
		common.SysError("release task concurrency slot error: " + err.Error())
	}
}

// ReconcileTaskConcurrencySlots 兜底对账：修复泄漏/重复释放的名额计数。
// 阈值取 SEEDANCE_CONCURRENCY_RECONCILE_TTL_MINUTES（默认 30 分钟）。
func ReconcileTaskConcurrencySlots() {
	ttl := constant.SeedanceConcurrencyReconcileTTLMinutes
	if ttl <= 0 {
		ttl = 30
	}
	fixed, err := model.ReconcileTaskConcurrencySlots(time.Duration(ttl) * time.Minute)
	if err != nil {
		common.SysError("reconcile task concurrency slots error: " + err.Error())
		return
	}
	if fixed > 0 {
		common.SysLog("task concurrency slots reconciled: fixed " + strconv.Itoa(fixed) + " rows")
	}
}
