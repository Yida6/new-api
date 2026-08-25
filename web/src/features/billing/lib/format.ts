/*
Copyright (C) 2023-2026 QuantumNous

This program is free software: you can redistribute it and/or modify
it under the terms of the GNU Affero General Public License as
published by the Free Software Foundation, either version 3 of the
License, or (at your option) any later version.

This program is distributed in the hope that it will be useful,
but WITHOUT ANY WARRANTY; without even the implied warranty of
MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
GNU Affero General Public License for more details.

You should have received a copy of the GNU Affero General Public License
along with this program. If not, see <https://www.gnu.org/licenses/>.

For commercial licensing, please contact support@quantumnous.com
*/
/**
 * Formatting and mapping helpers for the billing page.
 */
import type { StatusVariant } from '@/components/status-badge'
import type { UsageLog } from '@/features/usage-logs/data/schema'
// 直接引用具体模块而非 barrel，避免把整个 usage-logs 特性（含 React 组件）拖入
// 仅做纯逻辑聚合的 billing 工具链，也便于单元测试。
import { parseLogOther } from '@/features/usage-logs/lib/format'
import { formatTimestampToDate } from '@/lib/format'

import { BILLING_LOG_TYPES } from '../types'

// ============================================================================
// Record Classification
// ============================================================================

/** Whether a log record is bill-related (consume / error / refund). */
export function isBillableLog(log: UsageLog): boolean {
  return (
    log.type === BILLING_LOG_TYPES.CONSUME ||
    log.type === BILLING_LOG_TYPES.ERROR ||
    log.type === BILLING_LOG_TYPES.REFUND
  )
}

/**
 * Whether a log record represents a real user request that should count
 * towards the "Total Requests" stat.
 *
 * Both successful consumes (type=2) and failed requests (type=5) are real
 * requests. Refund records (type=6) and task billing adjustments are not new
 * requests: a single async task is counted once by its submission consume log,
 * then any 差额补扣/退款 are quota adjustments for the same task. Adjustments
 * carry `pre_consumed_quota` (and a `task_id`) in `other`.
 */
export function isRealRequest(log: UsageLog): boolean {
  if (
    log.type !== BILLING_LOG_TYPES.CONSUME &&
    log.type !== BILLING_LOG_TYPES.ERROR
  ) {
    return false
  }
  // 差额补扣/计费调整记录不是一次新请求（仅消费日志存在该标记）。
  if (log.type === BILLING_LOG_TYPES.CONSUME) {
    const other = parseLogOther(log.other)
    if (other && other.pre_consumed_quota !== undefined) return false
  }
  return true
}

export interface BillingStatusInfo {
  /** i18n key rendered as the badge label */
  labelKey: string
  /** StatusBadge variant matching the backend log type semantics */
  variant: StatusVariant
}

/**
 * Map a log record to its billing status badge.
 * type=2 → success (green), type=5 → failed (pink), type=6 → refund (blue).
 */
export function getBillingStatus(log: UsageLog): BillingStatusInfo {
  switch (log.type) {
    case BILLING_LOG_TYPES.ERROR:
      return { labelKey: 'Failed', variant: 'danger' }
    case BILLING_LOG_TYPES.REFUND:
      return { labelKey: 'Refund', variant: 'info' }
    case BILLING_LOG_TYPES.CONSUME:
    default:
      return { labelKey: 'Success', variant: 'success' }
  }
}

/**
 * Derive the billing-type tag label from the log's `other` metadata.
 * Falls back to per-request billing when no marker is present.
 */
export function getBillingTypeLabel(log: UsageLog): {
  labelKey: string
  useFallback: boolean
} {
  const other = parseLogOther(log.other)
  if (other?.billing_source === 'subscription') {
    return { labelKey: 'Subscription Charge', useFallback: false }
  }
  // 任务计费：提交日志带 is_task；任务退款/差额调整记录带 task_id 且常含
  // pre_consumed_quota（但不带 is_task），同样归属任务计费。
  if (
    other?.is_task ||
    other?.task_id ||
    other?.pre_consumed_quota !== undefined
  ) {
    return { labelKey: 'Task Billing', useFallback: false }
  }
  return { labelKey: 'Per-request Billing', useFallback: true }
}

// ============================================================================
// Aggregation
// ============================================================================

/**
 * Build a map of `task_id → total_tokens` from the billable log set.
 *
 * For async tasks (Seedance etc.) the upstream total token count is recorded
 * on the settlement/adjustment log (退款日志 or 补扣日志) — NOT on the
 * submission consume log. Both record-task-billing code paths in
 * service/task_billing.go call `RecordTaskBillingLog` with `TotalTokens`,
 * which only fires on settlement; the submission consume log keeps its
 * prompt/completion tokens at 0 and `other` has no `total_tokens`.
 *
 * To make the token attributable to the task itself (and not double-counted
 * across an optional 补扣 + 退款 pair), we deduplicate by `task_id` and keep
 * the first seen value.
 */
export function taskTotalTokensByTaskId(logs: UsageLog[]): Map<string, number> {
  const map = new Map<string, number>()
  for (const log of logs) {
    const other = parseLogOther(log.other)
    if (!other?.task_id || !other.total_tokens) continue
    if (!map.has(other.task_id)) {
      map.set(other.task_id, other.total_tokens)
    }
  }
  return map
}

/**
 * Total tokens across the whole log set.
 *
 * Async task totals (other.total_tokens) are the authoritative token source for
 * a task and can live on a settlement/adjustment log (退款/补扣) that does not
 * itself represent a new request — and, for historical logs, may have no
 * matching submission log with the same task_id in the set at all. So we do NOT
 * require finding a submission log: we collect every `task_id → total_tokens`
 * across all billable logs, deduplicate by task_id, and count each task once.
 *
 * Algorithm:
 *  1. Real request logs (sync consume / error): sum prompt + completion.
 *     If a real request log also carries a task_id that is covered by the task
 *     map (e.g. a submission log that was backfilled with total_tokens), skip
 *     it here — its full task total is counted once via the map, so it never
 *     double counts via the ordinary token fields.
 *  2. Async task totals: each entry in the deduplicated task map is summed
 *     exactly once, even when both a 补扣 and a 退款 log carry the same
 *     task_id + total_tokens.
 */
export function sumTokens(logs: UsageLog[]): number {
  const taskTokens = taskTotalTokensByTaskId(logs)

  let total = 0

  for (const log of logs) {
    if (!isRealRequest(log)) continue
    const other = parseLogOther(log.other)
    // A submission log whose task total is already accounted for by the map
    // (backfilled total_tokens) must not also contribute its token fields.
    if (other?.task_id && taskTokens.has(other.task_id)) continue
    total += (log.prompt_tokens || 0) + (log.completion_tokens || 0)
  }

  for (const value of taskTokens.values()) {
    total += value
  }

  return total
}

// ============================================================================
// Per-row token resolution (table display)
// ============================================================================

export interface RowTokenInfo {
  /** Effective token count to render for the row. */
  value: number
  /** Whether the value is an async task's single upstream total (no in/out split). */
  isTaskTotal: boolean
}

/**
 * Collect the task_ids of actual async task submissions (is_task=true and not
 * a 差额补扣 adjustment). A submission row that matches one of these ids is the
 * canonical place to render that task's total tokens, so sibling adjustment
 * rows (退款/补扣) for the same task_id do not repeat the number.
 */
export function submissionTaskIds(logs: UsageLog[]): Set<string> {
  const ids = new Set<string>()
  for (const log of logs) {
    const other = parseLogOther(log.other)
    if (other?.is_task && !other.pre_consumed_quota && other.task_id) {
      ids.add(other.task_id)
    }
  }
  return ids
}

/**
 * Resolve the token count shown for a single billing row.
 *
 * Order of precedence:
 *  - prompt/completion are non-zero → their sum (sync requests).
 *  - A submission row whose task_id hits the token map → the task total.
 *  - An adjustment row (退款/补扣) that carries `total_tokens` and whose
 *    task_id has no matching submission row in the set → the task total
 *    (historical logs where the submission lost its task_id or is filtered out).
 *  - A matching submission exists → adjustment row renders 0 (no repeat).
 */
export function rowTokenInfo(
  log: UsageLog,
  taskTokens: Map<string, number>,
  submissionIds: Set<string>
): RowTokenInfo {
  const input = log.prompt_tokens || 0
  const output = log.completion_tokens || 0
  if (input > 0 || output > 0) {
    return { value: input + output, isTaskTotal: false }
  }
  const other = parseLogOther(log.other)
  const taskId = other?.task_id
  const isSubmissionRow = !!other?.is_task && !other?.pre_consumed_quota
  // 提交行：is_task 且非补扣调整，其 task_id 命中任务 map → 显示任务总 token。
  if (taskId && isSubmissionRow && taskTokens.has(taskId)) {
    return { value: taskTokens.get(taskId) ?? 0, isTaskTotal: true }
  }
  // 调整行（退款/补扣）：携带 total_tokens 且集合内没有可匹配的提交行 →
  // 直接显示该任务的 total（兼容历史日志：提交行无 task_id 或已不在当前集合）。
  if (taskId && other?.total_tokens && !submissionIds.has(taskId)) {
    return { value: other.total_tokens, isTaskTotal: true }
  }
  return { value: 0, isTaskTotal: false }
}

// ============================================================================
// Formatting (re-exports with billing-page defaults)
// ============================================================================

/** Format a unix-seconds timestamp as YYYY-MM-DD HH:mm:ss. */
export function formatBillingTime(ts?: number): string {
  return formatTimestampToDate(ts, 'seconds')
}

/** Format token counts with thousands separators. */
export function formatTokenCount(n: number): string {
  return (n || 0).toLocaleString('en-US')
}
