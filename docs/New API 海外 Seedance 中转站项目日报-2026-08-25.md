# New API 海外 Seedance 中转站项目日报

日期：2026-08-25

> 说明：本日报根据 2026-08-24 已完成的代码修复与检查结果预先整理，提交前请根据实际提交、部署和 Redis 回归情况调整结论性表述。

## 一、今日总结

- 修复用户主动注销路径的 API Key 数据残留问题：在 `User.Delete()` 的数据库事务内，按 `user_id` 对 `tokens` 执行物理删除，并与用户软删除保持原子性，避免继续产生孤儿 Token。
- 补充注销清理回归测试，覆盖注销用户 Token 被物理删除、其他用户 Token 不受影响、Token 清理失败时事务整体回滚，以及管理员硬删除行为保持不变。
- 完成改动范围代码检查，确认事务边界和删除范围符合预期；`go test ./model ./controller`、`go vet ./model ./controller` 和 `git diff --check` 均通过，未发现阻断性问题。
- 当前新增测试文件 `model/user_delete_tokens_test.go` 尚未纳入 Git 跟踪；现有测试运行时关闭 Redis，注销后的 Redis Token 缓存清理尚缺少实际自动化覆盖。
- 修复 Seedance 任务日志 Tokens 列显示 "—" 的问题（token 消耗不显示）：根因为异步任务日志从不落库 token——上游 `usage.total_tokens` 已被 doubao adaptor 解析并被结算消费，但 `LogTaskConsumption` / `RecordTaskBillingLog` 均未写入，前端在 prompt/completion 均为 0 时渲染 "-"。改动：`RecordTaskBillingLog` 写 `other.total_tokens`（>0 才写，other 懒初始化防 "null"）；`SettleSeedanceTaskBilling` / `RecalculateTaskQuota(ByTokens)` 透传 totalTokens；前端 Tokens 列 fallback 显示 `other.total_tokens` 并标注"任务总 Token"，同步 zh/zh-TW/en locale。commit **4b6297c0**（本地未 push，10 files +211/-26）。
- Seedance token 回显修复验证：`go build ./...`、`go vet ./service/ ./model/`、`go test ./service/ ./model/` 全通过；前端 tsgo typecheck ✅、oxlint 改动文件 0 错 ✅、bun test 179 pass / 3 fail（3 fail 为 api-key-group-cell Auto 环动画既有失败，与本次零依赖）；rsbuild 生产 build ✅（build 前已 mv 旧 dist 规避 safe-delete 挂起）。新增 4 个回归测试（seedance_settle_test.go）：补扣带 token、退款带 token、通用 token 重算透传、无 usage 不伪造字段。
- 已知限制（如实标注）：delta=0（预扣准确）不写调整日志 → 该条任务 Tokens 列仍为 "-"，完整闭环需方案 3（结算时 UPDATE 回填提交日志）；失败退款日志无 usage 语义上无 token 可写；存量历史日志不受影响。
- Seedance token 回显修复（4b6297c0）已于 08-24 晚间部署至生产（deploy-app.ps1 / compose.prod.yml）；部署脚本已修复 PowerShell 5.1 stderr 误判问题（scp/ssh 输出 2>&1 合并），本次部署未再中断。部署结果（健康检查 / smoke / 镜像版本）以实际验证为准，提交前请更新本条目。

## 二、明日计划

- 将 `model/user_delete_tokens_test.go` 纳入版本控制，复核提交范围，避免混入当前工作区中的无关文档和前端报告文件。
- 使用 miniredis 补充用户注销缓存回归测试：预热用户与 Token 缓存后执行 `User.Delete()`，验证 Token 缓存、用户缓存和认证版本墓碑状态符合预期。
- 完成新增缓存用例后重新运行 `go test ./model ./controller`、`go vet ./model ./controller` 和差异检查，并根据验证结果决定后续提交与部署安排。
- 清理 `web/dist-stale-20260824` / `web/dist-stale-20260824-2` 两个旧构建目录（各约 58MB，可手动删除）。

