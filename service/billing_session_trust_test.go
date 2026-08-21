package service

import (
	"testing"

	"github.com/QuantumNous/new-api/common"
	relaycommon "github.com/QuantumNous/new-api/relay/common"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// shouldTrust 回归测试。
//
// 背景：middleware/auth.go 的 SetupContextForToken 以 int64 写入
// "token_quota"（token.RemainQuota）。shouldTrust 曾用 c.GetInt 读取，
// Gin 对 int64 做 int 断言失败会静默返回 0，导致所有有限额度 Key
// 永远无法命中信任额度旁路（用户钱包充足也会走预扣路径）。
// 本文件锁定正确的 int64 读取行为。

// newTrustTestSession 构造一个最小可测的 BillingSession。
func newTrustTestSession(tokenUnlimited bool, userQuota int64) (*BillingSession, *relaycommon.RelayInfo) {
	relayInfo := &relaycommon.RelayInfo{
		UserId:         1,
		TokenUnlimited: tokenUnlimited,
		UserQuota:      userQuota,
	}
	s := &BillingSession{
		relayInfo: relayInfo,
		funding:   &WalletFunding{userId: 1},
	}
	return s, relayInfo
}

// pinQuotaPerUnit 固定 QuotaPerUnit，使 GetTrustQuota() = 10 * 500_000 = 5_000_000。
func pinQuotaPerUnit(t *testing.T) {
	t.Helper()
	original := common.QuotaPerUnit
	common.QuotaPerUnit = 500_000
	t.Cleanup(func() { common.QuotaPerUnit = original })
}

// 回归核心：有限额度 Key 的 token_quota 以 int64 写入时必须能命中信任旁路。
// 旧实现 c.GetInt 读 int64 会得到 0，本用例将失败。
func TestShouldTrustLimitedTokenWithInt64ContextQuota(t *testing.T) {
	pinQuotaPerUnit(t) // trustQuota = 5_000_000
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(nil)

	// 与 middleware/auth.go SetupContextForToken 完全一致的写入方式
	c.Set("token_quota", int64(6_000_000))

	s, _ := newTrustTestSession(false, 6_000_000)
	require.True(t, s.shouldTrust(c),
		"有限额度 Key（token_quota 以 int64 写入且 > 信任额度）应命中信任旁路；"+
			"若失败说明 token_quota 又被当作 int 读取（回归）")
}

// 有限额度 Key：token 额度低于信任额度，即使钱包充足也不信任。
func TestShouldTrustLimitedTokenBelowThreshold(t *testing.T) {
	pinQuotaPerUnit(t)
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(nil)
	c.Set("token_quota", int64(4_999_999))

	s, _ := newTrustTestSession(false, 6_000_000)
	require.False(t, s.shouldTrust(c), "token 额度低于信任额度时不应信任")
}

// 有限额度 Key：token 额度大（超过 int32 上限的大额度），必须正确读取。
func TestShouldTrustLimitedTokenLargeInt64Quota(t *testing.T) {
	pinQuotaPerUnit(t)
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(nil)

	// 111111 USD ≈ 55_555_500_000，远超 int32；c.GetInt 在任何平台上都无法承载
	c.Set("token_quota", int64(111111*500_000))

	s, _ := newTrustTestSession(false, int64(111111*500_000))
	require.True(t, s.shouldTrust(c), "大额度（111111 USD）有限 Key 应命中信任旁路")
}

// 无限额度 Key：跳过 token 检查，仅看钱包。
func TestShouldTrustUnlimitedTokenSufficientWallet(t *testing.T) {
	pinQuotaPerUnit(t)
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(nil)

	s, _ := newTrustTestSession(true, 6_000_000)
	require.True(t, s.shouldTrust(c), "无限 Key + 钱包充足应信任")
}

func TestShouldTrustUnlimitedTokenInsufficientWallet(t *testing.T) {
	pinQuotaPerUnit(t)
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(nil)

	s, _ := newTrustTestSession(true, 5_000_000)
	require.False(t, s.shouldTrust(c), "钱包额度未严格大于信任额度时不应信任")
}

// 上下文缺失 token_quota（异常路径）不应误信任。
func TestShouldTrustMissingContextQuota(t *testing.T) {
	pinQuotaPerUnit(t)
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(nil)

	s, _ := newTrustTestSession(false, 6_000_000)
	require.False(t, s.shouldTrust(c), "上下文缺失 token_quota 时不应信任")
}

// 异步任务（ForcePreConsume）必须预扣全额，禁止信任旁路。
func TestShouldTrustForcePreConsumeDisabled(t *testing.T) {
	pinQuotaPerUnit(t)
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(nil)
	c.Set("token_quota", int64(6_000_000))

	s, relayInfo := newTrustTestSession(false, 6_000_000)
	relayInfo.ForcePreConsume = true
	require.False(t, s.shouldTrust(c), "ForcePreConsume 时禁止信任旁路")
}

// 订阅资金来源不支持信任旁路。
func TestShouldTrustSubscriptionNeverTrusted(t *testing.T) {
	pinQuotaPerUnit(t)
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(nil)
	c.Set("token_quota", int64(6_000_000))

	relayInfo := &relaycommon.RelayInfo{
		UserId:         1,
		TokenUnlimited: false,
		UserQuota:      6_000_000,
	}
	s := &BillingSession{
		relayInfo: relayInfo,
		funding:   &SubscriptionFunding{userId: 1, subscriptionId: 9},
	}
	require.False(t, s.shouldTrust(c), "订阅资金来源不应信任")
}
