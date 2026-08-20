# New API 海外中转站开发与上线清单

> 目标：将当前项目部署为面向海外用户的视频生成 API 中转站，首发只接入火山方舟 Seedance。
>
> 执行顺序：本地准备 → 部署但不开放 → 生产验收与灰度 → 正式开放与维护。

## 标记说明

- `[x]`：当前仓库或本地环境已经确认完成。
- `[ ]`：仍需执行或验收，不代表项目一定没有该功能。
- `【已有】`：项目代码已经具备。
- `【配置】`：已有能力，需要填写生产参数并启用。
- `【验证】`：需要使用生产环境实际测试。
- `【二开】`：当前仓库未发现完整能力，需要开发。
- `【外部】`：需要服务器、域名、对象存储或监控服务配合。
- `P0`：正式开放前必须完成；`P1`：灰度后尽快完成；`P2`：增强项。

## 0. 当前状态

- [x] 已创建本地二开分支 `custom-development`
- [x] 已禁止向 GitHub 误推送，仍可拉取上游更新
- [x] 本地开发数据库已切换为 PostgreSQL 15
- [x] 本地 Redis 7 已启用
- [x] 后端已启用 Air 热重载，修改 Go 代码无需手工 rebuild
- [x] 前端开发服务器可正常运行
- [x] 已提供 `start-dev.bat` 一键启动前后端
- [x] 当前仓库已支持火山方舟 Seedance 视频渠道（代码内部类型名为 `DoubaoVideo`）
- [x] 当前仓库已包含 Seedance 2.0 和 Seedance 2.0 Fast 模型名称
- [x] 当前仓库已具备异步任务、预扣费、差额结算和失败退款基础逻辑
- [x] 已明确为面向海外用户的公网服务
- [x] 首批上游供应商确定为火山方舟（Volcengine Ark）
- [x] PostgreSQL 中已存在 1 个管理员账号
- [x] Seedance 计费/轮询相关的 `service`、`middleware`、`model` 后端测试已通过
- [x] 首发生成资源访问安全已修复：非首发资源路由强制鉴权并校验所有者，Seedance 不再返回可绕过本地授权的上游直链
- [x] Seedance 上游 401、429、500/503、响应超时、上游余额不足及本地余额不足的自动化故障注入验证已通过；失败退款、并发名额、渠道禁用、禁止重复 POST、恢复记录和日志脱敏行为符合预期
- [x] 当前数据库已创建 2 个独立渠道、1 个模型元数据、2 个受限测试 Token，并完成 2 个 Seedance 成功任务
- [x] 首批目标地区确定为中亚地区；第二阶段服务器区域仍需结合中亚用户访问延迟与到火山方舟 Endpoint 的实测结果选择
- [x] 首发阶段不启用在线支付：不确认支付合规条款，不配置任何支付网关凭据，仅由管理员向受限测试账号发放额度
- [x] 已在 VPS `2.24.120.8` 完成生产部署，正式域名为 `globalaiclient.com`，Caddy、应用、PostgreSQL 16 和 Redis 7 容器运行健康
- [x] 本地开发数据库已迁移至生产 PostgreSQL；生产环境保持关闭注册和密码注册，并清除迁移带入的浏览器会话
- [x] 生产站点已启用 JadeRoute 名称与标志，首页定位统一调整为“主流大模型”API 基础设施
- [ ] 管理员尚未启用 Passkey 或 2FA（多次遗留，正式开放门禁 P0）
- [x] 确定首批目标国家或地区：中亚地区 `P0`
- [ ] 确定计划灰度日期和正式开放日期 `P1`

### 0.1 功能实现审计（更新至 2026-08-20）

| 功能 | 代码状态 | 当前实际状态 | 结论 |
|---|---|---|---|
| 管理员、用户和权限 | `【已有】` | 已有 1 个管理员账号 | 已实现 |
| Passkey / 2FA | `【已有】` | 数据库中尚无 Passkey 和 2FA 凭据 | 已实现，未启用 |
| 邮箱验证与找回密码 | `【已有】` | 尚未检测到邮件服务配置 | 已实现，待配置 |
| 渠道管理 | `【已有】` | 已创建并启用 `seedance-test`、`seedance-prod` | 测试/生产已按用户组和上游 Endpoint 隔离，并通过真实任务验证 |
| 模型映射、优先级、权重和分组 | `【已有】` | 两个渠道均开放 `doubao-seedance-2.0`，分别映射到独立 Endpoint；优先级 10、权重 100 | 已配置并验证 |
| 渠道重试、自动禁用和恢复 | `【已有/已验证】` | 两个渠道已开启自动禁用；401、429、500/503、超时和余额不足已完成自动化故障注入；周期测试仍关闭 | 401 与上游余额不足会自动禁用，429、5xx 和超时默认不禁用；明确失败和结果未知均禁止重复 POST，真实生产故障演练仍待执行 |
| 用户 Token | `【已有】` | 已创建 `seedance-test-token` 与 `seedance-prod-smoke-token` | 分别绑定 `test`、`prod` 用户组 |
| Token 有效期、额度和模型范围 | `【已有】` | 两个 Token 均设置短期有效期、有限额度和单模型限制 | 已配置并完成正常调用验证；IP 白名单待验证 |
| 用户额度、预扣费和结算 | `【已有/已验证】` | 测试、生产各完成 1 个 5 秒 480P 成功任务；8-19 线上验证预扣与火山官方 token 公式对齐（预扣/结算≈1.15），1080p→720p 实际降档正确计费；失败全额退款实测通过 | 成功任务扣费、差额结算及失败退款代码路径已验证；真实方舟失败账单仍待生产对账 |
| Seedance 完成后补扣余额守卫 | `【已修复】` | 已按分辨率、时长和视频输入保守预扣；钱包与有限 Token 的差额补扣均带余额守卫，补扣失败进入欠款、冻结、告警和可审计恢复闭环（上游 #4526） | 负余额、欠款清偿及并发冻结回归测试已通过；`-race` 因本机缺少 `gcc` 未执行 `P0` |
| 首发生成资源访问安全 | `【已修复】` | Midjourney `/mj/image/:id` 等资源入口已强制鉴权并按用户查询任务，非所有者统一返回 404；Seedance 结果只返回鉴权代理入口，任务数据、错误信息和代理响应头均不再暴露可绕过本地授权的上游地址 | 匿名访问 401、跨用户访问 404、所有者访问 200、上游直链清洗及代理响应头脱敏回归测试已通过；私有视频响应使用 `Cache-Control: private, no-store` `P0` |
| 多模型统一资源访问机制 | `【规划中】` | 尚未建立统一资源记录、Provider 抽象和短期签名 URL | 启用 Midjourney、其他生成模型或对象存储/CDN 前完成；不作为仅 Seedance 首发的阻塞项 `P1` |
| API 请求与消费日志 | `【已有】` | 数据库已有日志记录 | 已实现 |
| Seedance 2.0 / Fast 模型适配 | `【已有】` | 已有 2 条 Seedance 2.0 成功任务记录 | 测试和生产渠道均已接通 Seedance 2.0 |
| Seedance 创建和状态查询 | `【已有】` | 已观察到 `queued`、`IN_PROGRESS`、`SUCCESS` 和 0%～100% 进度 | 创建、轮询和成功结果返回已验证 |
| 后台任务轮询 | `【已有】` | 系统任务表已运行，相关测试通过 | 已实现 |
| 失败退款与完成后差额结算 | `【已有/已验证】` | 401、429、500/503、响应超时和上游余额不足的自动化场景均确认预扣额度退回，尚未实测方舟失败账单 | 代码路径已通过集成测试，真实上游账单待生产验证 |
| 模型/分组请求速率限制 | `【部分已有】` | 默认关闭，且当前视频路由未挂载该中间件 | Seedance 尚不能直接使用此页面限制 |
| Seedance 客户端提交幂等 | `【已有】` | 已实现 `Idempotency-Key`、任务恢复与重复提交保护 | 单次真实调用已携带幂等键；相同键重复提交测试待补 |
| Seedance 单用户运行中任务限制 | `【已有】` | 已实现活跃任务计数、并发闸门与额度预占 | 运行时限额边界和并发压测待补 |
| 全站 Seedance 成本熔断 | `【已有】` | 已实现任务预估成本上限与拒绝逻辑 | 生产阈值确认和超限负向测试待补 |
| 视频转存对象存储/CDN | `【未实现】` | 当前主要保存或代理上游结果地址 | P1 二开 |
| PostgreSQL / Redis | `【已有】` | 本地容器运行，PostgreSQL 健康；均未暴露主机端口 | 本地已完成 |
| 生产 Compose | `【已部署】` | `/opt/new-api/deploy/compose.prod.yml` 使用独立后端/前端网络、持久化卷、健康检查、日志轮转和版本镜像 | 已生产化并部署 |
| 健康接口 `/api/status` | `【已有/已验证】` | 生产域名可访问，应用容器健康检查通过 | 已实现并完成生产验证 |
| 前端类型检查与生产构建 | `【已有】` | `bun run typecheck`、`bun run build` 已通过 | 当前通过 |
| 前端 lint | `【已有工具】` | `bun run lint` 未通过，仓库存在多处现有规则错误 | 尚未完成 |
| 后端应用与 relaykit 构建 | `【已有】` | 应用入口和独立 `relaykit` 模块构建通过 | 当前通过 |
| 容器监控、告警和自动备份 | `【部分完成】` | 已配置容器健康检查、日志轮转、每日 PostgreSQL 备份；Uptime Kuma 已启用健康监控；PG 连接池 500 已修复（SQL_MAX_OPEN_CONNS=80） | 备份已运行，健康监控已启用；CPU/磁盘/5xx 告警与异机备份仍待配置 |

> 2026-08-11 测试：在后端开发容器中执行 `go test -p 1 ./service ./relay/channel/task/doubao ./middleware ./model`。除豆包视频适配包暂无独立测试文件外，其余目标包均通过。
>
> 2026-08-14 安全回归：执行 `go test ./relay/ ./router/ ./relay/channel/task/doubao/ -count=1`，首发生成资源的鉴权、所有权校验、直链清洗及响应头脱敏相关用例全部通过。
>
> 2026-08-17 上游异常回归：执行 `go test ./relay -run "TestClassifyErrorKind" -count=1` 及 `go test ./controller -run "TestRelayTask(Upstream401|Upstream429|Upstream5xx|UpstreamTimeout|UpstreamBalanceInsufficient|LocalBalanceInsufficient|UpstreamSuccessControl|FailureLogCorrelation)$" -count=1`，分类单元测试和 8 个集成测试全部通过。超时恢复记录 Note 分类缺陷已在提交 `eb970be0` 修复。

### 0.2 管理员后台实测结果（2026-08-11）

以下内容来自已登录管理员后台的只读检查，没有保存或修改配置：

- [x] 管理员概览、渠道、模型、用户、API Key、普通日志、任务日志和系统信息页面均可正常打开
- [x] 当前有 1 个启用的 Root 管理员；普通用户、渠道、模型元数据、API Key 和 Seedance 任务均为 0
- [x] 普通日志页面已有 4 条登录/系统操作记录；任务日志为 0
- [x] 系统任务历史有 4 条上游模型批量更新任务，状态均为成功
- [x] 密码登录、开放注册和密码注册已启用
- [ ] 邮箱验证、邮箱域限制和邮箱别名限制当前未启用
- [ ] Passkey 当前未启用，依赖方 ID 和允许来源尚未填写
- [ ] Cloudflare Turnstile 当前未启用，站点密钥与密钥尚未填写
- [x] SSRF 保护已启用；不允许私有 IP，并会对域名解析后的 IP 再次过滤
- [ ] 模型请求速率限制当前未启用：周期 1 分钟、总请求数 0（无限制）、成功请求数 1000，未配置分组规则；且 Seedance 视频路由尚未接入该限制
- [ ] 请求重试次数当前为 0；定期渠道测试、成功后恢复和失败自动禁用均未启用
- [x] 每个用户最多创建 1000 个 API Key 的限制已存在
- [x] 新用户初始额度为 0；预消耗额度为 500；免费模型预消耗已启用
- [x] 模型定价、分组定价、上游价格同步和支付网关均有管理界面
- [x] Seedance 的具体售价、分组倍率和真实成本已核对（8-19：2-0 倍率 51/31、4k 26/16，预扣/结算≈1.15；结算对账 ¥1.03 平价，汇率 7.27）
- [x] 模型性能指标已启用，5 分钟写库一次
- [ ] 指标保留天数当前为 0（永久保留），上线前应根据数据库容量调整
- [x] 服务器公开 URL 已填写 `https://globalaiclient.com`；Uptime Kuma 已启用健康监控；CPU/磁盘/告警渠道仍待配置

### 0.3 渠道配置与真实任务验证（2026-08-12）

- [x] 火山方舟已创建独立的测试、生产在线推理接入点，模型均为 Doubao-Seedance-2.0 `260128`，状态健康。
- [x] 火山方舟已创建独立的测试、生产 API Key；New API 数据库已确认两个渠道使用不同密钥。
- [x] New API 已创建 `test`、`prod` 分组，分组倍率均为 1。
- [x] New API 已创建测试渠道 `seedance-test` 和生产渠道 `seedance-prod`，分别绑定对应分组、Endpoint 与 API Key。
- [x] 对外模型统一为 `doubao-seedance-2.0`，渠道内部映射至各自的上游 Endpoint。
- [x] `/v1/models` 使用生产 Token 查询成功，只返回公开模型名 `doubao-seedance-2.0`。
- [x] 测试渠道完成 5 秒 480P 文生视频：经历 `queued`、`IN_PROGRESS`、`SUCCESS`，最终 50638 tokens、结算配额 159545。
- [x] 生产渠道完成 5 秒 480P 文生视频：公开模型名、`prod` 分组和生产渠道命中正确，最终 50638 tokens、结算配额 159545。
- [x] 任务状态详情已移除 `properties.upstream_model_name`；任务 DTO、恢复记录、错误响应和日志视图中的 Endpoint ID/API Key 泄露旁路已完成脱敏并通过回归测试。
- [ ] 尚未验证图生视频、相同幂等键重复提交、成本熔断、备用渠道和方舟账单三方对账（8-19 已补充自建 vs 上游的结算对账，¥1.03 平价；失败全额退款已实测 copyright_restriction 场景）；真实方舟故障账单仍待生产验收。

### 0.4 上游异常自动化验证（2026-08-17）

以下结果来自 `httptest` Mock 上游与测试数据库的实际集成测试，不等同于真实方舟生产故障演练：

| 场景 | 客户端结果 | 提交与计费 | 并发与渠道 | 结论 |
|---|---|---|---|---|
| 上游 401 | HTTP 401，`fail_to_fetch_task`，Endpoint ID/API Key 脱敏 | POST 1 次、不重试、预扣退回 | 名额释放、渠道自动禁用 | 通过 |
| 上游 429 | HTTP 429，`fail_to_fetch_task`，提示当前分组负载已饱和 | POST 1 次、不重试、预扣退回 | 名额释放、渠道不禁用 | 通过 |
| 上游 500/503 | 原状态码透传，`fail_to_fetch_task` | 每种状态 POST 1 次、不重试、预扣退回 | 名额释放、渠道不禁用 | 通过 |
| 请求已送达后响应超时 | HTTP 502，`task_submit_outcome_unknown`，提示结果未知 | POST 1 次、不重试、预扣退回 | 恢复记录持有名额、渠道不禁用，Note=`outcome_unknown: timeout` | 通过 |
| 上游余额不足 | HTTP 400，`fail_to_fetch_task`，保留余额不足提示 | POST 1 次、不重试、预扣退回 | 名额释放、渠道按关键词自动禁用 | 通过 |
| 本地用户余额不足 | HTTP 403，`insufficient_user_quota` | 上游 POST 0 次、不发生预扣 | 名额释放、渠道不禁用 | 通过 |

成功控制组确认 Mock 上游链路确实被调用、任务能够创建并扣费；失败日志串联测试确认包含状态码、重试次数和请求 ID，且不泄露渠道 API Key、Bearer Token 或 Endpoint ID。超时分类修复及回归测试见提交 `eb970be0`。

## 1. 第一阶段：上线前的本地准备

这一阶段全部在本地完成，不需要先把系统暴露到公网。

### 1.1 首发模型：仅 Seedance

- [x] 使用生产方舟账号确认模型的最新完整 ID、价格和 Region 可用性 `P0`
- [x] 主模型：对外模型名为 `doubao-seedance-2.0`，上游实际版本为 `doubao-seedance-2-0-260128` `P0`
- [x] 快速模型 `doubao-seedance-2-0-fast` 已实测（8-19 线上 8 个任务含 fast 10s，结算 415,970 quota/次）`P1`
- [x] 为对外模型设置稳定别名 `doubao-seedance-2.0`，通过渠道模型映射指向具体上游 Endpoint `P1`
- [x] 不向客户端暴露方舟 Endpoint ID 和 API Key `P0`
  - 已移除任务详情中的 `properties.upstream_model_name`，并统一覆盖任务数据、恢复记录、客户端错误、日志 API 与运行时日志出口；Endpoint ID/API Key 格式变体、嵌套结构及 `expr_b64` 均已补充脱敏测试。

### 1.2 火山方舟渠道

- [x] `【已配置并验证】` 创建独立的测试渠道 `seedance-test` 和生产渠道 `seedance-prod` `P0`
- [x] `【已配置并验证】` 两个渠道使用独立 Endpoint 与独立 API Key，数据库已确认密钥不同 `P0`
- [x] `【已配置并验证】` 两个渠道均支持 `doubao-seedance-2.0`，并分别映射到对应 Endpoint `P0`
- [x] `【已配置并验证】` 两个渠道优先级 10、权重 100，用户组分别为 `test`、`prod` `P0`
- [x] `【已验证】` 两个渠道均已通过真实 5 秒 480P 文生视频任务验证 `P0`
- [x] `【自动化验证】` 验证上游 401、429、500/503、响应超时、上游余额不足和本地余额不足的处理结果；失败退款、并发名额、渠道禁用、恢复记录、禁止重复 POST 及日志脱敏均通过 `P0`
- [ ] 为 Seedance 准备备用 Endpoint 或备用渠道 `P1`

### 1.3 需要二开的功能

- [x] `【二开】` 为 Seedance 创建接口增加客户端幂等键，避免重复 POST 创建多个任务 `P0`
- [x] `【二开】` 限制单用户同时运行的 Seedance 任务数量 `P0`
- [x] `【二开/外部】` 增加全站 Seedance 成本告警与自动熔断；只阻止新任务，不影响已提交任务 `P0`
- [x] `【二开/已修复】` 修复 Seedance 差额补扣可能产生负余额：已按分辨率、时长和视频输入保守预扣，余额不足时禁止无条件补扣，并完成欠款/冻结/告警处理与并发回归测试`P0`
- [x] `【安全二开/已修复】` 已禁用未鉴权的 `/mj/image/:id` 等非首发模型资源路由；Seedance 对外资源只通过鉴权且校验任务所有者的入口访问，不返回可绕过本地授权的上游直链；匿名访问、用户 A 越权访问用户 B 资源及所有者正常访问测试均已通过（上游 #6610）`P0`
- [ ] `【安全二开/后续模型启用前】` 建立可供 Seedance、Midjourney 及后续 Provider 复用的统一生成资源机制：统一资源记录关联任务所有者与 Provider，对外支持短期签名 URL，并衔接对象存储/CDN；不阻塞仅 Seedance 首发 `P1`
- [ ] `【启用支付前，当前不阻塞】` 为所有充值下单入口增加单笔额度上限校验，确保客户付款前即可拒绝 `Amount × QuotaPerUnit` 超出额度域的订单，并在前端显示明确上限（上游 #6831）；首发阶段已决定关闭在线支付 `P0（启用支付时）`
- [ ] `【外部+二开】` 将成功视频转存到自有对象存储和 CDN `P1`

> 当前任务计费方式是“提交前扣除预计额度 → 在任务中保存预扣额度 → 完成后差额结算 → 失败后退款”，不是独立冻结余额账本。

### 1.4 本地代码验收

- [x] 前端执行 `bun run typecheck` `P0`
- [x] 前端执行 `bun run lint`：当前存在仓库原有 lint 错误 `P0`
- [x] 前端执行 `bun run build` `P0`
- [x] 后端 `service`、豆包视频适配、`middleware`、`model` 目标包测试通过 `P0`
- [x] 后端应用入口构建通过 `P0`
- [x] 独立 `relaykit` 模块执行 `GOWORK=off go build ./...` 通过 `P0`
- [x] 验证注册、登录、退出和刷新 Session `P0`
- [x] 验证用户、分组、渠道、模型、Token 和日志的权限隔离 `P0`

## 2. 第二阶段：部署到服务器但暂不开放

这一步把系统部署到生产服务器，但先限制访问，只供管理员验收。

### 2.1 生产部署文件

- [x] 固定首个中亚部署源码基线：Git 标签 `deploy-central-asia-20260817-rc1` `P0`
- [x] `【已生产化】` 单独准备生产 Compose，不复用 `docker-compose.dev.yml` `P0`
- [x] 从固定 Git Commit `579f677b` 构建版本镜像 `new-api-central-asia:579f677b`，记录摘要 `sha256:4c6e72794fe4e430d8b6896be26baff9ac3384a13e348204787552f5c07222e9`，未使用 `latest` `P0`
- [x] PostgreSQL、Redis 和应用使用不同的强随机密码；两项密码长度均为 48，`SESSION_SECRET` 长度为 96，三者互不相同 `P0`
- [x] 生成足够长的随机 `SESSION_SECRET` `P0`
- [x] 密钥通过权限为 `0600` 的生产 `.env` 注入，未写入 Compose 文件 `P0`
- [x] 设置 `GIN_MODE=release`，关闭 `DEBUG` 和 `ENABLE_PPROF` `P0`
- [x] 保持 `TLS_INSECURE_SKIP_VERIFY=false` `P0`
- [x] 支付合规条款已确认（`payment_setting.compliance_confirmed=true`，admin 操作，terms v1）；Stripe、Creem、Waffo、Waffo Pancake、易支付凭据全部为空，`WaffoEnabled`/`PayAddress`/`TopUpLink` 未配置（options 无对应 key，支付无法发起）；`/api/user/topup/info` 接口验证待正式开放前补（需认证）`P0`
- [x] 设置 `TZ=UTC` 和明确的 `NODE_NAME=new-api-prod-1` `P1`
- [ ] 配置合理的请求、任务轮询和上游连接超时 `P1`

生产环境变量至少包含：

```env
SQL_DSN=postgresql://APP_USER:STRONG_PASSWORD@postgres:5432/new-api
REDIS_CONN_STRING=redis://:STRONG_PASSWORD@redis:6379/0
SESSION_SECRET=LONG_RANDOM_SECRET
SESSION_COOKIE_SECURE=true
SESSION_COOKIE_TRUSTED_URL=https://api.example.com
TRUSTED_PROXIES=REVERSE_PROXY_CIDR
FRONTEND_BASE_URL=https://api.example.com
TZ=UTC
GIN_MODE=release
ERROR_LOG_ENABLED=true
BATCH_UPDATE_ENABLED=true
NODE_NAME=new-api-prod-1
```

### 2.2 数据库与 Redis

- [x] PostgreSQL 使用独立业务账号 `newapi`（非超级用户），密码 48 位 `P0`
- [x] PostgreSQL 和 Redis 只允许 Docker 内部网络访问，主机未监听 5432/6379 `P0`
- [x] PostgreSQL 使用持久化 Docker Volume `P0`
- [x] Redis 开启密码认证并使用持久化 Docker Volume `P0`
- [x] 在生产数据库副本上验证自动迁移 `P0`
- [x] 设置 PostgreSQL 每日自动备份：`/etc/cron.d/new-api-backup` 每日 03:15 UTC 执行 `backup.sh` `P0`
- [x] 将备份保存到服务器之外的位置 `P0`
- [x] 完成至少一次数据库恢复演练 `P0`

### 2.3 域名与 HTTPS

- [x] 正式域名 `globalaiclient.com` 与 `www.globalaiclient.com` 已完成 DNS 解析 `P0`
- [x] 使用 Caddy 作为唯一 Web 公网入口 `P0`
- [x] 配置可信 TLS 证书和自动续期，两个域名 HTTPS 均返回 200 `P0`
- [x] 仅监听 80/443 和必要的 SSH 管理端口 22 `P0`
- [x] 不直接暴露应用 3000、PostgreSQL 5432 和 Redis 6379 `P0`
- [x] 设置 `SESSION_COOKIE_SECURE=true` `P0`
- [x] 设置准确的 `SESSION_COOKIE_TRUSTED_URL` 和 `TRUSTED_PROXIES` `P0`

### 2.4 已有日志与生产监控

- [x] `【已有】` 项目支持 API 请求与消费日志
- [x] `【已有】` 项目支持 Seedance 任务、扣费、差额结算和退款日志
- [x] `【已有】` 项目支持用户、模型、渠道、Token、额度和错误信息查询
- [x] `【配置】` 应用容器统一使用 `json-file` 日志轮转，单文件 20 MB、保留 5 个文件 `P0`
- [x] `【验证】` 确认生产日志不会记录完整用户 Token 和上游密钥 `P0`（8-20 生产日志抽检通过：应用日志 27 个文件与 DB logs 表对渠道 key `sk-YVBkmwm...`、用户 token `AvAy5DhVFr...`、Authorization 头均为 0 命中；logs 表无 request/response body 列；真实请求 200+403 复查新日志同样零命中）
  - 8-20 已执行生产日志抽检：以线上真实渠道 key 与用户 token 特征串在 `/app/logs/oneapi-*.log`（27 个文件）与 DB `logs` 表（content/other 列）grep 均为 0 命中；logs 表无 request/response body 列；触发真实 `GET /v1/models`(200) 与 `POST /v1/video/generations`(403 预扣失败零成本) 后复查新日志零命中；GIN 日志仅含 request_id/method/path/status/ip，不含 Authorization 头。结论：完整用户 Token 与上游密钥不会落盘。
- [x] `【外部】` 监控健康接口 `/api/status`（Uptime Kuma 已启用）`P0`
- [ ] `【外部】` 监控 CPU、内存、磁盘、容器重启和网络 `P0`
- [ ] `【外部】` 对 5xx 激增、渠道失败、上游余额不足和异常成本发送告警 `P0`
- [ ] `【外部】` 监控 PostgreSQL 连接数、容量和备份状态 `P1`
- [ ] `【外部】` 监控 Redis 内存、连接数和驱逐情况 `P1`

## 3. 第三阶段：生产验收和小范围灰度

部署完成不代表已经正式上线。本阶段通过后才开放注册或付费。

### 3.1 Seedance 验收

- [x] `【已有】` 支持火山方舟视频任务创建与状态查询
- [x] `【已有】` 保存公开任务 ID、上游任务 ID、用户、渠道、预扣额度和计费快照
- [x] `【已有】` 后台轮询支持排队、处理中、成功、失败和超时
- [x] `【已有】` 支持预扣费、完成后差额结算和失败退款
- [x] 使用测试与生产方舟渠道分别跑通 5 秒、480P、16:9 文生视频 `P0`
- [ ] 使用生产方舟渠道跑通图生视频 `P0`
- [x] 验证创建任务、排队、处理中和成功状态 `P0`
- [ ] `【自动化已通过，生产待验收】` 使用真实方舟验证上游拒绝、失败和超时状态；本地故障注入已覆盖 401、429、500/503、响应超时和余额不足 `P0`
- [x] 核对公开模型名、用户组、渠道、480P 分辨率和无音频选项在任务记录中的实际结果 `P0`
- [ ] 核对视频输入、其他分辨率和音频选项的实际账单 `P0`
- [ ] 使用真实小额任务完成三方对账：系统任务日志与用户扣费已吻合，方舟账单明细仍待核对 `P0`
- [x] 已通过自动化回归测试构造“余额只够预扣、不够完成后差额”的任务，确认系统不会产生负余额、不会静默承担未支付成本，并能留下可处理的欠款/冻结/告警记录 `P0`
- [ ] 验证失败与超时任务只退款一次 `P0`
- [ ] 验证重复请求不会重复创建任务或重复扣费 `P0`
- [x] `【自动化验证】` 验证方舟 429 的兜底处理：本地名额及时释放、预扣额度正确退款、任务不被自动重复创建，并记录可串联错误；真实方舟限流演练仍并入生产验收 `P0`
- [x] 验证成功视频可以稳定下载（8-19 实测 20s 1080p 45,935,221 字节完整下载，60s 超时 bug 已修复为 600s）；CDN 转存仍待做 `P1`

### 3.2 账号、Token 与限流

- [x] `【已有】` 已初始化管理员账号
- [ ] `【已有，未启用】` 为管理员启用二步验证或 Passkey `P0`
- [ ] `【已有，待配置】` 配置邮箱验证和密码重置邮件 `P0（开放注册时）`
- [ ] `【已有，待配置】` 普通用户 Token 默认设置有效期、额度或模型范围 `P0`
- [ ] `【已有，待验证】` 验证 Token 禁用、删除、过期和额度耗尽（删除 404 路由缺陷已修复上线 `361256f8`；完整禁用/过期场景仍待补）`P0`
- [ ] `【部分已有，需二开】` 将用户/分组请求速率限制接入 Seedance 视频路由 `P0`
- [x] 配置 IP、注册、登录和敏感接口限流（登录 429 已定位并修复，上线 `RATE_LIMIT_IP_WHITELIST=153.37.135.52,103.151.173.202`，容器重启后生效验证通过）`P0`
- [x] 对测试用户执行并发与限流测试（8-19 250 并发 20s 压测：QPS 546、p99 2380ms、500 归零；发现并修复 PG 连接池瓶颈 `SQL_MAX_OPEN_CONNS:80`）`P0`

### 3.3 灰度发布

- [x] 先只允许管理员和少量测试账号访问（注册与密码注册关闭；现有 admin + zhongqiyilian 两个账号）`P0`
- [x] 完成一次“注册 → 创建 Token → 创建视频 → 查询状态 → 下载视频”的全流程（8-19 线上实测：提交 200 → queued→IN_PROGRESS→SUCCESS → 鉴权取流 200，产物 seedance_test_20260819.mp4）`P0`
- [ ] 连续运行至少 24 小时，确认没有异常重启和持续错误 `P1`
- [x] 检查数据库备份确实生成；当前已保留 8 份压缩 SQL 备份 `P0`
- [ ] 使用最新生产备份完成隔离恢复演练 `P0`
- [ ] 检查 HTTPS、Session、真实客户端 IP 和任务查询 `P0`
- [x] 准备一条命令恢复上一个应用镜像（deploy-app.ps1 内置 rollback：失败自动回滚 compose + 45 次健康轮询）`P0`
- [x] 记录当前数据库备份和应用版本（8-20 最新：`newapi-20260820T014850Z.sql.gz` / `new-api-central-asia:9d7a0b94`）`P0`

## 4. 第四阶段：正式开放后的持续维护

下面事项不阻塞第一次部署，但需要长期执行。

- [ ] 每日检查应用存活、5xx、渠道错误和异常成本
- [ ] 每日确认数据库备份任务成功
- [ ] 定期执行真实恢复演练
- [ ] 定期核对方舟价格、模型版本和下线公告
- [ ] 定期检查 Endpoint 状态、余额和限流情况
- [ ] 每次发布记录版本、变更内容和数据库变化
- [ ] 发布前备份数据库并保留上一版本镜像
- [ ] 先更新测试环境，再更新生产环境
- [ ] 发布后检查健康接口、登录、视频创建、任务查询和日志
- [ ] 定期更新依赖并执行前后端回归测试
- [ ] 收费后每日核对充值、用户扣费、上游账单和差额

## 5. 上线信息确认表

| 项目 | 当前内容 |
|---|---|
| 使用范围 | 面向海外用户的公网服务 |
| 首批目标国家或地区 | 中亚地区；具体首发国家名单待灰度计划确认 |
| 服务器系统 | Ubuntu 24.04.4 LTS |
| CPU / 内存 / 磁盘 | 2 vCPU / 7.8 GiB / 96 GB |
| 部署区域 | 面向中亚选址；根据中亚用户访问延迟与到方舟 Endpoint 的实测延迟确定具体 Region |
| 正式域名 | `https://globalaiclient.com`；`https://www.globalaiclient.com` |
| 固定部署源码版本 | 当前生产 Commit `579f677b`；首个部署候选基线标签为 `deploy-central-asia-20260817-rc1` |
| 生产镜像 | `new-api-central-asia:579f677b`；`sha256:4c6e72794fe4e430d8b6896be26baff9ac3384a13e348204787552f5c07222e9` |
| 首期模型 | Seedance 2.0；Seedance 2.0 Fast 按需 |
| 对外模型名 | `doubao-seedance-2.0` |
| 上游模型版本 | `doubao-seedance-2-0-260128` |
| 上游供应商 | 火山方舟（Volcengine Ark） |
| 测试渠道 | `seedance-test` / `test` 组 / 独立 Endpoint 与 API Key |
| 生产渠道 | `seedance-prod` / `prod` 组 / 独立 Endpoint 与 API Key |
| 真实任务验证 | 测试、生产各 1 个 5 秒 480P 文生视频任务成功 |
| 单次验证结算 | 50638 tokens，New API 配额 159545，约 `$0.3191` / `¥2.33` |
| 预计用户数 | 待确定 |
| 峰值并发 | 待确定 |
| 是否开放注册 | 已关闭注册和密码注册，仅管理员和受限测试账号访问 |
| 是否收费 | 首发阶段不启用在线支付；测试额度由管理员发放 |
| 邮件服务 | 待确定 |
| 支付方式 | 暂不启用；所有在线支付网关保持未配置和关闭状态 |
| 监控告警渠道 | 待确定 |
| 备份保存位置 | VPS 本机 `/opt/new-api/backups`；异机备份待配置 |
| 计划灰度日期 | 待确定 |
| 计划正式开放日期 | 待确定 |

### 5.1 2026-08-12 真实验证记录

| 环境 | 用户组 | 渠道 | 请求模型 | 规格 | 结果 |
|---|---|---|---|---|---|
| 测试 | `test` | `seedance-test` | `doubao-seedance-2.0` | 5 秒 / 480P / 16:9 | `SUCCESS` |
| 生产 | `prod` | `seedance-prod` | `doubao-seedance-2.0` | 5 秒 / 480P / 16:9 / 无音频 | `SUCCESS` |

### 5.2 2026-08-18 生产部署记录

| 项目 | 验证结果 |
|---|---|
| 站点品牌 | `JadeRoute`；Logo 为 `/brand/jaderoute-mark.svg` |
| 首页定位 | 中文页面统一使用“主流大模型”表述，不再使用“国产大模型”定位 |
| 生产服务 | Caddy、New API、PostgreSQL 16、Redis 7 均运行；New API 容器健康 |
| HTTP 验证 | 主域名、`www` 域名、文档页及品牌 SVG/PNG 均返回 200 |
| 生产配置 | `ServerAddress=https://globalaiclient.com`，注册和密码注册关闭，迁移会话已清空 |
| 数据迁移 | `users=2`、`channels=2`、`tokens=2`、`tasks=2`、`logs=54` |
| 发布前备份 | `/opt/new-api/backups/newapi-20260818T023223Z.sql.gz` |
| 应用版本 | Git Commit `579f677b`，镜像 `new-api-central-asia:579f677b` |
| 发布验证 | 首页 200，`/api/status` 返回 `system_name=JadeRoute` 与新 Logo，最近日志未发现 `ERR`、`panic` 或 `fatal` |

## 6. 正式开放门禁

只有以下项目全部完成后才开放公网用户：

- [ ] 所有 `P0` 项目已完成
- [x] 生产 Compose、域名、HTTPS、Session 和代理配置正确
- [x] PostgreSQL 与 Redis 未暴露公网
- [x] 数据库自动备份成功（每日 03:15 UTC cron，8 份压缩备份保留）；恢复演练仍待执行
- [ ] 管理员启用强密码和二步验证
- [x] Seedance 差额补扣已具备余额下限守卫，负余额、欠款清偿和并发冻结回归测试通过（`-race` 因本机缺少 `gcc` 未执行）
- [x] 未鉴权的非首发模型资源路由（包括 `/mj/image/:id`）已禁用；Seedance 资源入口已鉴权并校验任务所有者，匿名访问和跨用户访问均被拒绝，且不向客户端返回可绕过本地授权的上游直链
- [x] 方舟生产渠道与模型映射已通过真实 480P 文生视频任务验证
- [ ] 备用 Endpoint / 渠道策略已配置并验证
- [x] Seedance 的创建、轮询、结算和失败退款已验证（8-19 线上全链路实测 + copyright_restriction 失败全额退款 5,328,747 quota）
- [x] 限流已启用（RATE_LIMIT_IP_WHITELIST）；客户端幂等键与全站成本熔断代码已上线并测试通过
- [ ] 日志中没有完整 Token、密码或上游密钥
- [ ] 小范围灰度验收通过
- [ ] 发布和回滚流程经过演练
- [ ] ⚠️ 页面署名已按运营决策移除（footer/About attribution，2026-08-19/20 两次提交）；镜像内保留 LICENSE/NOTICE/THIRD-PARTY-LICENSES。AGPL 对外分发存在合规风险，建议保留仓库级署名
- [ ] 已确认火山方舟账号允许当前调用方式
- [x] 在线支付保持关闭（8-20 查库：options 无 stripe/creem/waffo 凭据，`WaffoEnabled=false`；合规条款已确认 `payment_setting.compliance_confirmed=true`）；`/api/user/topup/info` 需认证，接口验证待正式开放前
- [ ] 如启用支付网关，充值下单上限校验与超限提示已经上线并验证
