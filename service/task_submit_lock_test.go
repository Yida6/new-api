package service

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"

	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func newSubmitCtx(t *testing.T, userID, tokenID int, body string) *gin.Context {
	t.Helper()
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/videos", strings.NewReader(body))
	c.Set(string(constant.ContextKeyUserId), userID)
	c.Set(string(constant.ContextKeyTokenId), tokenID)
	return c
}

// 相同用户 + 相同 token + 相同请求体 → 相同指纹；不同用户/token → 不同指纹
// （相同请求体在不同用户之间不会被错误合并）。
func TestSubmitFingerprintUserScoped(t *testing.T) {
	body := `{"model":"doubao-seedance-2-0-260128","prompt":"一只柯基在草地上奔跑"}`
	f1 := SubmitFingerprint(newSubmitCtx(t, 7, 11, body))
	f2 := SubmitFingerprint(newSubmitCtx(t, 7, 11, body))
	f3 := SubmitFingerprint(newSubmitCtx(t, 8, 11, body)) // 不同用户
	f4 := SubmitFingerprint(newSubmitCtx(t, 7, 12, body)) // 不同 token

	require.NotEmpty(t, f1)
	assert.Equal(t, f1, f2, "相同提交指纹必须一致")
	assert.NotEqual(t, f1, f3, "不同用户的相同请求体不得互相合并")
	assert.NotEqual(t, f1, f4, "不同 token 不得互相合并")
}

// 进程内锁：在途重复被拒绝；完成后宽限期内仍拒绝；宽限期过后放行（超窗不误合并）。
func TestMemorySubmitLockGraceAndWindow(t *testing.T) {
	origGrace := submitLockGraceTTL
	submitLockGraceTTL = 20 * time.Millisecond
	defer func() { submitLockGraceTTL = origGrace }()

	lock := newMemorySubmitLock()
	ctx := context.Background()
	key := "fp:user1:hash1"

	ok, err := lock.Acquire(ctx, key, "token-a")
	require.NoError(t, err)
	require.True(t, ok, "首次获取应成功")

	ok2, err := lock.Acquire(ctx, key, "token-b")
	require.NoError(t, err)
	assert.False(t, ok2, "在途重复提交必须被拒绝")

	require.NoError(t, lock.Release(ctx, key, "token-a"))
	ok3, err := lock.Acquire(ctx, key, "token-c")
	require.NoError(t, err)
	assert.False(t, ok3, "完成后宽限期内相同提交仍应被拒绝（双击保护）")

	time.Sleep(submitLockGraceTTL + 30*time.Millisecond)
	ok4, err := lock.Acquire(ctx, key, "token-d")
	require.NoError(t, err)
	assert.True(t, ok4, "宽限期过后（去重窗口外）应允许再次提交")
}

// 进程内锁：不同指纹互不干扰。
func TestMemorySubmitLockDifferentKeysAllowed(t *testing.T) {
	lock := newMemorySubmitLock()
	ctx := context.Background()

	ok1, _ := lock.Acquire(ctx, "fp:a", "t1")
	ok2, _ := lock.Acquire(ctx, "fp:b", "t2")
	assert.True(t, ok1)
	assert.True(t, ok2, "不同指纹不应互相拦截")
}

// 进程内锁：原持有者锁过期被清理后，新持有者拿到锁；旧持有者的迟到 Release
// （令牌不匹配）不得误释放新持有者的锁。
func TestMemorySubmitLockStaleReleaseWithWrongToken(t *testing.T) {
	lock := newMemorySubmitLock()
	ctx := context.Background()
	key := "fp:stale"

	okA, err := lock.Acquire(ctx, key, "token-old")
	require.NoError(t, err)
	require.True(t, okA)

	// 模拟旧持有者请求卡死后锁过期被清理
	lock.mu.Lock()
	delete(lock.entries, key)
	lock.mu.Unlock()

	// 新持有者拿到锁
	okB, err := lock.Acquire(ctx, key, "token-new")
	require.NoError(t, err)
	require.True(t, okB, "旧锁清理后新持有者应能获锁")

	// 旧持有者的迟到 Release（错误令牌）→ 必须 no-op
	require.NoError(t, lock.Release(ctx, key, "token-old"))

	// 新持有者的锁仍然有效：相同指纹仍被拒绝
	okC, err := lock.Acquire(ctx, key, "token-other")
	require.NoError(t, err)
	assert.False(t, okC, "旧 Release 不得误释放新持有者的锁")
}

// 数据库唯一约束锁：模拟多实例并发，相同指纹只能有一个请求进入创建流程；
// 不同指纹可同时持有。宽限期与清理语义与进程内锁一致；Release 校验 owner token。
func TestDBSubmitLockMultiInstanceExclusion(t *testing.T) {
	origDB := model.DB
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	model.DB = db
	t.Cleanup(func() { model.DB = origDB })
	require.NoError(t, db.AutoMigrate(&model.TaskSubmitLockRow{}))

	origGrace := submitLockGraceTTL
	submitLockGraceTTL = 20 * time.Millisecond
	defer func() { submitLockGraceTTL = origGrace }()

	lock1 := &dbSubmitLock{} // 实例 A
	lock2 := &dbSubmitLock{} // 实例 B（共享同一数据库 = 共享存储）
	ctx := context.Background()

	// 实例 A 获取锁
	okA, err := lock1.Acquire(ctx, "fp:shared:1", "token-a")
	require.NoError(t, err)
	require.True(t, okA, "多实例下第一个请求应获得锁")

	// 实例 B 相同指纹 → 必须失败（唯一约束）
	okB, err := lock2.Acquire(ctx, "fp:shared:1", "token-b")
	require.NoError(t, err)
	assert.False(t, okB, "多实例并发提交只能有一个请求进入创建流程")

	// 不同指纹 → 双方可同时持有
	okB2, err := lock2.Acquire(ctx, "fp:shared:2", "token-b2")
	require.NoError(t, err)
	assert.True(t, okB2)

	// A 完成后（宽限期内）B 仍拿不到同指纹
	require.NoError(t, lock1.Release(ctx, "fp:shared:1", "token-a"))
	okB3, err := lock2.Acquire(ctx, "fp:shared:1", "token-b3")
	require.NoError(t, err)
	assert.False(t, okB3, "宽限期内相同提交仍应被拒绝")

	// 宽限期过后：A 再次提交可获锁（去重窗口外不误合并）
	time.Sleep(submitLockGraceTTL + 30*time.Millisecond)
	okA2, err := lock1.Acquire(ctx, "fp:shared:1", "token-a2")
	require.NoError(t, err)
	assert.True(t, okA2, "宽限期过后应允许再次提交")
}

// DB 锁：旧持有者锁过期被清理、新持有者获锁后，旧持有者的迟到 Release
// （错误令牌）不得误释放新持有者的锁。
func TestDBSubmitLockStaleReleaseWithWrongToken(t *testing.T) {
	origDB := model.DB
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	model.DB = db
	t.Cleanup(func() { model.DB = origDB })
	require.NoError(t, db.AutoMigrate(&model.TaskSubmitLockRow{}))

	lock := &dbSubmitLock{}
	ctx := context.Background()
	key := "fp:db-stale"

	okA, err := lock.Acquire(ctx, key, "token-old")
	require.NoError(t, err)
	require.True(t, okA)

	// 模拟旧持有者请求卡死后锁过期被清理
	require.NoError(t, model.DB.Where("fingerprint = ?", key).Delete(&model.TaskSubmitLockRow{}).Error)

	// 新持有者拿到锁
	okB, err := lock.Acquire(ctx, key, "token-new")
	require.NoError(t, err)
	require.True(t, okB, "旧锁清理后新持有者应能获锁")

	// 旧持有者的迟到 Release（错误令牌）→ 必须 no-op
	require.NoError(t, lock.Release(ctx, key, "token-old"))

	// 新持有者的锁仍然有效
	okC, err := lock.Acquire(ctx, key, "token-other")
	require.NoError(t, err)
	assert.False(t, okC, "旧 Release 不得误释放新持有者的锁")
}

// Redis 锁后端：有 Redis 环境时验证 SETNX 互斥语义（无 Redis 时跳过）。
func TestRedisSubmitLockWhenAvailable(t *testing.T) {
	if !common.RedisEnabled || common.RDB == nil {
		t.Skip("redis not enabled, skip")
	}
	lock := &redisSubmitLock{}
	ctx := context.Background()
	key := "task_submit_lock:test:" + time.Now().Format("150405.000000000")

	ok1, err := lock.Acquire(ctx, key, "token-a")
	require.NoError(t, err)
	require.True(t, ok1)

	ok2, err := lock.Acquire(ctx, key, "token-b")
	require.NoError(t, err)
	assert.False(t, ok2, "Redis SETNX 下相同指纹应互斥")

	require.NoError(t, lock.Release(ctx, key, "token-a"))
}

// ---------------------------------------------------------------------------
// TryAcquireTaskSubmitLock 入口：共享后端故障必须 fail-closed（绝不放行）
// ---------------------------------------------------------------------------

type failingLockBackend struct{ err error }

func (f *failingLockBackend) Acquire(context.Context, string, string) (bool, error) {
	return false, f.err
}
func (f *failingLockBackend) Release(context.Context, string, string) error { return nil }

type stubLockBackend struct {
	acquireResult bool
	acquireCalled int
	released      bool
}

func (s *stubLockBackend) Acquire(context.Context, string, string) (bool, error) {
	s.acquireCalled++
	return s.acquireResult, nil
}
func (s *stubLockBackend) Release(context.Context, string, string) error { s.released = true; return nil }

// 共享后端故障 → fail-closed 返回错误，绝不静默降级为进程内锁放行。
func TestTryAcquireTaskSubmitLockFailClosedOnBackendError(t *testing.T) {
	setSubmitLockBackendForTest(&failingLockBackend{err: errors.New("redis down")})
	defer setSubmitLockBackendForTest(nil)

	c := newSubmitCtx(t, 1, 1, `{"model":"m","prompt":"p"}`)
	fp, release, acquired, err := TryAcquireTaskSubmitLock(c)
	require.Error(t, err, "共享锁故障必须返回错误（fail-closed）")
	assert.False(t, acquired)
	assert.Nil(t, release, "失败时不得返回可用的 release")
	assert.NotEmpty(t, fp)
}

// 成功获取与释放路径（release 携带 owner token 落到后端）。
func TestTryAcquireTaskSubmitLockSuccessAndRelease(t *testing.T) {
	backend := &stubLockBackend{acquireResult: true}
	setSubmitLockBackendForTest(backend)
	defer setSubmitLockBackendForTest(nil)

	c := newSubmitCtx(t, 1, 1, `{"model":"m","prompt":"p"}`)
	_, release, acquired, err := TryAcquireTaskSubmitLock(c)
	require.NoError(t, err)
	assert.True(t, acquired)
	assert.Equal(t, 1, backend.acquireCalled)
	release()
	assert.True(t, backend.released, "release 必须落到后端（含宽限期语义）")
}

// 重复提交（后端返回未获取）→ acquired=false 且无错误（调用方回 409）。
func TestTryAcquireTaskSubmitLockDuplicate(t *testing.T) {
	backend := &stubLockBackend{acquireResult: false}
	setSubmitLockBackendForTest(backend)
	defer setSubmitLockBackendForTest(nil)

	c := newSubmitCtx(t, 1, 1, `{"model":"m","prompt":"p"}`)
	_, release, acquired, err := TryAcquireTaskSubmitLock(c)
	require.NoError(t, err)
	assert.False(t, acquired)
	assert.Nil(t, release)
}
