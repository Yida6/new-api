# New API 海外 Seedance 中转站项目日报

日期：2026-08-18

## 一、今日总结

- 完成 New API 生产环境部署，在 VPS `2.24.120.8` 上运行 Caddy、New API、PostgreSQL 16 和 Redis 7，并启用正式域名 `globalaiclient.com` 与 `www.globalaiclient.com` 的 HTTPS 访问。
- 完成本地开发数据库向生产 PostgreSQL 的迁移，生产数据核对结果为 `users=2`、`channels=2`、`tokens=2`、`tasks=2`、`logs=54`；同步关闭注册和密码注册，并清理迁移带入的用户会话。
- 修复前端 API 路径兼容问题并更新用户指南，完成首页内容回归测试、定向 lint、`bun run typecheck` 和生产构建验证。
- 完成 JadeRoute 品牌方向、SVG 标志和概念图整理，将站点名称、导航 Logo、favicon、生产数据库品牌配置及文档页标题和图标统一切换为 JadeRoute。
- 将首页“国产大模型”定位统一调整为“主流大模型”，保留标准 API 接入、智能路由、精细计费和可观测能力的产品表达；相关首页回归测试 3 项全部通过。
- 完成生产镜像 `new-api-central-asia:199d5217` 构建与滚动部署，镜像摘要为 `sha256:44e8398e04cbc77c44c87fbdbcd0e08681fa4cd5125c0b5e44be3bf2eac0a808`；应用容器健康，API 返回 200，最近日志未发现 `ERR`、`panic` 或 `fatal`。
- 将 `/docs/` 与 `/brand/*` 从 Go 二进制发布链路拆分为 Caddy 独立静态托管，新增文档生成、打包上传、原子切换、公网验证和失败回滚的一键脚本；迁移后快速发布耗时 13.7 秒，应用与 Caddy 容器均未重启。
- 配置每日 PostgreSQL 自动备份并确认备份文件生成，本次发布前备份为 `/opt/new-api/backups/newapi-20260818T023223Z.sql.gz`，上一版本镜像和部署文件继续保留用于回滚；同步更新开发与上线清单及文档发布说明，补充缓存策略、镜像版本和验证记录。

## 二、明日计划

- 使用最新生产备份在隔离环境执行一次 PostgreSQL 恢复演练，验证备份完整性和回滚流程，并规划异机备份保存位置。
- 在生产环境完成 Seedance 图生视频、相同幂等键重复提交、失败与超时退款、成本熔断及备用渠道验证，补齐正式开放前的 P0 验收项。
- 抽检生产数据库、应用日志和日志 API，确认完整用户 Token、方舟 Endpoint ID 与 API Key 均未落盘或返回客户端。
- 为管理员启用 Passkey 或 2FA，并补充外部健康监控、主机资源监控及 5xx、渠道失败和异常成本告警。
