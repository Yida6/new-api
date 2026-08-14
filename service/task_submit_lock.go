package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// ---------------------------------------------------------------------------
// 客户端提交锁（去重范围：单实例或共享锁后端覆盖的实例集合）
//
// 目的：对"重复点击 / 并发重复提交"做短时间去重——同一用户、同一 token、
// 相同请求体的提交，在请求未完成（含完成后短宽限期）时直接拒绝，避免双击
// 导致多次 POST 创建多个任务。
//
// 边界（必须明确）：
//   - 提交锁只能防止"单实例或共享锁范围（同一 Redis / 同一数据库）内"的重复
//     提交；进程内 mutex/map 无法阻止多实例部署下不同实例的重复提交；
//   - 多实例部署时使用共享后端：Redis SETNX（优先）或数据库唯一约束；
//   - 该锁不能替代服务端幂等键（Seedance 未声明支持 Idempotency-Key）；
//   - 锁粒度是"请求体摘要"，客户端若每次点击都改变请求体（如注入时间戳）将
//     无法命中，因此仍应以官方任务查询作为最终确认手段。
// ---------------------------------------------------------------------------

var (
	// submitLockGraceTTL 请求完成后锁保留的宽限期，吸收网络延迟的双击重复请求。
	submitLockGraceTTL = 2 * time.Second
	// submitLockOrphanTTL release 未执行（进程异常/未捕获 panic）时锁的最长保留时间。
	submitLockOrphanTTL = 5 * time.Minute
)

// submitLockBackend 提交锁后端。Acquire 返回 false 表示该指纹已持锁（在途或宽限内）。
//
// ownerToken 是每次获取时生成的唯一令牌：Release 必须携带同一令牌，后端只在
// 令牌匹配时才释放/进入宽限期——防止"过期锁被迟到的旧 Release 误释放新持有者"
// （例如原持有者请求卡死后 TTL 过期、新请求拿到锁，随后旧请求的 Release 到达，
// 若无令牌校验会把新持有者的锁误删 → 重复提交失去拦截 → 重复创建）。
type submitLockBackend interface {
	Acquire(ctx context.Context, key, ownerToken string) (bool, error)
	Release(ctx context.Context, key, ownerToken string) error
}

// memorySubmitLock 进程内互斥（单实例场景 / 无共享后端的降级）。
type memorySubmitLock struct {
	mu      sync.Mutex
	entries map[string]memoryLockEntry // key → entry
}

type memoryLockEntry struct {
	expireAt   time.Time
	ownerToken string
}

func newMemorySubmitLock() *memorySubmitLock {
	return &memorySubmitLock{entries: make(map[string]memoryLockEntry)}
}

func (m *memorySubmitLock) Acquire(_ context.Context, key, ownerToken string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now()
	for k, e := range m.entries {
		if now.After(e.expireAt) {
			delete(m.entries, k)
		}
	}
	if e, ok := m.entries[key]; ok && now.Before(e.expireAt) {
		return false, nil
	}
	m.entries[key] = memoryLockEntry{expireAt: now.Add(submitLockOrphanTTL), ownerToken: ownerToken}
	return true, nil
}

func (m *memorySubmitLock) Release(_ context.Context, key, ownerToken string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.entries[key]
	if !ok {
		return nil // 已被清理（如已过期）→ 无需操作
	}
	if e.ownerToken != ownerToken {
		// 令牌不匹配：该锁已被其他请求持有，绝不能误释放/延长
		return nil
	}
	m.entries[key] = memoryLockEntry{expireAt: time.Now().Add(submitLockGraceTTL), ownerToken: ownerToken}
	return nil
}

// redisSubmitLock 基于 Redis SETNX 的跨实例互斥（多实例首选）。
type redisSubmitLock struct{}

func submitLockRedisKey(fp string) string { return "task_submit_lock:" + fp }

func (r *redisSubmitLock) Acquire(ctx context.Context, key, ownerToken string) (bool, error) {
	if common.RDB == nil {
		return false, fmt.Errorf("redis client is nil")
	}
	return common.RDB.SetNX(ctx, submitLockRedisKey(key), ownerToken, submitLockOrphanTTL).Result()
}

func (r *redisSubmitLock) Release(ctx context.Context, key, ownerToken string) error {
	if common.RDB == nil {
		return fmt.Errorf("redis client is nil")
	}
	// Lua 原子校验 owner token：只有持有者能进入宽限期（PEXPIRE），
	// 防止旧请求的迟到 Release 误删/误延长新持有者的锁。
	const script = `
if redis.call('get', KEYS[1]) == ARGV[1] then
	return redis.call('pexpire', KEYS[1], ARGV[2])
end
return 0`
	return common.RDB.Eval(ctx, script, []string{submitLockRedisKey(key)}, ownerToken, submitLockGraceTTL.Milliseconds()).Err()
}

// dbSubmitLock 基于数据库唯一约束（主键）的跨实例互斥。
type dbSubmitLock struct{}

func (d *dbSubmitLock) Acquire(_ context.Context, key, ownerToken string) (bool, error) {
	return model.AcquireTaskSubmitLockRow(key, ownerToken, submitLockOrphanTTL.Milliseconds())
}

func (d *dbSubmitLock) Release(_ context.Context, key, ownerToken string) error {
	return model.ReleaseTaskSubmitLockRowGrace(key, ownerToken, submitLockGraceTTL.Milliseconds())
}

var (
	submitLockBackendOnce sync.Once
	submitLockBackendImpl submitLockBackend
)

// getSubmitLockBackend 选择共享锁后端：Redis（启用时）→ 数据库 → 进程内存（降级）。
// 显式注入（setSubmitLockBackendForTest）优先；仅首次调用时自动初始化。
func getSubmitLockBackend() submitLockBackend {
	if submitLockBackendImpl != nil {
		return submitLockBackendImpl
	}
	submitLockBackendOnce.Do(func() {
		if common.RedisEnabled && common.RDB != nil {
			submitLockBackendImpl = &redisSubmitLock{}
			return
		}
		if model.DB != nil {
			submitLockBackendImpl = &dbSubmitLock{}
			return
		}
		submitLockBackendImpl = newMemorySubmitLock()
	})
	return submitLockBackendImpl
}

// setSubmitLockBackendForTest 测试注入专用（传 nil 恢复自动选择）。
func setSubmitLockBackendForTest(b submitLockBackend) {
	submitLockBackendImpl = b
}

// SubmitFingerprint 计算"相同提交"的稳定指纹：用户 + token + 请求体 SHA-256 摘要。
// 用户维度内置在指纹中，不同用户即使请求体相同也不会被合并。
// 仅用于进程内/共享后端去重与恢复记录，不随请求发送，也不记录请求内容。
func SubmitFingerprint(c *gin.Context) string {
	if c == nil || c.Request == nil {
		return ""
	}
	userId := common.GetContextKeyInt(c, constant.ContextKeyUserId)
	tokenId := common.GetContextKeyInt(c, constant.ContextKeyTokenId)

	body, err := common.GetBodyStorage(c)
	if err != nil {
		return ""
	}
	if _, err := body.Seek(0, io.SeekStart); err != nil {
		return ""
	}
	h := sha256.New()
	if _, err := io.Copy(h, body); err != nil {
		return ""
	}
	// 恢复游标，后续读取（UnmarshalBodyReusable 等）不受影响
	_, _ = body.Seek(0, io.SeekStart)

	return fmt.Sprintf("%d:%d:%s", userId, tokenId, hex.EncodeToString(h.Sum(nil)))
}

// TryAcquireTaskSubmitLock 尝试登记一个"在途提交"。
// 成功时返回请求指纹、release 函数、nil 错误；相同提交仍在途（或刚完成仍处于
// 宽限期）时返回 (fp, nil, false, nil)，调用方应拒绝该重复提交（409）。
//
// 每次获取都会生成唯一的 owner token 并随 release 校验：只有锁的持有者才能
// 释放/进入宽限期，迟到的旧 Release 无法误删新持有者的锁。
//
// 锁后端选择（见 getSubmitLockBackend）：Redis（启用时）→ 数据库 → 进程内存。
// 重要：多实例部署必须使用共享后端；若选中的共享后端（Redis/数据库）在获取时
// 报错，本函数 fail-closed 返回错误（调用方应回 503），绝不静默降级为进程内锁
// —— 否则跨实例的重复提交将无法拦截（锁失效 → 重复创建）。
func TryAcquireTaskSubmitLock(c *gin.Context) (string, func(), bool, error) {
	fp := SubmitFingerprint(c)
	if fp == "" {
		// 无法计算指纹（如无请求体）时不加锁，直接放行。
		return "", func() {}, true, nil
	}

	ownerToken := uuid.NewString()
	backend := getSubmitLockBackend()
	acquired, err := backend.Acquire(context.Background(), fp, ownerToken)
	if err != nil {
		// fail-closed：共享锁不可用时不放行（宁可拒绝，不可无锁放行导致重复创建）
		common.SysLog("task submit lock backend acquire failed: " + err.Error())
		return fp, nil, false, fmt.Errorf("task submit lock unavailable: %w", err)
	}
	if !acquired {
		return fp, nil, false, nil
	}

	released := false
	return fp, func() {
		if released {
			return
		}
		released = true
		if err := backend.Release(context.Background(), fp, ownerToken); err != nil {
			common.SysLog("task submit lock release failed: " + err.Error())
		}
	}, true, nil
}
