package constant

var StreamingTimeout int
var DifyDebug bool
var MaxFileDownloadMB int
var StreamScannerMaxBufferMB int
var ForceStreamOption bool
var CountToken bool
var GetMediaToken bool
var GetMediaTokenNotStream bool
var UpdateTask bool
var MaxRequestBodyMB int
var AnonymousRequestBodyLimitKB int
var AzureDefaultAPIVersion string
var NotifyLimitCount int
var NotificationLimitDurationMinute int
var GenerateDefaultToken bool
var ErrorLogEnabled bool
var TaskQueryLimit int
var TaskTimeoutMinutes int

// SeedanceMaxConcurrentTasks 单个用户最多同时运行（queued/processing 等非终态）的
// Seedance 任务数量。0 或负数表示不限制。可通过环境变量 SEEDANCE_MAX_CONCURRENT_TASKS 调整。
var SeedanceMaxConcurrentTasks int

// SeedanceConcurrencyReconcileTTLMinutes 并发名额对账（兜底清理）的过期阈值（分钟）：
// 名额计数与真实运行任务数不一致、且计数行超过该时长未被更新的，视为异常残留并修复。
var SeedanceConcurrencyReconcileTTLMinutes int

// SeedanceDailyCostAlertUSD / SeedanceDailyCostLimitUSD are site-wide daily
// upstream-cost guardrails. Values <= 0 disable the corresponding guardrail.
var SeedanceDailyCostAlertUSD float64
var SeedanceDailyCostLimitUSD float64

// SeedanceUnpricedCostMultiplier 无官方价格表模型的 Seedance 保守预扣系数
// （doubao adaptor 会读该值；默认 2.0，见 relay/channel/task/doubao/constants.go
// 的契约注释）。生产可按账单对账结果调整。
var SeedanceUnpricedCostMultiplier float64

// SeedanceMaxSupportedDurationSeconds 本地允许的 Seedance 生成时长上限
// （doubao adaptor 会读该值；默认 10 秒，Seedance 文档支持 5/10 秒）。
var SeedanceMaxSupportedDurationSeconds int

// temporary variable for sora patch, will be removed in future
var TaskPricePatches []string

// TrustedRedirectDomains is a list of trusted domains for redirect URL validation.
// Domains support subdomain matching (e.g., "example.com" matches "sub.example.com").
var TrustedRedirectDomains []string
