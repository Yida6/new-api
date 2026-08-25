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
 * Type definitions for the consumer billing page.
 *
 * The page shows the current user's consumption records (consume / error /
 * refund log types only) with aggregated statistics. Data is fetched through
 * the same `/api/log/self*` endpoints used by the usage-logs feature.
 */
import type { UsageLog } from '@/features/usage-logs/data/schema'

// ============================================================================
// Log Type Constants
// ============================================================================
// Backend Log type enum (model/log.go). Only these three are bill-related
// and rendered on the billing page.
export const BILLING_LOG_TYPES = {
  CONSUME: 2, // 成功消费
  ERROR: 5, // 失败请求
  REFUND: 6, // 退款
} as const

/** Log types treated as billable records. */
export const BILLABLE_LOG_TYPES: readonly number[] = [
  BILLING_LOG_TYPES.CONSUME,
  BILLING_LOG_TYPES.ERROR,
  BILLING_LOG_TYPES.REFUND,
]

// ============================================================================
// Filters
// ============================================================================

export interface BillingFilters {
  /** Substring match on model_name */
  model: string
  /** Substring match on request_id */
  requestId: string
  /** Unix seconds; 0 = unbounded */
  startTs: number
  /** Unix seconds; 0 = unbounded */
  endTs: number
}

export const EMPTY_BILLING_FILTERS: BillingFilters = {
  model: '',
  requestId: '',
  startTs: 0,
  endTs: 0,
}

/** Quick time-range presets for the filter bar. days=0 means "all time". */
export const BILLING_TIME_PRESETS = [
  { days: 0, labelKey: 'All Time' },
  { days: 1, labelKey: '24 Hours' },
  { days: 7, labelKey: '7 Days' },
  { days: 14, labelKey: '14 Days' },
  { days: 30, labelKey: '30 Days' },
] as const

// ============================================================================
// Pagination
// ============================================================================

/** Max rows fetched to the client for aggregation & client-side pagination. */
export const BILLING_MAX_ROWS = 5000

/** Rows fetched per /api/log/self request. */
export const BILLING_FETCH_BATCH = 200

export const BILLING_PAGE_SIZES = [10, 20, 50] as const

// ============================================================================
// Derived Types
// ============================================================================

export interface BillingStats {
  /** Net spend (consume - refund), server-aggregated via /self/stat */
  netQuota: number
  /** Number of real requests in the filtered dataset (excludes adjustments) */
  requestCount: number
  /**
   * Total tokens across real requests. For sync requests this is
   * prompt_tokens + completion_tokens; for async tasks (Seedance etc.) it is
   * their single upstream total (other.total_tokens), since those have no
   * input/output split.
   */
  totalTokens: number
}

export type BillingLog = UsageLog
