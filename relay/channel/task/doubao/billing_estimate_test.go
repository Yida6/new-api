package doubao

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"
	relaycommon "github.com/QuantumNous/new-api/relay/common"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ===========================================================================
// 真实结算倍率与保守预扣彻底分离（EstimateSeedancePricing / PreConsumeMultiplier）
// 契约说明见 constants.go 文件头注释；测试同时充当公式依据文档。
//
// 核心不变量：
//   - actualPricingMultiplier = 请求实际组合单价 / 基准组合单价（允许 < 1）；
//   - 预扣 = baseQuota × actualPricingMultiplier × preConsumeMultiplier
//         = baseQuota × durationSafety × conservativePriceRatio ≥ 实际费用；
//   - fallback（缺失/未知档位）只进预扣，绝不进入真实结算。
// ===========================================================================

// 标准 2.0 的 6 个 (分辨率档, hasVideo) 组合：真实结算倍率 = 组合单价/46
// （允许 < 1，如 28/46、31/46、26/46、16/46），预扣按表内最大单价比档
// （51/46）保守预留，绝不低估。
func TestEstimateSeedancePricing_Standard2Combos(t *testing.T) {
	const model = "doubao-seedance-2-0-260128"
	const base = 46.0 // 基准组合单价
	cases := []struct {
		name       string
		resolution string
		hasVideo   bool
		price      float64 // 组合单价（元/百万 token）
	}{
		{"480p-t2v", "480p", false, 46.0},
		{"480p-i2v", "480p", true, 28.0},
		{"1080p-t2v", "1080p", false, 51.0},
		{"1080p-i2v", "1080p", true, 31.0},
		{"4k-t2v", "4k", false, 26.0},
		{"4k-i2v", "4k", true, 16.0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			est := EstimateSeedancePricing(SeedanceBillingParams{
				Model:      model,
				Resolution: tc.resolution,
				Duration:   5,
				HasVideo:   tc.hasVideo,
			})
			require.True(t, est.PricedModel)
			assert.False(t, est.ResolutionFellBack)

			want := tc.price / base
			assert.InDelta(t, want, est.PricingMultiplier, 1e-9,
				"真实结算倍率 = 实际组合单价/基准单价（允许 < 1）")
			if want != 1.0 {
				require.Contains(t, est.PricingRatios, "size", "倍率 ≠1 时必须写入结算 OtherRatio")
				assert.InDelta(t, want, est.PricingRatios["size"], 1e-9)
			} else {
				assert.NotContains(t, est.PricingRatios, "size", "倍率 = 1.0 的组合不写 OtherRatio")
			}

			// 预扣 = baseQuota × actual × preConsumeMultiplier = baseQuota × 51/46
			baseQuota := 100000
			preConsume, clamp := quotaFromSeedanceEstimate(baseQuota, est)
			require.Nil(t, clamp, "常规组合不得触发额度饱和")
			assert.InDelta(t, float64(baseQuota)*51.0/base, float64(preConsume), 1.0,
				"预扣 = baseQuota × durationSafety × 表内最大单价比档（51/46）")
			assert.GreaterOrEqual(t, float64(preConsume)+1.0, float64(baseQuota)*want,
				"预扣绝不低估真实费用（保守预扣 ≥ baseQuota × actual；QuotaFromFloat 截断 <1 单位）")
			// 10 秒：durationSafety = 2.0，预扣放大到 base × 2 × 51/46
			est10 := EstimateSeedancePricing(SeedanceBillingParams{
				Model:      model,
				Resolution: tc.resolution,
				Duration:   10,
				HasVideo:   tc.hasVideo,
			})
			preConsume10, _ := quotaFromSeedanceEstimate(baseQuota, est10)
			assert.InDelta(t, float64(baseQuota)*2.0*51.0/base, float64(preConsume10), 1.0,
				"10 秒预扣 = baseQuota × 2 × 51/46")
			assert.InDelta(t, want, est10.PricingMultiplier, 1e-9, "真实结算倍率与时长无关（token 已含时长）")
		})
	}
}

// fast 的全部支持组合（基准档 28/16.6）：
//   - (480p, t2v)：真实倍率 1.0，预扣缓冲 1.0；
//   - (480p, i2v)：真实倍率 16.6/28（<1），预扣缓冲 = maxRatio(1.0) / (16.6/28)。
func TestEstimateSeedancePricing_FastCombos(t *testing.T) {
	const model = "doubao-seedance-2-0-fast-260128"
	const base = 28.0

	est := EstimateSeedancePricing(SeedanceBillingParams{Model: model, Resolution: "480p", Duration: 5, HasVideo: false})
	require.True(t, est.PricedModel)
	assert.Equal(t, 1.0, est.PricingMultiplier, "fast 基准文生组合倍率 = 1.0")
	assert.Equal(t, 1.0, est.PreConsumeMultiplier)
	assert.NotContains(t, est.PricingRatios, "size")

	estI2V := EstimateSeedancePricing(SeedanceBillingParams{Model: model, Resolution: "480p", Duration: 5, HasVideo: true})
	require.True(t, estI2V.PricedModel)
	assert.InDelta(t, 16.6/base, estI2V.PricingMultiplier, 1e-9, "fast 视频输入真实倍率 = 16.6/28（允许 <1）")
	assert.Contains(t, estI2V.PricingRatios, "size")
	assert.InDelta(t, 16.6/base, estI2V.PricingRatios["size"], 1e-9)
	assert.InDelta(t, 1.0/(16.6/base), estI2V.PreConsumeMultiplier, 1e-9,
		"预扣缓冲 = maxRatio(1.0) / actual（预扣 = base × 1.0，永不低估）")
}

func TestEstimateSeedancePricing_NewModelCombos(t *testing.T) {
	cases := []struct {
		name       string
		model      string
		resolution string
		hasVideo   bool
		base       float64
		price      float64
	}{
		{"mini-480p-t2v", "doubao-seedance-2-0-mini-260615", "480p", false, 9.5, 9.5},
		{"mini-480p-i2v", "doubao-seedance-2-0-mini-260615", "480p", true, 9.5, 5.8},
		{"mini-720p-t2v", "doubao-seedance-2-0-mini-260615", "720p", false, 9.5, 9.5},
		{"mini-720p-i2v", "doubao-seedance-2-0-mini-260615", "720p", true, 9.5, 5.8},
		{"2.5-480p-t2v", "doubao-seedance-2-5-260628", "480p", false, 70.7, 70.7},
		{"2.5-480p-i2v", "doubao-seedance-2-5-260628", "480p", true, 70.7, 42.4},
		{"2.5-720p-t2v", "doubao-seedance-2-5-260628", "720p", false, 70.7, 70.7},
		{"2.5-720p-i2v", "doubao-seedance-2-5-260628", "720p", true, 70.7, 42.4},
		// 2.5 官方 2026-08-17 起支持原生 1080p：真实倍率 77.8/70.7、46.5/70.7。
		{"2.5-1080p-t2v", "doubao-seedance-2-5-260628", "1080p", false, 70.7, 77.8},
		{"2.5-1080p-i2v", "doubao-seedance-2-5-260628", "1080p", true, 70.7, 46.5},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			est := EstimateSeedancePricing(SeedanceBillingParams{
				Model: tc.model, Resolution: tc.resolution, Duration: 5, HasVideo: tc.hasVideo,
			})
			require.True(t, est.PricedModel)
			assert.InDelta(t, tc.price/tc.base, est.PricingMultiplier, 1e-9)
			assert.False(t, est.ResolutionFellBack)
		})
	}
}

// 空分辨率（未指定 = 上游默认，合法）：按基准档结算，预扣 = base × dur × maxRatio。
func TestEstimateSeedancePricing_EmptyResolution(t *testing.T) {
	est := EstimateSeedancePricing(SeedanceBillingParams{
		Model: "doubao-seedance-2-0-260128", Resolution: "", Duration: 5, HasVideo: false,
	})
	require.True(t, est.PricedModel)
	assert.False(t, est.ResolutionFellBack, "空分辨率 = 未指定，属于基准档而非 fallback")
	assert.Equal(t, 1.0, est.PricingMultiplier)
	assert.InDelta(t, 51.0/46.0, est.PreConsumeMultiplier, 1e-9, "5 秒基准档预扣缓冲 = 51/46")
}

// 未知分辨率：预扣按表内最大单价比档 fail closed，但该 fallback **绝不进入**
// 真实结算（真实倍率保持 1.0，PricingRatios 为空）。
func TestEstimateSeedancePricing_UnknownResolutionFallbackNeverInSettlement(t *testing.T) {
	est := EstimateSeedancePricing(SeedanceBillingParams{
		Model: "doubao-seedance-2-0-260128", Resolution: "1024x576", Duration: 5, HasVideo: false,
	})
	require.True(t, est.PricedModel)
	assert.True(t, est.ResolutionFellBack)
	assert.Equal(t, 1.0, est.PricingMultiplier, "fallback 绝不进入真实结算")
	assert.Len(t, est.PricingRatios, 0)
	assert.InDelta(t, 51.0/46.0, est.PreConsumeMultiplier, 1e-9, "预扣按表内最大单价比档 fail closed")
}

// 10 秒任务与固定价格模式：真实结算倍率不含时长；固定价格模式预扣缓冲 = 1.0。
func TestEstimateSeedancePricing_TenSecondAndFixedPrice(t *testing.T) {
	est := EstimateSeedancePricing(SeedanceBillingParams{
		Model: "doubao-seedance-2-0-260128", Resolution: "480p", Duration: 10, HasVideo: false,
	})
	assert.Equal(t, 1.0, est.PricingMultiplier, "真实结算倍率与时长无关（token 已含时长）")
	assert.InDelta(t, 2.0*51.0/46.0, est.PreConsumeMultiplier, 1e-9, "10 秒预扣缓冲 = 2 × 51/46")
	assert.NotContains(t, est.PricingRatios, "seconds", "定价倍率绝不包含时长缓冲")

	estFixed := EstimateSeedancePricing(SeedanceBillingParams{
		Model: "doubao-seedance-2-0-260128", Resolution: "480p", Duration: 10, HasVideo: false, UsePrice: true,
	})
	assert.Equal(t, 1.0, estFixed.PreConsumeMultiplier, "固定价格模式不加任何预扣缓冲（预扣 = 实际价）")
	assert.Equal(t, 1.0, estFixed.PricingMultiplier)
}

func TestEstimateSeedancePricing_UnpricedModelPreConsumeOnly(t *testing.T) {
	// 无价格表模型：保守系数只进预扣缓冲（2.0 × 时长 2.0 = 4.0），
	// **绝不**写 PricingRatios（不污染真实结算）
	old := unpricedMultiplierOverride
	setSeedanceUnpricedMultiplierForTest(2.0)
	defer setSeedanceUnpricedMultiplierForTest(old)
	est := EstimateSeedancePricing(SeedanceBillingParams{
		Model:    "doubao-seedance-1-0-pro-250528",
		Duration: 10,
		UsePrice: false,
	})
	assert.False(t, est.PricedModel)
	assert.Equal(t, 4.0, est.PreConsumeMultiplier, "无表模型预扣缓冲 = 保守系数 × 时长缓冲")
	assert.Equal(t, 1.0, est.PricingMultiplier, "无表模型真实结算倍率为 1.0（不污染结算）")
	assert.Len(t, est.PricingRatios, 0)
}

func TestEstimateSeedancePricing_QuotaSaturationNoOverflow(t *testing.T) {
	// 极端组合 → 饱和转换，绝不为负数/溢出
	est := EstimateSeedancePricing(SeedanceBillingParams{
		Model:      "doubao-seedance-1-0-pro-250528",
		Resolution: "weird-format",
		Duration:   3600,
		HasVideo:   true,
	})
	quota, clamp := quotaFromSeedanceEstimate(common.MaxQuota, est)
	require.NotNil(t, clamp, "超长时长必须触发饱和，不允许静默溢出")
	assert.GreaterOrEqual(t, quota, 0, "饱和后额度不得为负数")
}

// ===========================================================================
// 模型版本归一化（upstream 命中价格表优先；ep-* 不作为价格版本；
// upstream 无法识别才回退公开别名）
// ===========================================================================

func TestResolveSeedancePriceModel_AliasHitsPriceTable(t *testing.T) {
	// 公开别名 doubao-seedance-2.0（OriginModelName）→ 命中 2-0-260128 价格表
	got := ResolveSeedancePriceModel("doubao-seedance-2.0", "ep-20260812xxxx")
	assert.Equal(t, "doubao-seedance-2-0-260128", got, "公开别名必须命中规范版本（Endpoint ID 不干扰）")
	assert.True(t, HasSeedancePriceTable(got))

	// fast 别名
	got = ResolveSeedancePriceModel("doubao-seedance-2-0-fast", "ep-20260812yyyy")
	assert.Equal(t, "doubao-seedance-2-0-fast-260128", got)

	got = ResolveSeedancePriceModel("doubao-seedance-2.0-mini", "ep-20260812mini")
	assert.Equal(t, "doubao-seedance-2-0-mini-260615", got)
	got = ResolveSeedancePriceModel("doubao-seedance-2.5", "ep-20260812v25")
	assert.Equal(t, "doubao-seedance-2-5-260628", got)
}

func TestResolveSeedancePriceModel_UpstreamVersionHitsPriceTable(t *testing.T) {
	// 无别名但上游映射为规范版本 → 命中
	got := ResolveSeedancePriceModel("some-public-name", "doubao-seedance-2-0-260128")
	assert.Equal(t, "doubao-seedance-2-0-260128", got)
}

// 公开 doubao-seedance-2.0 被渠道映射为 fast 规范版本 → **upstream 优先**，
// 必须解析为 fast（而非别名 2-0-260128），从而 fast 不支持的 1080p 被拒绝。
func TestResolveSeedancePriceModel_PublicAliasMappedToFastWins(t *testing.T) {
	got := ResolveSeedancePriceModel("doubao-seedance-2.0", "doubao-seedance-2-0-fast-260128")
	assert.Equal(t, "doubao-seedance-2-0-fast-260128", got,
		"明确且命中价格表的 upstream 映射必须优先于公开别名")
	// fast 无 1080p/4k 档：无论是否视频输入都必须拒绝（完整组合校验）
	assert.NotEmpty(t, ValidateSeedanceResolutionForModel(got, "1080p", false), "fast 1080p 文生必须拒绝")
	assert.NotEmpty(t, ValidateSeedanceResolutionForModel(got, "1080p", true), "fast 1080p 视频输入必须拒绝")
	assert.NotEmpty(t, ValidateSeedanceResolutionForModel(got, "4k", false), "fast 4k 必须拒绝")
	assert.Empty(t, ValidateSeedanceResolutionForModel(got, "480p", false), "fast 基准档放行")
	assert.Empty(t, ValidateSeedanceResolutionForModel(got, "480p", true), "fast 基准档+视频输入放行")
	// 估算与校验同源：解析结果为 fast → 命中 fast 价格表
	est := EstimateSeedancePricing(SeedanceBillingParams{Model: got, Resolution: "480p", Duration: 5, HasVideo: false})
	assert.True(t, est.PricedModel)
}

func TestResolveSeedancePriceModel_EndpointIDNeverTrusted(t *testing.T) {
	// Endpoint ID（ep-xxx）绝不当作可信模型版本
	got := ResolveSeedancePriceModel("doubao-seedance-2.0", "ep-20260812zzzz")
	assert.Equal(t, "doubao-seedance-2-0-260128", got, "别名优先，但若 origin 也无别名且 upstream 是 ep- 前缀 → 空")
	got2 := ResolveSeedancePriceModel("unknown-model", "ep-20260812zzzz")
	assert.Equal(t, "", got2, "Endpoint ID 不得直接命中价格表（未知模型 fail closed）")
}

func TestResolveSeedancePriceModel_UnknownModelUnpriced(t *testing.T) {
	assert.Equal(t, "", ResolveSeedancePriceModel("doubao-seedance-1-0-pro-250528", ""))
	assert.False(t, HasSeedancePriceTable(""))
}

// ===========================================================================
// 时长解析（三态）与支持矩阵
// ===========================================================================

func TestResolveSeedanceDurationEx_TriState(t *testing.T) {
	// 合法：Seconds 字符串 / Duration 数字 / metadata 数字与字符串
	d, o := ResolveSeedanceDurationEx(&relaycommon.TaskSubmitReq{Seconds: "10"})
	assert.Equal(t, 10, d)
	assert.Equal(t, DurationParseOK, o)

	d, o = ResolveSeedanceDurationEx(&relaycommon.TaskSubmitReq{Duration: 5})
	assert.Equal(t, 5, d)
	assert.Equal(t, DurationParseOK, o)

	d, o = ResolveSeedanceDurationEx(&relaycommon.TaskSubmitReq{Metadata: map[string]interface{}{"duration": 5.0}})
	assert.Equal(t, 5, d)
	assert.Equal(t, DurationParseOK, o)

	d, o = ResolveSeedanceDurationEx(&relaycommon.TaskSubmitReq{Metadata: map[string]interface{}{"SECONDS": "10"}})
	assert.Equal(t, 10, d)
	assert.Equal(t, DurationParseOK, o, "metadata key 大小写必须真正归一化")

	// 缺失（未传）→ 合法（上游默认）
	d, o = ResolveSeedanceDurationEx(&relaycommon.TaskSubmitReq{})
	assert.Equal(t, 0, d)
	assert.Equal(t, DurationParseMissing, o)

	// 无法解析：非数字 / 浮点小数 / 非正数 → Unparsable（发送上游前 400）
	_, o = ResolveSeedanceDurationEx(&relaycommon.TaskSubmitReq{Seconds: "abc"})
	assert.Equal(t, DurationParseUnparsable, o)
	_, o = ResolveSeedanceDurationEx(&relaycommon.TaskSubmitReq{Metadata: map[string]interface{}{"duration": 2.5}})
	assert.Equal(t, DurationParseUnparsable, o, "浮点小数不得 int 截断伪装成合法时长")
	_, o = ResolveSeedanceDurationEx(&relaycommon.TaskSubmitReq{Metadata: map[string]interface{}{"seconds": "abc"}})
	assert.Equal(t, DurationParseUnparsable, o, "metadata 声明了时长字段但值非法 → 明确拒绝")
	_, o = ResolveSeedanceDurationEx(&relaycommon.TaskSubmitReq{Duration: -1})
	assert.Equal(t, DurationParseUnparsable, o)
}

func TestSeedanceSupportedDurations_BySeries(t *testing.T) {
	// 2.0 系列（公开别名与规范版本均可识别）：4–15s
	for _, m := range []string{"doubao-seedance-2.0", "doubao-seedance-2-0-260128",
		"doubao-seedance-2-0-fast-260128", "doubao-seedance-2-0-mini-260615"} {
		allowed := SeedanceSupportedDurationsForModel(m)
		for _, d := range []int{4, 5, 10, 15} {
			assert.True(t, allowed[d], "%s 应允许 %d 秒", m, d)
		}
		for _, d := range []int{1, 2, 3, 16, 20, 30} {
			assert.False(t, allowed[d], "%s 不应允许 %d 秒", m, d)
		}
	}
	// 2.5：4–30s（含用户目标 20s）
	allowed25 := SeedanceSupportedDurationsForModel("doubao-seedance-2.5")
	for _, d := range []int{4, 5, 10, 15, 20, 30} {
		assert.True(t, allowed25[d], "2.5 应允许 %d 秒", d)
	}
	for _, d := range []int{1, 2, 3, 31, 60} {
		assert.False(t, allowed25[d], "2.5 不应允许 %d 秒", d)
	}
	// 未知模型 fail closed 到保守 {5,10}
	allowedUnknown := SeedanceSupportedDurationsForModel("doubao-seedance-1-5-pro-251215")
	assert.True(t, allowedUnknown[5])
	assert.True(t, allowedUnknown[10])
	assert.False(t, allowedUnknown[15], "未知模型不得放行 15 秒")
	assert.False(t, allowedUnknown[20], "未知模型不得放行 20 秒")
}

// ===========================================================================
// 参数校验（与支持矩阵同一契约；未知/缺失/非法一律发送上游前 400）
// ===========================================================================

func TestValidateRequestAndSetAction_DurationMatrix(t *testing.T) {
	gin.SetMode(gin.TestMode)
	a := &TaskAdaptor{}

	// 2.0 系列：允许 4–15s，其余拒绝
	info2 := &relaycommon.RelayInfo{TaskRelayInfo: &relaycommon.TaskRelayInfo{}, OriginModelName: "doubao-seedance-2.0"}
	for _, sec := range []string{"1", "2", "3", "16", "17", "30"} {
		body := `{"model":"doubao-seedance-2.0","prompt":"hello","seconds":"` + sec + `"}`
		c := &gin.Context{Keys: make(map[string]any)}
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/video/generations", strings.NewReader(body))
		c.Request.Header.Set("Content-Type", "application/json")
		taskErr := a.ValidateRequestAndSetAction(c, info2)
		require.NotNil(t, taskErr, "%s 秒必须被拒绝（2.0）", sec)
		assert.Equal(t, "invalid_seedance_duration", taskErr.Code)
	}
	for _, sec := range []string{"4", "5", "10", "15"} {
		body := `{"model":"doubao-seedance-2.0","prompt":"hello","seconds":"` + sec + `"}`
		c := &gin.Context{Keys: make(map[string]any)}
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/video/generations", strings.NewReader(body))
		c.Request.Header.Set("Content-Type", "application/json")
		taskErr := a.ValidateRequestAndSetAction(c, info2)
		require.Nil(t, taskErr, "%s 秒必须被允许（2.0）", sec)
	}

	// 2.5 系列：允许 4–30s（含用户目标 20s），其余拒绝
	info25 := &relaycommon.RelayInfo{TaskRelayInfo: &relaycommon.TaskRelayInfo{}, OriginModelName: "doubao-seedance-2.5"}
	for _, sec := range []string{"1", "3", "31", "60"} {
		body := `{"model":"doubao-seedance-2.5","prompt":"hello","seconds":"` + sec + `"}`
		c := &gin.Context{Keys: make(map[string]any)}
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/video/generations", strings.NewReader(body))
		c.Request.Header.Set("Content-Type", "application/json")
		taskErr := a.ValidateRequestAndSetAction(c, info25)
		require.NotNil(t, taskErr, "%s 秒必须被拒绝（2.5）", sec)
		assert.Equal(t, "invalid_seedance_duration", taskErr.Code)
	}
	for _, sec := range []string{"4", "10", "20", "30"} {
		body := `{"model":"doubao-seedance-2.5","prompt":"hello","seconds":"` + sec + `"}`
		c := &gin.Context{Keys: make(map[string]any)}
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/video/generations", strings.NewReader(body))
		c.Request.Header.Set("Content-Type", "application/json")
		taskErr := a.ValidateRequestAndSetAction(c, info25)
		require.Nil(t, taskErr, "%s 秒必须被允许（2.5）", sec)
	}
}

func TestValidateRequestAndSetAction_UnparsableDurationRejected(t *testing.T) {
	gin.SetMode(gin.TestMode)
	a := &TaskAdaptor{}
	for _, body := range []string{
		`{"model":"doubao-seedance-2-0-260128","prompt":"hello","seconds":"abc"}`,
		`{"model":"doubao-seedance-2-0-260128","prompt":"hello","metadata":{"duration":2.5}}`,
		`{"model":"doubao-seedance-2-0-260128","prompt":"hello","metadata":{"Duration":"xx"}}`,
	} {
		c := &gin.Context{Keys: make(map[string]any)}
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/video/generations", strings.NewReader(body))
		c.Request.Header.Set("Content-Type", "application/json")
		taskErr := a.ValidateRequestAndSetAction(c, &relaycommon.RelayInfo{TaskRelayInfo: &relaycommon.TaskRelayInfo{}})
		require.NotNil(t, taskErr, "无法解析时长必须 400: %s", body)
		assert.Equal(t, "invalid_seedance_duration", taskErr.Code)
	}
}

func TestValidateRequestAndSetAction_ResolutionMatrix(t *testing.T) {
	gin.SetMode(gin.TestMode)
	a := &TaskAdaptor{}
	info := &relaycommon.RelayInfo{TaskRelayInfo: &relaycommon.TaskRelayInfo{}, OriginModelName: "doubao-seedance-2.0"}

	// 未知分辨率 → 400（不低价预扣后提交）
	body := `{"model":"doubao-seedance-2.0","prompt":"hello","metadata":{"resolution":"1024x576"}}`
	c := &gin.Context{Keys: make(map[string]any)}
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/video/generations", strings.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	taskErr := a.ValidateRequestAndSetAction(c, info)
	require.NotNil(t, taskErr)
	assert.Equal(t, "invalid_seedance_resolution", taskErr.Code)

	// 合法分辨率放行（2-0 价格表含全部 (分辨率档, hasVideo) 组合）
	for _, res := range []string{"480p", "720p", "1080p", "4k", "1080P"} {
		body := `{"model":"doubao-seedance-2.0","prompt":"hello","metadata":{"resolution":"` + res + `"}}`
		c := &gin.Context{Keys: make(map[string]any)}
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/video/generations", strings.NewReader(body))
		c.Request.Header.Set("Content-Type", "application/json")
		taskErr := a.ValidateRequestAndSetAction(c, info)
		require.Nil(t, taskErr, "分辨率 %s 必须放行", res)
	}
}

// 完整 (分辨率档, hasVideo) 组合校验：有表模型缺失组合必须拒绝（不能固定
// 检查 hasVideo=false——否则"仅视频输入支持"的组合会被误判）。
func TestValidateSeedanceResolutionForModel_FullCombinationCheck(t *testing.T) {
	// fast 无 1080p/4k 档：无论是否视频输入都拒绝
	require.NotEmpty(t, ValidateSeedanceResolutionForModel("doubao-seedance-2-0-fast-260128", "1080p", false))
	require.NotEmpty(t, ValidateSeedanceResolutionForModel("doubao-seedance-2-0-fast-260128", "1080p", true))
	require.NotEmpty(t, ValidateSeedanceResolutionForModel("doubao-seedance-2-0-fast-260128", "4k", false))
	require.NotEmpty(t, ValidateSeedanceResolutionForModel("doubao-seedance-2-0-fast-260128", "4k", true))
	// fast 基准档两个组合都放行
	assert.Empty(t, ValidateSeedanceResolutionForModel("doubao-seedance-2-0-fast-260128", "480p", false))
	assert.Empty(t, ValidateSeedanceResolutionForModel("doubao-seedance-2-0-fast-260128", "480p", true))
	assert.Empty(t, ValidateSeedanceResolutionForModel("doubao-seedance-2-0-fast-260128", "720p", false))
	assert.Empty(t, ValidateSeedanceResolutionForModel("doubao-seedance-2-0-fast-260128", "720p", true))

	// mini 仅支持 480p/720p，且两档都支持视频输入。
	for _, hasVideo := range []bool{false, true} {
		for _, res := range []string{"480p", "720p"} {
			assert.Empty(t, ValidateSeedanceResolutionForModel("doubao-seedance-2-0-mini-260615", res, hasVideo))
		}
	}
	require.NotEmpty(t, ValidateSeedanceResolutionForModel("doubao-seedance-2-0-mini-260615", "1080p", false))

	// 2.5 支持 480p/720p/1080p（官方 2026-08-17 起原生 1080p），无 4k：
	// 480p/720p/1080p 两种输入组合放行，4k 拒绝。
	for _, hasVideo := range []bool{false, true} {
		for _, res := range []string{"480p", "720p", "1080p"} {
			assert.Empty(t, ValidateSeedanceResolutionForModel("doubao-seedance-2-5-260628", res, hasVideo))
		}
	}
	require.NotEmpty(t, ValidateSeedanceResolutionForModel("doubao-seedance-2-5-260628", "4k", false))

	// 2-0 含全部 8 个组合：全部放行
	for _, hasVideo := range []bool{false, true} {
		for _, res := range []string{"480p", "720p", "1080p", "4k"} {
			assert.Empty(t, ValidateSeedanceResolutionForModel("doubao-seedance-2-0-260128", res, hasVideo),
				"2-0 %s hasVideo=%t 必须放行", res, hasVideo)
		}
	}
	// 无表模型不校验（fail closed）
	assert.Empty(t, ValidateSeedanceResolutionForModel("", "1080p", false))
	// 未知分辨率一律拒绝
	require.NotEmpty(t, ValidateSeedanceResolutionForModel("doubao-seedance-2-0-260128", "super-hd", false))
}

// 问题五：临时缺失基准档视频输入组合时，基准档/空分辨率带视频输入必须返回
// 400——tier=="" 不能绕过 (分辨率档, hasVideo) 的精确键检查。
func TestValidateSeedanceResolutionForModel_MissingBaseVideoComboRejected(t *testing.T) {
	// 备份并临时删除 2-0 价格表的基准档视频输入组合（模拟配置缺失/未收录）
	baseKey := videoPriceKey{hasVideo: true}
	orig, had := videoPriceTable["doubao-seedance-2-0-260128"][baseKey]
	if had {
		delete(videoPriceTable["doubao-seedance-2-0-260128"], baseKey)
	}
	t.Cleanup(func() {
		if had {
			videoPriceTable["doubao-seedance-2-0-260128"][baseKey] = orig
		}
	})

	// 基准档（480p/720p）带视频输入：组合缺失 → 必须 400
	require.NotEmpty(t, ValidateSeedanceResolutionForModel("doubao-seedance-2-0-260128", "480p", true),
		"基准档 hasVideo=true 组合缺失必须拒绝")
	// 空分辨率（未指定）同样检查 hasVideo 组合：视频输入 → 400（修复点）
	require.NotEmpty(t, ValidateSeedanceResolutionForModel("doubao-seedance-2-0-260128", "", true),
		"空分辨率不能绕过 hasVideo 组合检查")
	// 空分辨率 + 无视频输入：零值键 {hasVideo:false} 由 HasSeedancePriceTable 保证 → 放行
	assert.Empty(t, ValidateSeedanceResolutionForModel("doubao-seedance-2-0-260128", "", false))
	// 其余未缺失的组合不受影响
	assert.Empty(t, ValidateSeedanceResolutionForModel("doubao-seedance-2-0-260128", "1080p", false))
	assert.Empty(t, ValidateSeedanceResolutionForModel("doubao-seedance-2-0-260128", "4k", true))
}

// ===========================================================================
// 模型映射后的校验与估算同一价格表（缺陷二：映射时机修复回归）
// 渠道映射把公开名映射为规范版本/无表模型时，校验必须与估算使用同一
// ResolveSeedancePriceModel 结果，绝不能把映射模型当无表模型放行。
// ===========================================================================

func TestValidateRequestAndSetAction_MappedModelUsesSamePriceTable(t *testing.T) {
	gin.SetMode(gin.TestMode)
	a := &TaskAdaptor{}

	// 公开名无别名，但渠道映射为 fast（无 1080p 档）→ 1080p 必须 400，
	// 不能因"校验阶段未识别模型"而按无表模型放行（否则会低价预扣后提交上游报错）
	info := &relaycommon.RelayInfo{
		TaskRelayInfo:   &relaycommon.TaskRelayInfo{},
		OriginModelName: "my-seedance-fast",
		ChannelMeta: &relaycommon.ChannelMeta{
			UpstreamModelName: "doubao-seedance-2-0-fast-260128",
		},
	}
	body := `{"model":"my-seedance-fast","prompt":"hello","metadata":{"resolution":"1080p"}}`
	c := &gin.Context{Keys: make(map[string]any)}
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/video/generations", strings.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	taskErr := a.ValidateRequestAndSetAction(c, info)
	require.NotNil(t, taskErr, "映射为 fast 的模型 1080p 必须被拒绝")
	assert.Equal(t, "invalid_seedance_resolution", taskErr.Code)

	// 同一公开名映射为 2-0-260128（有 1080p 档）→ 1080p 放行
	info2 := &relaycommon.RelayInfo{
		TaskRelayInfo:   &relaycommon.TaskRelayInfo{},
		OriginModelName: "my-seedance-pro",
		ChannelMeta: &relaycommon.ChannelMeta{
			UpstreamModelName: "doubao-seedance-2-0-260128",
		},
	}
	body2 := `{"model":"my-seedance-pro","prompt":"hello","metadata":{"resolution":"1080p"}}`
	c2 := &gin.Context{Keys: make(map[string]any)}
	c2.Request = httptest.NewRequest(http.MethodPost, "/v1/video/generations", strings.NewReader(body2))
	c2.Request.Header.Set("Content-Type", "application/json")
	taskErr2 := a.ValidateRequestAndSetAction(c2, info2)
	require.Nil(t, taskErr2, "映射为 2-0 的模型 1080p 必须放行")

	// 估算与校验同源：同一 info 下 EstimateBilling 命中 fast 价格表（PricedModel）
	est := EstimateSeedancePricing(SeedanceBillingParams{
		Model:      ResolveSeedancePriceModel(info.OriginModelName, info.UpstreamModelName),
		Resolution: "480p",
		Duration:   5,
		HasVideo:   false,
	})
	assert.True(t, est.PricedModel, "映射后的估算必须命中价格表")
	assert.Equal(t, "doubao-seedance-2-0-fast-260128", ResolveSeedancePriceModel(info.OriginModelName, info.UpstreamModelName))
}
