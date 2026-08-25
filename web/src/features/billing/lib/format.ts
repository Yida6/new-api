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
import { parseLogOther } from '@/features/usage-logs/lib'
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
 * Effective token contribution of a single billable log, as a single total.
 *
 * Aggregation rules:
 *  - Real request logs (consume / error) are the only source of tokens.
 *  - For sync requests: prompt_tokens + completion_tokens.
 *  - For async tasks: the submission consume log has prompt/completion = 0 and
 *    no `total_tokens`; its real token count lives on the settlement log. We
 *    resolve it through `taskTokens` (built from the same log set) by matching
 *    `other.task_id`. Adjustment logs (补扣/退款) themselves carry
 *    `pre_consumed_quota` and are excluded by `isRealRequest`, so they never
 *    contribute here — the token is attributed once via the submission log.
 *  - For 错误 (type=5) logs that happen to also be an async task, prefer the
 *    task total if known; otherwise fall back to prompt+completion.
 */
export function effectiveTokens(
  log: UsageLog,
  taskTokens?: Map<string, number>
): number {
  if (!isRealRequest(log)) return 0
  const input = log.prompt_tokens || 0
  const output = log.completion_tokens || 0
  if (input > 0 || output > 0) return input + output
  const other = parseLogOther(log.other)
  // Async-task submission consume log (is_task=true, no prompt/completion):
  // pull the settled total from the task id map.
  if (other?.task_id && taskTokens?.has(other.task_id)) {
    return taskTokens.get(other.task_id)!
  }
  // Fallback: an adjustment log that somehow still passes isRealRequest
  // (shouldn't happen, but be defensive).
  if (other?.total_tokens) return other.total_tokens
  return 0
}

export function sumTokens(logs: UsageLog[]): number {
  const taskTokens = taskTotalTokensByTaskId(logs)
  return logs.reduce((sum, log) => sum + effectiveTokens(log, taskTokens), 0)
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
