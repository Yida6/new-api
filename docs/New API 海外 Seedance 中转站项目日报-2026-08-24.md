# New API 海外 Seedance 中转站项目日报

日期：2026-08-24

## 一、今日总结

- 为 `globalaiclient.com` 开通 Hostinger Starter Business Email Free Trial（免费 12 个月），创建并激活 `noreply@globalaiclient.com`；确认 MX、SPF、DKIM 和 DMARC 均正常，套餐当前到期日为 2027-08-24。
- 在 JadeRoute 生产后台接入 `smtp.hostinger.com:465`、SSL/TLS、完整邮箱账号及发件地址，完成管理员邮箱绑定验证码、密码重置邮件和未注册邮箱注册验证码的生产收件验证，未实际修改管理员密码。
- 开启生产环境的注册、密码注册和电子邮件验证，确认注册验证码与密码重置两条邮件链路可用。
- 更新上线清单，关闭“配置邮箱验证和密码重置邮件”P0 待办，并同步记录当前开放注册状态、Hostinger 邮件参数和端到端验收结果。
- 完成影响使用问题排查与修复：为管理员用户编辑抽屉增加最新请求保护和加载失败提示，避免旧响应覆盖当前用户表单；调整 Playground 附件提交清理时机，附件读取或提交失败时保留草稿并显示错误提示，同时补齐多语言文案。
- 新增 4 条回归测试并全部通过，前端类型检查、生产构建和改动范围 lint 通过；全量前端测试共 179 条通过，另有 3 条未修改的 API Key Auto 分组视觉测试失败，待后续单独排查。修复已提交为 `2a7ae25e` 并部署生产镜像 `new-api-central-asia:2a7ae25e`，发布前备份为 `newapi-20260824T025351Z.sql.gz`；容器健康、重启次数为 0，双域名首页、`/channels` 和 `/api/status` 均返回 HTTP 200，启动后未匹配到 `panic`、`fatal` 或 `error` 日志。
- 移除不适用于 JadeRoute 定制版本的“系统维护”入口、版本检查页面及相关状态读取逻辑，类型检查、改动文件 lint、格式检查和生产构建均通过；改动提交为 `dfec8b4a`，已部署镜像 `new-api-central-asia:dfec8b4a`，发布前备份为 `newapi-20260824T062344Z.sql.gz`，容器健康且重启次数为 0，双域名首页、`/channels` 和 `/api/status` 均返回 HTTP 200。
- 在 VPS 独立部署 Uptime Kuma `2.5.3`，使用 `/opt/uptime-kuma/data` 持久化数据并通过 Caddy 接入 `status.globalaiclient.com`；完成 DNS、HTTPS、公开状态页 `jaderoute` 和 JadeRoute 控制台面板对接，首页与 `/api/status` 两个监控项均正常，面板显示 24 小时可用率 `100.00%`。

## 二、明日计划

- 观察开放注册后的验证码发送成功率、垃圾邮件投递情况和异常注册请求，并核对 Hostinger 邮件发送限制；必要时评估启用邮箱别名限制、邮箱域限制或机器人保护。
- 启动小范围灰度用户注册与 Seedance 试用，核对注册、Token 创建、任务提交、结算、失败退款和日志链路。
- 为 Uptime Kuma 配置异常通知渠道和每日备份计划，观察首页与 API 监控的告警准确性，并确认备份文件可用。
- 继续推进生产数据库恢复演练、异机备份和备用渠道准备，补齐剩余上线门禁。
- 观察生产版本 `dfec8b4a` 的管理员用户编辑、Playground 附件提交和 Uptime Kuma 面板行为，检查前端异常、容器重启及应用错误日志，并单独排查 API Key Auto 分组的 3 条视觉测试失败。
