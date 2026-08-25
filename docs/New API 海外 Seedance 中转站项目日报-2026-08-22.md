# New API 海外 Seedance 中转站项目日报

日期：2026-08-22

## 一、今日总结

- 完成 Seedance 创建接口模型请求限流接入并部署 `60114903`：仅 `POST /v1/video/generations` 按 `TokenAuth → ModelRequestRateLimit → Distribute` 执行，任务轮询、播放及其他视频路由不计数；生产继续保持限流开关默认关闭，不改变既有请求与并发行为。
- 补充 Seedance 路由限流回归测试，覆盖 Redis 与内存模式、429 阈值、分组覆盖、用户隔离、GET 轮询不计数及关闭开关兼容；`go test ./router ./middleware`、`go vet ./router ./middleware`、新增测试 `-count=2` 和 `go build ./...` 均通过。
- 完成管理员 2FA 启用，并在生产验证 Token 禁用、删除、过期和额度耗尽场景均能按预期拒绝请求，关闭对应上线门禁项。
- 完成 HTTPS、真实客户端 IP 和任务查询生产 P0 验收：双域名 HTTPS、证书链、安全响应头及公网端口符合预期，伪造代理来源头不能改变真实客户端 IP；任务所有者查询、匿名访问、跨用户访问、随机任务 ID、重复查询无副作用及失败退款状态均完成核对。
- 在 Session 验收中定位 Caddy 终止 TLS 后 `request.TLS=nil` 导致同主机 HTTP Origin 被误放行的问题；完成 Secure 模式仅接受 HTTPS Origin 的最小修复，补充正式域名正负向矩阵、反向代理同源、转发头伪造和 Referer 回退测试，相关 Go 测试与构建通过。
- 提交并部署 Session Origin 修复 `c119cba2`，发布前生成备份 `/opt/new-api/backups/newapi-20260822T083416Z.sql.gz`；新镜像 `new-api-central-asia:c119cba2` 运行健康，RestartCount 为 0，未发生 OOM，双域名首页与 `/api/status` 均返回 HTTP 200。
- 完成 Session 生产复验：20 个负向 refresh/logout 用例均返回 `403 AUTH_ORIGIN_FORBIDDEN`，两个正式 HTTPS Origin 刷新成功，Cookie 安全属性和登出撤销行为无回归；验证窗口内无 5xx、日志未命中凭据，6 个测试会话已全部吊销。
- 更新上线清单，记录 Seedance 请求限流部署、管理员 2FA、Token 生命周期验证及 HTTPS/Session/真实客户端 IP/任务查询 P0 验收证据。

## 二、明日计划

- 启动 Seedance 内部白名单试用，保持开放注册和在线支付关闭，由管理员为首批内部用户配置受限账号、Token、模型权限与小额测试额度。
- 启用并验证 Seedance 模型请求速率限制，结合现有并发控制设置内部试用阈值，确认超限请求能够被正确拦截且不创建上游任务、不产生扣费。
- 跟踪内部试用首批真实任务，核对任务状态、方舟 Usage、用户扣费、失败退款和服务日志，及时记录异常并控制试用成本。
- 使用最新生产备份在隔离环境执行恢复演练，核对数据完整性、恢复步骤和耗时，并继续推进异机备份与备用渠道准备。
- 协调开通方舟拆分账单能力，待 T+1 财务数据可用后补充核对方舟 CNY 明细、系统任务日志和用户扣费。
