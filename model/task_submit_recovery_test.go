package model

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// 人工重试占位必须原子且互斥：并发/双击时只有一个请求能进入创建流程。
func TestMarkRecoveryRecreatedAtomic(t *testing.T) {
	require.NoError(t, DB.AutoMigrate(&TaskSubmitRecovery{}))

	rec := &TaskSubmitRecovery{
		UserId:  1,
		Model:   "m",
		Outcome: "outcome_unknown",
		Status:  TaskRecoveryStatusUnknown,
	}
	require.NoError(t, rec.Insert())

	// 第一次占位成功
	claimed, err := MarkRecoveryRecreated(rec.ID, 1, "user confirmed retry")
	require.NoError(t, err)
	assert.True(t, claimed, "unknown 状态的记录应能被原子占位")

	// 第二次占位必须失败（已被前一次占位 → 状态已是 recreated）
	claimed2, err := MarkRecoveryRecreated(rec.ID, 1, "duplicate claim")
	require.NoError(t, err)
	assert.False(t, claimed2, "已占位（recreated）的记录不得被再次占位")

	// 其他用户不可占位
	claimed3, err := MarkRecoveryRecreated(rec.ID, 999, "other user")
	require.NoError(t, err)
	assert.False(t, claimed3, "其他用户不得占位")

	// 已 associated 的记录不可占位
	rec2 := &TaskSubmitRecovery{
		UserId:   1,
		Model:    "m",
		Outcome:  "confirmed_success",
		Status:   TaskRecoveryStatusAssociated,
		UpstreamTaskID: "cgt-x",
	}
	require.NoError(t, rec2.Insert())
	claimed4, err := MarkRecoveryRecreated(rec2.ID, 1, "should fail")
	require.NoError(t, err)
	assert.False(t, claimed4, "已关联上游任务的记录不得重新创建")
}

// 结果回填只更新备注，不影响状态机。
func TestUpdateRecoveryNote(t *testing.T) {
	require.NoError(t, DB.AutoMigrate(&TaskSubmitRecovery{}))

	rec := &TaskSubmitRecovery{
		UserId:  1,
		Model:   "m",
		Outcome: "outcome_unknown",
		Status:  TaskRecoveryStatusRecreated,
	}
	require.NoError(t, rec.Insert())

	require.NoError(t, UpdateRecoveryNote(rec.ID, 1, "人工重试已完成：public_task_id=task_x"))
	got, err := GetTaskSubmitRecoveryByID(rec.ID, 1)
	require.NoError(t, err)
	assert.Equal(t, TaskRecoveryStatusRecreated, got.Status, "回填备注不得改变状态")
	assert.Contains(t, got.Note, "task_x")
}

// 候选发现结果的条件更新：非终态可写；已被并发 recreate/associate 占位后必须失败。
func TestUpdateRecoveryDiscoveryResultConditional(t *testing.T) {
	require.NoError(t, DB.AutoMigrate(&TaskSubmitRecovery{}))

	rec := &TaskSubmitRecovery{UserId: 1, Model: "m", Outcome: "outcome_unknown", Status: TaskRecoveryStatusUnknown}
	require.NoError(t, rec.Insert())

	// 非终态（unknown）→ 可写
	ok, err := UpdateRecoveryDiscoveryResult(rec.ID, 1, TaskRecoveryStatusInferred, "[]", "候选")
	require.NoError(t, err)
	assert.True(t, ok)

	// 并发 recreate 占位后 → 必须失败（discover 不得覆盖 recreated）
	claimed, err := MarkRecoveryRecreated(rec.ID, 1, "concurrent recreate")
	require.NoError(t, err)
	require.True(t, claimed)
	ok2, err := UpdateRecoveryDiscoveryResult(rec.ID, 1, TaskRecoveryStatusAmbiguous, "[1]", "不应写入")
	require.NoError(t, err)
	assert.False(t, ok2, "终态 recreated 上 discover 结果不得写入")

	got, err := GetTaskSubmitRecoveryByID(rec.ID, 1)
	require.NoError(t, err)
	assert.Equal(t, TaskRecoveryStatusRecreated, got.Status)
	assert.Equal(t, "[]", got.Candidates, "被丢弃的发现结果不得写入")
}

// 关联的条件更新：与人工重试互斥（只允许一种操作成功）。
func TestMarkRecoveryAssociatedConditional(t *testing.T) {
	require.NoError(t, DB.AutoMigrate(&TaskSubmitRecovery{}))

	rec := &TaskSubmitRecovery{UserId: 1, Model: "m", Outcome: "outcome_unknown", Status: TaskRecoveryStatusUnknown}
	require.NoError(t, rec.Insert())

	// 非终态 → 关联成功
	ok, err := MarkRecoveryAssociated(rec.ID, 1, "cgt-x", "关联")
	require.NoError(t, err)
	assert.True(t, ok)

	// 已关联后再关联 → 失败
	ok2, err := MarkRecoveryAssociated(rec.ID, 1, "cgt-y", "重复关联")
	require.NoError(t, err)
	assert.False(t, ok2)

	// 已关联后再人工重试 → 失败（互斥）
	claimed, err := MarkRecoveryRecreated(rec.ID, 1, "recreate")
	require.NoError(t, err)
	assert.False(t, claimed, "已关联的记录不得被人工重试占位")

	// 被 recreate 占位的记录不得关联（反向互斥）
	rec2 := &TaskSubmitRecovery{UserId: 1, Model: "m", Outcome: "outcome_unknown", Status: TaskRecoveryStatusUnknown}
	require.NoError(t, rec2.Insert())
	claimed2, err := MarkRecoveryRecreated(rec2.ID, 1, "recreate first")
	require.NoError(t, err)
	assert.True(t, claimed2)
	ok3, err := MarkRecoveryAssociated(rec2.ID, 1, "cgt-z", "关联")
	require.NoError(t, err)
	assert.False(t, ok3, "recreated 状态不得被关联覆盖")
}

// 人工重试明确失败后重新打开：仅 recreated 可被重置为 unknown（可执行的恢复路径）。
func TestResetRecoveryForRetry(t *testing.T) {
	require.NoError(t, DB.AutoMigrate(&TaskSubmitRecovery{}))

	rec := &TaskSubmitRecovery{UserId: 1, Model: "m", Outcome: "outcome_unknown", Status: TaskRecoveryStatusRecreated}
	require.NoError(t, rec.Insert())

	require.NoError(t, ResetRecoveryForRetry(rec.ID, 1, "重试失败，已重新打开"))
	got, err := GetTaskSubmitRecoveryByID(rec.ID, 1)
	require.NoError(t, err)
	assert.Equal(t, TaskRecoveryStatusUnknown, got.Status, "失败后应重新打开为 unknown")
	assert.Contains(t, got.Note, "重新打开")

	// 非 recreated（如 associated）不可被重置
	rec2 := &TaskSubmitRecovery{UserId: 1, Model: "m", Outcome: "confirmed_success", Status: TaskRecoveryStatusAssociated}
	require.NoError(t, rec2.Insert())
	require.NoError(t, ResetRecoveryForRetry(rec2.ID, 1, "不应生效"))
	got2, err := GetTaskSubmitRecoveryByID(rec2.ID, 1)
	require.NoError(t, err)
	assert.Equal(t, TaskRecoveryStatusAssociated, got2.Status, "非 recreated 状态不得被重置")
}
