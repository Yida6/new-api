package model

import (
	"testing"

	"github.com/QuantumNous/new-api/constant"
	commonRelay "github.com/QuantumNous/new-api/relay/common"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// 幂等键必须随任务一起持久化（TaskPrivateData.IdempotencyKey），
// 供进程重启后查询确认 / 排查重复创建使用。
func TestInitTaskPersistsIdempotencyKey(t *testing.T) {
	info := &commonRelay.RelayInfo{
		UserId: 1,
		ChannelMeta: &commonRelay.ChannelMeta{
			ChannelId: 42,
		},
		TaskRelayInfo: &commonRelay.TaskRelayInfo{
			PublicTaskID:   "task_public123",
			IdempotencyKey: "11111111-1111-4111-8111-111111111111",
		},
	}

	task := InitTask(constant.TaskPlatform("doubao-video"), info)
	require.NotNil(t, task)
	assert.Equal(t, "task_public123", task.TaskID)
	assert.Equal(t, "11111111-1111-4111-8111-111111111111", task.PrivateData.IdempotencyKey,
		"幂等键必须随任务持久化")
}

// 未生成幂等键（非任务提交路径）时，持久化字段保持为空，不影响既有行为。
func TestInitTaskWithoutIdempotencyKey(t *testing.T) {
	info := &commonRelay.RelayInfo{
		UserId: 1,
		ChannelMeta: &commonRelay.ChannelMeta{
			ChannelId: 42,
		},
		TaskRelayInfo: &commonRelay.TaskRelayInfo{PublicTaskID: "task_public456"},
	}
	task := InitTask(constant.TaskPlatform("doubao-video"), info)
	require.NotNil(t, task)
	assert.Empty(t, task.PrivateData.IdempotencyKey)
}
