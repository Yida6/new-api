package common

import (
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// 本地幂等键：生成、复用、新任务新键
// ---------------------------------------------------------------------------

func TestEnsureIdempotencyKeyFirstCallGeneratesUUIDv4(t *testing.T) {
	info := &RelayInfo{TaskRelayInfo: &TaskRelayInfo{}}
	key := info.EnsureTaskIdempotencyKey()
	require.NotEmpty(t, key, "首次创建必须生成幂等键")
	parsed, err := uuid.Parse(key)
	require.NoError(t, err, "幂等键必须是合法 UUID: %q", key)
	assert.Equal(t, uuid.Version(4), parsed.Version(), "幂等键应为 UUID v4")
}

// 同一次任务（同一 RelayInfo）的自动重试（含超时重试、渠道切换重试）必须复用同一个键。
func TestEnsureIdempotencyKeyReusedOnRetries(t *testing.T) {
	info := &RelayInfo{TaskRelayInfo: &TaskRelayInfo{}}
	first := info.EnsureTaskIdempotencyKey()
	for i := 0; i < 5; i++ {
		got := info.EnsureTaskIdempotencyKey()
		assert.Equal(t, first, got, "重试 %d 次后幂等键必须保持不变（超时重试不能生成新键）", i+1)
	}
}

// 显式的超时场景：创建请求超时但结果未知时，重试使用相同幂等键，不生成新键。
func TestEnsureIdempotencyKeyReusedAfterTimeoutRetry(t *testing.T) {
	info := &RelayInfo{TaskRelayInfo: &TaskRelayInfo{}}
	keyBeforeTimeout := info.EnsureTaskIdempotencyKey()

	// 模拟 DoRequest 超时（结果未知）后的自动重试
	keyAfterTimeoutRetry := info.EnsureTaskIdempotencyKey()

	assert.Equal(t, keyBeforeTimeout, keyAfterTimeoutRetry,
		"超时后重试必须复用原幂等键，不能生成新键")
}

// 用户发起全新任务（新的 HTTP 请求 → 新的 RelayInfo）时必须生成新的幂等键。
func TestEnsureIdempotencyKeyDifferentForNewTask(t *testing.T) {
	info1 := &RelayInfo{TaskRelayInfo: &TaskRelayInfo{}}
	info2 := &RelayInfo{TaskRelayInfo: &TaskRelayInfo{}}
	k1 := info1.EnsureTaskIdempotencyKey()
	k2 := info2.EnsureTaskIdempotencyKey()
	assert.NotEqual(t, k1, k2, "全新逻辑任务必须使用不同的幂等键")
}

func TestEnsureIdempotencyKeyNilSafe(t *testing.T) {
	var nilInfo *RelayInfo
	assert.Empty(t, nilInfo.EnsureTaskIdempotencyKey())
	assert.Empty(t, (&RelayInfo{}).EnsureTaskIdempotencyKey())
	var nilTaskRelay *TaskRelayInfo
	assert.Empty(t, nilTaskRelay.EnsureIdempotencyKey())
}

// ---------------------------------------------------------------------------
// X-Client-Request-Id：生成、复用（仅日志串联，非幂等键）
// ---------------------------------------------------------------------------

func TestEnsureClientRequestIDReusedOnRetries(t *testing.T) {
	info := &RelayInfo{TaskRelayInfo: &TaskRelayInfo{}}
	first := info.EnsureTaskClientRequestID()
	require.NotEmpty(t, first)
	parsed, err := uuid.Parse(first)
	require.NoError(t, err)
	assert.Equal(t, uuid.Version(4), parsed.Version())

	for i := 0; i < 3; i++ {
		assert.Equal(t, first, info.EnsureTaskClientRequestID(),
			"同一次逻辑任务的 X-Client-Request-Id 必须在重试中复用（用于日志串联）")
	}

	info2 := &RelayInfo{TaskRelayInfo: &TaskRelayInfo{}}
	assert.NotEqual(t, first, info2.EnsureTaskClientRequestID(), "全新任务必须生成新的 X-Client-Request-Id")
}

// 幂等键与 X-Client-Request-Id 是相互独立的两个值（职责分离：去重 vs 追踪）。
func TestIdempotencyKeyAndClientRequestIDAreDistinct(t *testing.T) {
	info := &RelayInfo{TaskRelayInfo: &TaskRelayInfo{}}
	assert.NotEqual(t, info.EnsureTaskIdempotencyKey(), info.EnsureTaskClientRequestID())
}

// ---------------------------------------------------------------------------
// 内容指纹（候选发现的模糊匹配依据）
// ---------------------------------------------------------------------------

func TestSubmitContentFingerprintStableAndSensitiveFree(t *testing.T) {
	req := TaskSubmitReq{
		Model:  "doubao-seedance-2-0-260128",
		Prompt: "一只柯基在草地上奔跑",
		Images: []string{"data:image/png;base64,AAAA"},
	}
	fp := SubmitContentFingerprint(req)
	require.NotEmpty(t, fp)
	// 指纹不含敏感原文
	assert.NotContains(t, fp, "柯基")
	assert.NotContains(t, fp, "base64")

	// 相同内容 → 相同指纹
	assert.Equal(t, fp, SubmitContentFingerprint(TaskSubmitReq{
		Model:  "doubao-seedance-2-0-260128",
		Prompt: "一只柯基在草地上奔跑",
		Images: []string{"data:image/png;base64,BBBB"}, // 图片内容变化不影响指纹（只计数）
	}))

	// 不同内容 → 不同指纹
	assert.NotEqual(t, fp, SubmitContentFingerprint(TaskSubmitReq{
		Model:  "doubao-seedance-2-0-260128",
		Prompt: "一只猫在奔跑",
		Images: []string{"data:image/png;base64,AAAA"},
	}))

	// 与上游任务项同构：模型 + 文本 + 图片数量
	upstreamFP := ContentFingerprintFromParts("doubao-seedance-2-0-260128", []string{"一只柯基在草地上奔跑"}, 1)
	assert.Equal(t, fp, upstreamFP, "客户端与上游候选指纹必须同构，才能用于模糊匹配")
}
