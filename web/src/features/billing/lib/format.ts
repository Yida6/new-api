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
  if (other?.is_task) {
    return { labelKey: 'Task Billing', useFallback: false }
  }
  return { labelKey: 'Per-request Billing', useFallback: true }
}

// ============================================================================
// Aggregation
// ============================================================================

export function sumInputTokens(logs: UsageLog[]): number {
  return logs.reduce((sum, log) => sum + (log.prompt_tokens || 0), 0)
}

export function sumOutputTokens(logs: UsageLog[]): number {
  return logs.reduce((sum, log) => sum + (log.completion_tokens || 0), 0)
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
