package doubao

import (
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
)

var ModelList = []string{
	"doubao-seedance-1-0-pro-250528",
	"doubao-seedance-1-0-lite-t2v",
	"doubao-seedance-1-0-lite-i2v",
	"doubao-seedance-1-5-pro-251215",
	"doubao-seedance-2-0-260128",
	"doubao-seedance-2-0-fast-260128",
	"doubao-seedance-2-0-mini-260615",
	"doubao-seedance-2-5-260628",
}

var ChannelName = "doubao-video"

// ---------------------------------------------------------------------------
// Seedance 计费契约（代码注释即计费依据，测试引用同一常量）
//
// 上游计费模型（火山方舟 Seedance）：视频生成按"生成内容折算的 token 数 ×
// 单价"计费，token 数随 输出分辨率 与 生成时长 变化，单价见 videoPriceTable
// （元/百万 token）。本项目仓库内唯一权威实测数据点（docs/launch-checklist）：
//   5 秒 480P 文生视频 → 上游返回 50638 tokens。
//
// 两个明确分离的概念（videoPriceTable 每个键已是 (分辨率档, hasVideo) 的
// 组合单价，绝不能把 sizeRatio 与 videoInputRatio 当成两个独立价格相乘）：
//  1. 真实结算倍率（PricingMultiplier / OtherRatios["size"]）：
//     actualPricingMultiplier = 请求实际组合单价 / 基准组合单价（允许 < 1，
//     如 28/46、31/46、26/46、16/46）。轮询结算 computeTaskQuotaByTokens 只
//     乘它：结算 = actualTokens × actualPricingMultiplier。绝不乘时长缓冲，
//     也绝不为保守预扣把 <1 的真实倍率强制抬到 1。
//  2. 仅预扣缓冲（PreConsumeMultiplier）：
//     preConsumeMultiplier = durationSafety × conservativePriceRatio /
//     actualPricingMultiplier，只用于提交前保守预留，绝不进入 totalTokens
//     结算；固定价格模式（UsePrice）费用固定无超支风险，缓冲=1.0。
//     从而 预扣 = baseQuota × actualPricingMultiplier × preConsumeMultiplier
//             = baseQuota × durationSafety × conservativePriceRatio
//       ≥ baseQuota × durationSafety × actualPricingMultiplier（永不低估）。
//
// 保守性说明：
//  1. 时长：token 数与生成秒数正相关（视频按帧/秒折算 token 是行业通行口径），
//     以实测锚点 5 秒为基准，时长安全系数 = duration/5（下限 1.0）。即使线性
//     关系无法被权威文档证明，倍率模式的基础预扣（隐含 25 万 token，见下）
//     相对实测 5 秒 480P（50638 tokens）已有约 4.9 倍缓冲，时长系数只会在
//     更长任务上放大该缓冲，不会形成低估窗口。
//  2. 分辨率/视频输入：conservativePriceRatio 取 videoPriceTable 中该模型
//     表内最大单价比档——单价比 <1 的档（如 4k 26/46、视频输入 28/46）不能
//     用来缩小预扣（token 数无法证明同比例减少）。价格表缺失的组合与未收录
//     价格表的模型（1-0 系列 / 1-5-pro）——其单价与 token 上界均无法证明——
//     一律 fail closed 到"该模型价格表内最高单价比档"（有表模型）或全站可
//     配置保守系数 SeedanceUnpricedCostMultiplier（无表模型），绝不静默回退
//     到倍率 1.0。该 fallback 只作用于预扣，**绝不进入真实结算**（组合缺失
//     时真实倍率保持 1.0）。
//  3. 未知/无法解析的分辨率、时长：校验阶段（ValidateRequestAndSetAction）
//     在发送上游前直接返回 400，不允许以低价预扣后继续提交。
// ---------------------------------------------------------------------------

// SeedanceBaseDurationSeconds 预扣时长倍率的基准秒数（实测数据点的 5 秒）。
const SeedanceBaseDurationSeconds = 5

// SeedanceSupportedDurationValues 当前 Seedance 全系模型支持的生成长度集合。
// 火山方舟 Seedance 系列公开文档支持 5/10 秒；1/6/7/8/9/11 等其余时长上游
// 必然报错，校验阶段直接 400。若未来某模型支持其他时长，在
// SeedanceSupportedDurationsForModel 中按模型显式扩展。
var SeedanceSupportedDurationValues = []int{5, 10}

// SeedanceSupportedDurationsForModel 返回指定模型的允许时长集合（当前全系
// 一致；扩展点：未来可按模型差异化）。未知模型默认 {5,10}（fail closed 到
// 文档支持集合，绝不放行文档外的时长）。
func SeedanceSupportedDurationsForModel(_ string) map[int]bool {
	m := make(map[int]bool, len(SeedanceSupportedDurationValues))
	for _, d := range SeedanceSupportedDurationValues {
		m[d] = true
	}
	return m
}

// SeedanceMaxSupportedDurationSeconds 本地允许的 Seedance 生成时长上限。
// 火山方舟 Seedance 系列公开文档支持 5/10 秒；大于该值的时长上游必然报错，
// 本地先行拒绝。生产可按需调整（SEEDANCE_MAX_SUPPORTED_DURATION_SECONDS，
// common/init.go 初始化）。注意：这是"上限"（兼容旧配置语义），实际校验
// 用 SeedanceSupportedDurationsForModel 的允许集合（5/10）。
// 注意：通用异步任务校验（relay/common.MaxTaskDurationSeconds=3600）允许更宽
// 范围，此处是 Seedance 家族收窄后的上限。
func SeedanceMaxSupportedDurationSeconds() int {
	if maxSupportedOverride > 0 {
		return maxSupportedOverride
	}
	if v := constant.SeedanceMaxSupportedDurationSeconds; v > 0 {
		return v
	}
	return 10
}

var maxSupportedOverride int

// setSeedanceMaxSupportedDurationForTest 测试注入专用（传 0 恢复配置值）。
func setSeedanceMaxSupportedDurationForTest(v int) {
	maxSupportedOverride = v
}

// SeedanceUnpricedCostMultiplier 无价格表模型的保守预扣系数。
// 默认 2.0，由 common/init.go 从 SEEDANCE_UNPRICED_COST_MULTIPLIER 初始化，
// 测试可通过 setSeedanceUnpricedMultiplierForTest 覆盖。
// 依据：仓库内唯一实测数据点（5 秒 480P → 50638 tokens）相对倍率模式基础预扣
// 隐含 token 数（25 万）已有约 4.9 倍缓冲；无表模型单价与 token 上界无法证明，
// 取 2.0 作为该缓冲之外再叠加的最坏成本放大，仍远低于 4.9 倍缓冲，不会因叠加
// 而产生收费超上限风险；生产可按账单对账结果调整。
func SeedanceUnpricedCostMultiplier() float64 {
	if unpricedMultiplierOverride > 0 {
		return unpricedMultiplierOverride
	}
	if v := constant.SeedanceUnpricedCostMultiplier; v > 0 {
		return v
	}
	return 2.0
}

var unpricedMultiplierOverride float64

// setSeedanceUnpricedMultiplierForTest 测试注入专用（传 0 恢复配置值）。
func setSeedanceUnpricedMultiplierForTest(v float64) {
	unpricedMultiplierOverride = v
}

// videoPriceKey 价格表的键：输出分辨率档与输入是否含视频。
// 零值键表示 480p/720p（以及未显式指定分辨率时采用的基准档）且无视频输入。
// 480p/720p 的每秒价格因生成 Token 数不同而不同，但每 Token 单价相同，
// 因此两者共用一个计费档位。
type videoPriceKey struct {
	is1080p  bool
	is4k     bool
	hasVideo bool
}

// videoPriceTable 各模型在不同 (输出分辨率档, 是否含视频输入) 下的单价（元/百万 token）。
// 其中零值键 {480p/720p, 不含视频} 为基准价，等于管理员应配置的 ModelRatio；
// 计费时取 实际单价/基准价 作为 OtherRatio。
var videoPriceTable = map[string]map[videoPriceKey]float64{
	"doubao-seedance-2-0-260128": {
		{hasVideo: false}:                46.0,
		{hasVideo: true}:                 28.0,
		{is1080p: true, hasVideo: false}: 51.0,
		{is1080p: true, hasVideo: true}:  31.0,
		{is4k: true, hasVideo: false}:    26.0,
		{is4k: true, hasVideo: true}:     16.0,
	},
	"doubao-seedance-2-0-fast-260128": {
		{hasVideo: false}: 28.0,
		{hasVideo: true}:  16.6,
	},
	"doubao-seedance-2-0-mini-260615": {
		{hasVideo: false}: 9.5,
		{hasVideo: true}:  5.8,
	},
	"doubao-seedance-2-5-260628": {
		{hasVideo: false}:                70.7,
		{hasVideo: true}:                 42.4,
		{is1080p: true, hasVideo: false}: 77.8,
		{is1080p: true, hasVideo: true}:  46.5,
	},
}

func seedanceVideoPriceKey(tier string, hasVideo bool) videoPriceKey {
	return videoPriceKey{
		is1080p:  tier == "1080p",
		is4k:     tier == "4k",
		hasVideo: hasVideo,
	}
}

// GetVideoInputRatio 返回指定模型在给定输出分辨率/是否含视频输入下，相对基准价的
// 计费倍率（= 实际组合单价 / 基准组合单价，允许 < 1）。
// 第二个返回值表示该模型是否配置了价格表；倍率为 1.0 时调用方可忽略该 OtherRatio。
//
// 注意：真实结算倍率与预扣缓冲的统一计算入口是 EstimateSeedancePricing——
// 本函数只暴露"组合单价倍率"本身，供需要单独读取单价档位的调用方使用。
func GetVideoInputRatio(modelName, resolution string, hasVideo bool) (float64, bool) {
	prices, ok := videoPriceTable[modelName]
	base := prices[videoPriceKey{}] // 零值键 = {480p/720p, 不含视频} 基准价
	if !ok || base <= 0 {
		return 0, false
	}
	tier, ok := NormalizeResolution(resolution)
	if !ok {
		return 1.0, true // 未知分辨率：调用方应走校验 400 / fail closed
	}
	price, ok := prices[seedanceVideoPriceKey(tier, hasVideo)]
	if !ok || price <= 0 {
		// 未配置的组合（如 fast 无 1080p/4k，上游会自行报错）按基准价计费即可。
		return 1.0, true
	}
	return price / base, true
}

// ---------------------------------------------------------------------------
// 模型版本归一化（修复公开别名无法命中价格表）
// ---------------------------------------------------------------------------

// seedancePublicAliases 公开别名 → 规范模型版本（videoPriceTable 的键）。
// 渠道模型映射可能把公开名映射为 Endpoint ID（ep-xxx），而价格表只认规范
// 版本名；别名表是显式、可测试的归一化入口，Endpoint ID 绝不直接当版本用。
var seedancePublicAliases = map[string]string{
	"doubao-seedance-2.0":      "doubao-seedance-2-0-260128",
	"doubao-seedance-2-0":      "doubao-seedance-2-0-260128",
	"doubao-seedance-2.0-fast": "doubao-seedance-2-0-fast-260128",
	"doubao-seedance-2-0-fast": "doubao-seedance-2-0-fast-260128",
	"doubao-seedance-2.0-mini": "doubao-seedance-2-0-mini-260615",
	"doubao-seedance-2-0-mini": "doubao-seedance-2-0-mini-260615",
	"doubao-seedance-2.5":      "doubao-seedance-2-5-260628",
	"doubao-seedance-2-5":      "doubao-seedance-2-5-260628",
}

// ResolveSeedancePriceModel 解析用于价格表选择的规范模型版本，优先级：
//  1. 优先采用**明确且命中价格表**的 upstream 映射版本（模型映射是管理员在
//     渠道上显式配置的权威信息，如公开名 doubao-seedance-2.0 被映射为 fast
//     规范版本时，必须按 fast 的价格表校验/估算——否则 fast 不支持的 1080p
//     会被错误放行）；
//  2. ep-* 前缀（方舟 Endpoint ID）不得直接作为价格版本，跳过；
//  3. upstream 无法识别时才回退公开模型别名（seedancePublicAliases）；
//  4. 都未命中 → ""（无价格表模型，走 unpriced fail closed）。
func ResolveSeedancePriceModel(origin, upstream string) string {
	up := strings.TrimSpace(upstream)
	if up != "" && !strings.HasPrefix(strings.ToLower(up), "ep-") {
		if _, ok := videoPriceTable[up]; ok {
			return up // upstream 命中价格表 → 优先采用
		}
	}
	if v, ok := seedancePublicAliases[strings.TrimSpace(origin)]; ok {
		return v
	}
	return ""
}

// HasSeedancePriceTable 判断规范化模型版本是否配置了官方价格表。
func HasSeedancePriceTable(modelVersion string) bool {
	if modelVersion == "" {
		return false
	}
	prices, ok := videoPriceTable[modelVersion]
	return ok && prices[videoPriceKey{}] > 0
}

// ---------------------------------------------------------------------------
// 时长解析（三态：合法 / 缺失 / 无法解析）
// ---------------------------------------------------------------------------

// DurationParseOutcome 时长解析结果。
type DurationParseOutcome int

const (
	DurationParseOK         DurationParseOutcome = iota // 可可靠解析且 >0
	DurationParseMissing                                // 未提供（上游用默认值，合法请求）
	DurationParseUnparsable                             // 提供了但无法解析/非法（发送上游前 400）
)

// ResolveSeedanceDurationEx 解析生成秒数，三态返回（预扣与校验同源）。
// 优先顺序（与 convertToRequestPayload 的取值一致，保证预扣与上游请求同源）：
//  1. req.Seconds（字符串，如 "10"）；
//  2. req.Duration（JSON 数字或字符串，TaskSubmitReq.UnmarshalJSON 已归一化）；
//  3. metadata["duration"] / metadata["seconds"]（大小写不敏感，见 metadataIntValue）。
//
// 注意：metadata 浮点小数（如 2.5）不得 int 截断伪装成合法时长 → Unparsable。
func ResolveSeedanceDurationEx(req *relaycommon.TaskSubmitReq) (int, DurationParseOutcome) {
	if req == nil {
		return 0, DurationParseMissing
	}
	if strings.TrimSpace(req.Seconds) != "" {
		sec, err := strconv.Atoi(strings.TrimSpace(req.Seconds))
		if err != nil || sec <= 0 {
			return 0, DurationParseUnparsable // 传了但坏：明确非法
		}
		return sec, DurationParseOK
	}
	if req.Duration != 0 {
		if req.Duration < 0 {
			return 0, DurationParseUnparsable
		}
		return req.Duration, DurationParseOK
	}
	if v, ok := metadataIntValue(req.Metadata, "duration"); ok {
		return v, DurationParseOK
	}
	if v, ok := metadataIntValue(req.Metadata, "seconds"); ok {
		return v, DurationParseOK
	}
	if hasMetadataKey(req.Metadata, "duration") || hasMetadataKey(req.Metadata, "seconds") {
		// metadata 里声明了时长字段但值无法解析 → 明确非法，不得静默按缺失处理
		return 0, DurationParseUnparsable
	}
	return 0, DurationParseMissing
}

// ResolveSeedanceDuration 兼容旧调用：返回 (秒数, 是否缺失/无法解析)。
// 新代码请使用 ResolveSeedanceDurationEx（三态）。
func ResolveSeedanceDuration(req *relaycommon.TaskSubmitReq) (int, bool) {
	d, outcome := ResolveSeedanceDurationEx(req)
	return d, outcome != DurationParseOK
}

// metadataStringValue 从 metadata 读取字符串字段（大小写不敏感）。
func metadataStringValue(metadata map[string]interface{}, key string) string {
	if metadata == nil {
		return ""
	}
	raw, ok := metadata[key]
	if !ok {
		for k, v := range metadata {
			if strings.EqualFold(k, key) {
				raw, ok = v, true
				break
			}
		}
	}
	if !ok {
		return ""
	}
	if s, ok := raw.(string); ok {
		return s
	}
	return ""
}

// hasMetadataKey 大小写不敏感地判断 metadata 是否含指定 key。
func hasMetadataKey(metadata map[string]interface{}, key string) bool {
	if metadata == nil {
		return false
	}
	if _, ok := metadata[key]; ok {
		return true
	}
	for k := range metadata {
		if strings.EqualFold(k, key) {
			return true
		}
	}
	return false
}

// metadataIntValue 从 metadata 读取数值字段，兼容字符串/数字、**大小写不敏感**。
// 浮点值必须是整数值（如 5.0），2.5 这类小数 → 不可靠解析（返回 false 且
// 由调用方结合 hasMetadataKey 判定 Unparsable，绝不 int 截断伪装合法时长）。
func metadataIntValue(metadata map[string]interface{}, key string) (int, bool) {
	if metadata == nil {
		return 0, false
	}
	raw, ok := metadata[key]
	if !ok {
		for k, v := range metadata {
			if strings.EqualFold(k, key) {
				raw, ok = v, true
				break
			}
		}
	}
	if !ok {
		return 0, false
	}
	switch v := raw.(type) {
	case float64:
		if v != math.Trunc(v) || v <= 0 || math.IsInf(v, 0) || math.IsNaN(v) {
			return 0, false // 小数/非正数/无穷 → 不可靠
		}
		return int(v), true
	case float32:
		f := float64(v)
		if f != math.Trunc(f) || f <= 0 {
			return 0, false
		}
		return int(f), true
	case int:
		if v <= 0 {
			return 0, false
		}
		return v, true
	case int64:
		if v <= 0 {
			return 0, false
		}
		return int(v), true
	case string:
		s := strings.TrimSpace(v)
		if s == "" {
			return 0, false
		}
		if n, err := strconv.Atoi(s); err == nil && n > 0 {
			return n, true
		}
	}
	return 0, false
}

// ---------------------------------------------------------------------------
// 定价倍率与预扣缓冲（两概念分离）
// ---------------------------------------------------------------------------

// SeedanceBillingParams 估算 Seedance 任务计费所需的标准化请求参数。
type SeedanceBillingParams struct {
	Model      string // 规范化模型版本（ResolveSeedancePriceModel 结果；可空表示无表）
	Resolution string // 输出分辨率原始值（大小写不敏感；空表示未指定）
	Duration   int    // 标准化后的生成秒数（>0；<=0 表示缺失，按基准 5 秒）
	HasVideo   bool   // 请求是否包含 video_url 视频输入
	UsePrice   bool   // 固定价格模式（true 时预扣不加时长缓冲，费用固定）
}

// SeedancePricingEstimate 一次计费估算的结果。
type SeedancePricingEstimate struct {
	// PricingRatios 真实结算倍率（写入 PriceData.OtherRatios 驱动结算），键
	// "size" 承载"请求实际组合单价 / 基准组合单价"（**允许 < 1**，如 28/46、
	// 31/46、26/46、16/46；组合倍率 = 1.0 时省略）。绝不包含时长缓冲，也绝不
	// 为保守预扣把 <1 的真实倍率抬到 1。
	PricingRatios map[string]float64
	// PricingMultiplier 真实结算倍率（PricingRatios 的乘积；允许 < 1）。
	// 结算 = actualTokens × PricingMultiplier。
	PricingMultiplier float64
	// PreConsumeMultiplier 仅预扣缓冲 = durationSafety × conservativePriceRatio /
	// actualPricingMultiplier。只用于提交前保守预留，绝不进入 totalTokens 结算；
	// 固定价格模式为 1.0。
	PreConsumeMultiplier float64
	// PricedModel 是否命中官方价格表（false = 使用可配置保守系数，fail closed）。
	PricedModel bool
	// ResolutionFellBack 分辨率缺失/未知：预扣已按表内最高单价比档 fail closed，
	// 且真实结算倍率保持 1.0（fallback 绝不进入真实结算）。
	ResolutionFellBack bool
}

// EstimateSeedancePricing 计算 Seedance 任务的真实结算倍率与保守预扣缓冲
// （见文件头注释的契约说明）。params.Model 必须是 ResolveSeedancePriceModel
// 的规范化结果（空表示无价格表模型）。
//
// 两个概念的严格分离（videoPriceTable 的每个键已是 (分辨率档, hasVideo) 的
// 组合单价，**绝不是** sizeRatio 与 videoInputRatio 两个独立价格的乘积）：
//
//	actualPricingMultiplier = 请求实际组合单价 / 基准组合单价
//	  - 允许 < 1（如 28/46、31/46、26/46、16/46）；写入 PricingRatios["size"]
//	    供真实 Token 结算使用：结算 = actualTokens × actualPricingMultiplier；
//	  - 缺失或无法可靠确定的组合（未知分辨率）→ actualPricingMultiplier 保持
//	    1.0 且不写 PricingRatios——该 fallback **绝不进入真实结算**。
//	preConsumeMultiplier = durationSafety × conservativePriceRatio / actualPricingMultiplier
//	  - 仅用于提交前保守预扣：
//	    预扣 = baseQuota × actualPricingMultiplier × preConsumeMultiplier
//	         = baseQuota × durationSafety × conservativePriceRatio
//	         ≥ baseQuota × durationSafety × actualPricingMultiplier（永不低估）；
//	  - conservativePriceRatio 按分辨率/时长/是否视频输入保守计算：取该模型
//	    价格表内最大单价比档（token 数无法证明随单价同比例变化，低单价档
//	    （如 4k 26/46、视频输入 28/46）不得用来缩小预扣）；无价格表模型取
//	    可配置保守系数 SeedanceUnpricedCostMultiplier（fail closed）。
func EstimateSeedancePricing(params SeedanceBillingParams) SeedancePricingEstimate {
	est := SeedancePricingEstimate{PricingRatios: make(map[string]float64, 1)}
	rawResolution := strings.ToLower(strings.TrimSpace(params.Resolution))
	tier, resOK := NormalizeResolution(rawResolution)
	if !resOK {
		est.ResolutionFellBack = true // 未知分辨率：校验阶段已先行 400，此处防御性兜底
	}

	// ---- 时长预扣安全系数（仅预扣；固定价格模式费用固定不加缓冲）----
	durationSafety := 1.0
	if !params.UsePrice {
		duration := params.Duration
		if duration <= 0 {
			duration = SeedanceBaseDurationSeconds // 缺失 → 实测基准 5 秒
		}
		if duration < SeedanceBaseDurationSeconds {
			duration = SeedanceBaseDurationSeconds
		}
		durationSafety = float64(duration) / SeedanceBaseDurationSeconds
		if !isFinitePositive(durationSafety) || durationSafety < 1.0 {
			durationSafety = 1.0
		}
	}
	est.PreConsumeMultiplier = durationSafety

	// ---- 无价格表模型：fail closed 保守系数（只进预扣，绝不污染真实结算）----
	prices, hasPriceTable := videoPriceTable[params.Model]
	if !hasPriceTable || prices[videoPriceKey{}] <= 0 {
		est.PricedModel = false
		est.ResolutionFellBack = true
		est.PricingMultiplier = 1.0 // 真实 Token 结算不乘任何定价倍率
		if !params.UsePrice {
			mult := SeedanceUnpricedCostMultiplier()
			if !isFinitePositive(mult) {
				mult = 1.0
			}
			est.PreConsumeMultiplier *= mult
		}
		// 固定价格模式（UsePrice）：费用固定、无超支风险、不产生需退差额 → 缓冲恒为 1.0。
		if params.UsePrice {
			est.PreConsumeMultiplier = 1.0
		}
		return est
	}
	est.PricedModel = true
	base := prices[videoPriceKey{}]

	// ---- 真实结算倍率 = 实际组合单价 / 基准组合单价（允许 < 1）----
	actualMultiplier := 1.0
	comboKnown := false
	if !est.ResolutionFellBack {
		if price, ok := prices[seedanceVideoPriceKey(tier, params.HasVideo)]; ok && price > 0 {
			actualMultiplier = price / base
			comboKnown = true
		}
	}
	if comboKnown && actualMultiplier != 1.0 {
		// 单键承载组合倍率（sizeRatio × videoInputRatio 的旧相乘语义已废弃）
		est.PricingRatios["size"] = actualMultiplier
	}
	est.PricingMultiplier = actualMultiplier

	// ---- 保守价格倍率（仅预扣）：表内最大单价比档，缺失/无法确定档位 fail closed ----
	maxRatio := 0.0
	for _, price := range prices {
		if price <= 0 {
			continue
		}
		if r := price / base; r > maxRatio {
			maxRatio = r
		}
	}
	if !isFinitePositive(maxRatio) || maxRatio < 1.0 {
		maxRatio = 1.0
	}
	conservativePriceRatio := maxRatio

	// ---- 预扣缓冲 = durationSafety × conservativePriceRatio / actualPricingMultiplier ----
	// 固定价格模式（UsePrice）：费用固定、无超支风险、不产生需退差额 → 缓冲恒为 1.0
	// （预扣 = baseQuota × actualPricingMultiplier，结算按固定价格路径处理）。
	if params.UsePrice {
		est.PreConsumeMultiplier = 1.0
	} else {
		settlementMultiplier := actualMultiplier
		if !isFinitePositive(settlementMultiplier) {
			settlementMultiplier = 1.0 // 组合未知：结算按 1.0，预扣按保守最高档
		}
		est.PreConsumeMultiplier = durationSafety * conservativePriceRatio / settlementMultiplier
		if !isFinitePositive(est.PreConsumeMultiplier) {
			est.PreConsumeMultiplier = 1.0
		}
	}
	return est
}

// NormalizeResolution 把原始分辨率归一化为价格表档位（"" = 480p/720p/默认基准档）。
// 未知格式返回 "" 且 ok=false（校验阶段据此 400，不低价预扣后提交）。
func NormalizeResolution(res string) (tier string, ok bool) {
	switch strings.ToLower(strings.TrimSpace(res)) {
	case "1080p", "1080", "fhd":
		return "1080p", true
	case "4k", "4k ultra", "ultra":
		return "4k", true
	case "480p", "720p", "720", "480":
		return "", true
	case "":
		return "", true // 未指定：上游默认，合法
	default:
		return "", false // 未知分辨率：校验阶段拒绝
	}
}

func isFinitePositive(v float64) bool {
	return v > 0 && !math.IsNaN(v) && !math.IsInf(v, 0)
}

// quotaFromSeedanceEstimate 把定价倍率 + 预扣缓冲安全地换算为预扣额度。
// 输入为"基础配额 × 定价倍率 × 预扣缓冲"的浮点值，输出经饱和转换（禁止
// 浮点截断成负数或整数溢出，见 common.QuotaFromFloatChecked）。
func quotaFromSeedanceEstimate(baseQuota int, est SeedancePricingEstimate) (int, *common.QuotaClamp) {
	multiplier := est.PricingMultiplier * est.PreConsumeMultiplier
	if !isFinitePositive(multiplier) {
		multiplier = 1.0
	}
	return common.QuotaFromFloatChecked(float64(baseQuota) * multiplier)
}

// ValidateSeedanceResolutionForModel 校验分辨率与价格表的支持组合：
//   - 未知分辨率 → 400（任何模型）；
//   - 有价格表模型：检查价格表中的完整 (分辨率档, hasVideo) 组合——显式档
//     （1080p/4k）在表内缺失（无论是否视频输入）→ 400（如 fast 模型无
//     1080p/4k 档）；**基准档（480p/720p/空分辨率）同样检查**：tier=="" 时
//     映射到零值档键，必须存在对应的 hasVideo
//     组合（{hasVideo:false} 由 HasSeedancePriceTable 保证，{hasVideo:true}
//     必须显式在表内，缺失即 400——空分辨率绝不能绕过 hasVideo 组合检查）；
//   - 无价格表模型：无法判断支持范围，不拒绝（fail closed，预扣走保守系数）。
//
// hasVideo 表示请求是否包含视频输入（与结算倍率取用同一 (resolution, hasVideo)
// 组合，不能固定检查 hasVideo=false——否则"仅视频输入支持的高档"会被误判）。
// 返回错误信息；空字符串表示合法。
func ValidateSeedanceResolutionForModel(modelVersion, resolution string, hasVideo bool) string {
	_, ok := NormalizeResolution(resolution)
	if !ok {
		return fmt.Sprintf("unsupported seedance resolution: %q", resolution)
	}
	if !HasSeedancePriceTable(modelVersion) {
		return "" // 无表模型不校验（无法判断），预扣 fail closed
	}
	tier, _ := NormalizeResolution(resolution)
	prices := videoPriceTable[modelVersion]
	// 统一检查完整 videoPriceKey：tier=="" 时所有分辨率标记均为 false（基准档），
	// 仍必须校验 hasVideo 组合，绝不放行价格表缺失的基准档视频输入。
	if _, exists := prices[seedanceVideoPriceKey(tier, hasVideo)]; !exists {
		return fmt.Sprintf("resolution %q (hasVideo=%t) not supported by model %s", resolution, hasVideo, modelVersion)
	}
	return ""
}
