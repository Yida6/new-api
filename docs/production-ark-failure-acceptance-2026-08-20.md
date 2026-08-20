# 火山方舟真实异常场景生产验收（2026-08-20）

## 验收范围

- 仅使用线上 `test` 用户组及测试渠道（数据库渠道 ID `4`，名称“测试”）。
- 测试 Token 禁止跨组重试；提交时 `test` 组只有一个启用渠道，因此不会路由到其他渠道。
- 真实请求限定为一次 5 秒、480P、无音频的 `doubao-seedance-2.0` 任务。
- 不修改生产渠道，不消耗余额制造“上游余额不足”，不通过高并发制造 429，不人为制造方舟 500/503。

## 真实方舟失败验收

| 项目 | 结果 |
|---|---|
| 提交时间 | 2026-08-20 04:27:18 UTC（12:27:18 Asia/Shanghai） |
| 路由 | `group=test`，`channel_id=4` |
| 任务 | `task_ia4y...KdpB`（报告中脱敏） |
| 提交结果 | HTTP 200，进入 `queued`，随后进入 `running` |
| 最终结果 | `FAILURE` |
| 上游错误 | `OutputVideoSensitiveContentDetected.PolicyViolation`，输出视频可能涉及版权限制 |
| 完成时间 | 2026-08-20 04:29:09 UTC（12:29:09 Asia/Shanghai） |
| 上游 POST 次数 | 1；数据库中该公开任务 ID 仅有一条任务记录 |
| 预扣 | 433,851 quota |
| 退款 | 433,851 quota |
| 用户余额 | 测试前后均为 498,617,302，差额为 0 |
| 最终任务额度 | 0 |
| 并发名额 | 已释放（`concurrency_released=true`） |
| 待补偿差额 | 0（`token_delta_pending=0`） |
| 测试渠道状态 | 保持启用 |

结论：真实方舟版权拒绝能够被后台轮询识别为失败；预扣只发生一次，退款额度与预扣一致，没有重复扣费或重复退款，测试渠道未被错误禁用。

## 日志脱敏

- 本次测试后的数据库 `logs.content` / `logs.other` 中，测试渠道完整密钥命中数为 0。
- 测试 Token 完整值命中数为 0。
- `Authorization` 命中数为 0。
- 应用 `/app/logs` 中，测试渠道完整密钥和测试 Token 完整值命中数均为 0。

## 自动化回归

2026-08-20 重新执行：

```text
go test ./relay -run "TestClassifyErrorKind" -count=1
go test ./controller -run "TestRelayTask(Upstream401|Upstream429|Upstream5xx|UpstreamTimeout|UpstreamBalanceInsufficient|LocalBalanceInsufficient|UpstreamSuccessControl|FailureLogCorrelation)$" -count=1
```

两个测试包均通过，覆盖 401、429、500/503、响应超时、上游余额不足、本地余额不足、成功对照组和失败日志串联。

退款幂等补充回归也已通过：

```text
go test ./service -run "Test(UpdateSunoTasksStalePollsRefundExactlyOnce|RunTaskPollingOnceDoesNotRefundHistoricalFailedTask|SweepTimedOutTasksHonorsRefundRolloutBoundary|TaskBilling_Idempotent_RepeatSettleAndRefund|CASGuardedRefund_Win|CASGuardedRefund_Lose)$" -count=1
go test ./controller -run "TestRelayTaskUpstreamTimeout$" -count=1
```

## 失败与超时任务退款验收

- 首个真实版权失败任务只有一条预扣日志和一条等额退款日志，均为 433,851 quota。
- 重放后产生的第二个真实版权失败任务同样只有一条预扣日志和一条等额退款日志，均为 433,851 quota。
- 持久幂等修复后的真实验收任务仍只有一条 433,851 预扣和一条等额退款；原样重放没有新增任务或计费日志。
- 三个任务最终 `quota=0`、`concurrency_released=true`、`token_delta_pending=0`。
- 第二个任务退款完成后，用户余额再次恢复为 498,617,302。
- 修复后任务退款完成，用户余额也恢复为 498,617,302。
- 真实失败任务“每个任务只退款一次”通过；超时任务的单次退款由自动化故障注入、重复结算和历史失败任务回归覆盖。真实超时未执行。

## 重复请求首次真实验收失败（修复前）

2026-08-20 04:35:51 UTC，使用与首个任务完全相同的请求体和客户端 `Idempotency-Key` 再次提交：

| 项目 | 重放前 | 重放后 |
|---|---:|---:|
| 测试渠道任务数 | 4 | 5 |
| 用户余额 | 498,617,302 | 498,183,451（再次预扣 433,851） |

- 服务返回新的公开任务 ID `task_WTwg...Z6U1`，方舟也返回新的上游任务 ID，证明发生了第二次真实创建。
- 第二个任务随后同样因版权限制失败，并于 04:38:39 UTC 全额退款；最终余额恢复为 498,617,302。
- 退款成功不改变幂等验收结论：如果重复任务成功，上游可能产生第二笔费用和第二份产物。

代码核对确认根因：

- 视频创建入口不读取客户端传入的 `Idempotency-Key`。
- 每个新的 HTTP 请求都会生成新的内部 UUID，仅用于单次请求内重试、日志和审计。
- 当前提交锁按“用户 + Token + 请求体摘要”防重，但任务完成后只保留 2 秒宽限期。
- 因此它只能吸收并发双击或极短延迟重试，不能提供客户端持久幂等。

结论：该次验收确认旧版本不具备客户端持久幂等，随后已完成二开与重新验收。

## 客户端持久幂等修复与重新验收

修复提交 `e1b5431c` 新增持久幂等映射，在访问上游、预扣额度和预留并发名额之前，按“用户 + Token + `Idempotency-Key`”原子占位：

- 同键、同请求体返回首次提交的公开任务，不再选择渠道、预扣、占用并发或调用上游。
- 同键、不同请求体返回 HTTP 409 `idempotency_key_conflict`。
- 只保存幂等键的 SHA-256 作用域摘要，不保存客户端原始 key。
- 上游任务已存在或结果未知时保留占位，禁止自动重放；上游明确未创建任务时才释放占位。

本地回归结果：

```text
go test ./controller -run '^TestRelayTaskClientIdempotencyPersistentReplay$' -count=10
go test ./model ./service ./relay ./controller -count=1
go build ./...
GOWORK=off go build ./...  # relaykit 目录
```

均通过。生产版本 `new-api-central-asia:e1b5431c` 于 2026-08-20 上线，发布前备份为 `newapi-20260820T045025Z.sql.gz`，容器健康检查和公网冒烟检查通过。

重新验收严格使用 `test` 组 Token；该 Token 禁止跨组重试，适配请求模型的启用渠道只有 ID `4`“测试”：

| 项目 | 首次提交 | 原样重放 |
|---|---|---|
| HTTP | 200 | 200 |
| 公开任务 | `task_333…gz3P` | `task_333…gz3P` |
| 响应头 | 无重放标记 | `Idempotency-Replayed: true` |
| 数据库任务数 | 1 | 仍为 1 |
| 命中渠道 | ID `4` | 未再次选择渠道 |
| 用户余额 | 498,183,451（预扣 433,851） | 仍为 498,183,451 |
| 幂等记录 | 1 条，`committed` | 仍为 1 条 |

该任务随后因版权限制进入 `FAILURE`：任务最终 `quota=0`、`token_delta_pending=0`，日志只有一条 433,851 预扣和一条等额退款，用户余额恢复为 498,617,302。

结论：`验证重复请求不会重复创建任务或重复扣费` 生产验收通过；真实失败任务也再次确认只退款一次。

## 未执行的真实故障

- 上游 401：在“只能使用现有测试渠道”的约束下，不能创建临时隔离渠道；修改现有测试渠道密钥还需要处理缓存、自动禁用和恢复，风险大于验收收益，因此未执行。
- 429：没有使用高并发冲击真实 Endpoint。
- 500/503：无法安全、稳定地要求方舟主动返回服务端错误。
- 响应超时：没有修改会影响线上请求的全局超时，也没有在请求结果可能未知时重复提交付费任务。
- 上游余额不足：没有通过耗尽真实余额制造故障。

以上场景保留自动化覆盖，真实生产证据等待自然故障或后续独立隔离环境补充。

## 方舟账单

- `arkcli` SSO 已重新认证。
- 账号未开通拆分账单能力，账单接口返回“当前账号未开通分账能力，无数据”。
- 火山账单为 T+1，测试当天不能取得该任务的最终结算明细。
- 因此本报告确认本地退款闭环通过，但不将方舟侧最终结算金额标记为已核对。

## 结论

真实上游拒绝/任务失败、每任务单次退款和客户端持久幂等均已通过生产验收；同键同请求不会再创建第二个方舟任务或再次扣费。真实超时和不可安全制造的上游故障仍保留自动化证据，等待自然故障或独立隔离环境补充，不影响本次幂等修复结论。
