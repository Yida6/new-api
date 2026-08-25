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
import {
  useCallback,
  useEffect,
  useMemo,
  useRef,
  useState,
} from 'react'
import { useTranslation } from 'react-i18next'

import { SectionPageLayout } from '@/components/layout'
import { getUserLogs, getUserLogStats } from '@/features/usage-logs/api'
import type { UsageLog } from '@/features/usage-logs/data/schema'
import { toast } from 'sonner'

import { BillingFilterBar } from './components/billing-filter-bar'
import { BillingStats } from './components/billing-stats'
import { BillingTable } from './components/billing-table'
import { isBillableLog, isRealRequest, sumTokens } from './lib/format'
import {
  BILLING_LOG_TYPES,
  BILLING_FETCH_BATCH,
  BILLING_MAX_ROWS,
  EMPTY_BILLING_FILTERS,
  type BillingFilters,
  type BillingStats as BillingStatsData,
} from './types'

function buildTimeRange(days: number): { startTs: number; endTs: number } {
  if (days <= 0) return { startTs: 0, endTs: 0 }
  const now = Date.now()
  return {
    startTs: Math.floor((now - days * 86_400_000) / 1000),
    endTs: Math.floor(now / 1000),
  }
}

export function Billing() {
  const { t } = useTranslation()
  const [filters, setFilters] = useState<BillingFilters>(EMPTY_BILLING_FILTERS)
  const [activePresetDays, setActivePresetDays] = useState(0)
  const [logs, setLogs] = useState<UsageLog[]>([])
  const [netQuota, setNetQuota] = useState<number>(0)
  const [loading, setLoading] = useState(true)
  const [truncated, setTruncated] = useState(false)
  // Guards against stale responses overwriting newer filter results when the
  // user rapidly switches filters / time ranges.
  const loadSeqRef = useRef(0)

  const load = useCallback(
    async (next: BillingFilters) => {
      const seq = ++loadSeqRef.current
      setLoading(true)
      setTruncated(false)
      try {
        const params = {
          model_name: next.model || undefined,
          request_id: next.requestId || undefined,
          start_timestamp: next.startTs || undefined,
          end_timestamp: next.endTs || undefined,
        }

        // Net spend (consume - refund), aggregated server-side.
        const statPromise = getUserLogStats({ type: 2, ...params })

        // Fetch all billable records (consume / error / refund) directly so
        // unrelated log types (login / manage / system) never crowd out real
        // billing rows within the client-side row cap. The /stat endpoint only
        // reports rpm/tpm for the last 60 seconds, so request/token totals
        // cannot come from it.
        const fetchByType = async (type: number): Promise<{
          items: UsageLog[]
          total: number
        }> => {
          const items: UsageLog[] = []
          let total = 0
          let page = 1
          while (items.length < BILLING_MAX_ROWS) {
            const body = await getUserLogs({
              p: page,
              page_size: BILLING_FETCH_BATCH,
              type,
              ...params,
            })
            const batch = (body.data?.items ?? []) as UsageLog[]
            items.push(...batch)
            total = body.data?.total ?? items.length
            if (items.length >= total) break
            page += 1
          }
          return { items, total }
        }

        const fetchAll = async (): Promise<{
          items: UsageLog[]
          total: number
        }> => {
          const [consume, error, refund] = await Promise.all([
            fetchByType(BILLING_LOG_TYPES.CONSUME),
            fetchByType(BILLING_LOG_TYPES.ERROR),
            fetchByType(BILLING_LOG_TYPES.REFUND),
          ])
          // Merge and sort newest-first (each type is already desc, stable).
          const items = [...consume.items, ...error.items, ...refund.items].sort(
            (a, b) => b.created_at - a.created_at
          )
          return {
            items,
            total: consume.total + error.total + refund.total,
          }
        }

        const [statResult, { items, total }] = await Promise.all([
          statPromise,
          fetchAll(),
        ])
        if (seq !== loadSeqRef.current) return // stale response, drop it
        setNetQuota(statResult.data?.quota ?? 0)
        setLogs(items.filter(isBillableLog))
        if (items.length >= BILLING_MAX_ROWS && items.length < total) {
          setTruncated(true)
          toast.warning(
            t('Billing data truncated, please narrow the time range')
          )
        }
      } catch (err) {
        if (seq !== loadSeqRef.current) return // stale response, drop it
        const message = err instanceof Error ? err.message : String(err)
        toast.error(message)
        setLogs([])
        setNetQuota(0)
      } finally {
        if (seq === loadSeqRef.current) setLoading(false)
      }
    },
    [t]
  )

  useEffect(() => {
    void load(EMPTY_BILLING_FILTERS)
  }, [load])

  const handlePresetChange = useCallback(
    (days: number) => {
      const range = buildTimeRange(days)
      setActivePresetDays(days)
      const next: BillingFilters = { ...filters, ...range }
      setFilters(next)
      void load(next)
    },
    [filters, load]
  )

  const handleApply = useCallback(
    (next: BillingFilters) => {
      setFilters(next)
      void load(next)
    },
    [load]
  )

  const handleReset = useCallback(() => {
    setActivePresetDays(0)
    setFilters(EMPTY_BILLING_FILTERS)
    void load(EMPTY_BILLING_FILTERS)
  }, [load])

  const stats: BillingStatsData | null = useMemo(
    () => ({
      netQuota,
      // 真实请求数：排除退款与差额补扣等计费调整记录，并计入失败请求。
      requestCount: logs.filter(isRealRequest).length,
      totalTokens: sumTokens(logs),
    }),
    [netQuota, logs]
  )

  return (
    <SectionPageLayout fixedContent>
      <SectionPageLayout.Title>
        {t('Consumer Billing')}
      </SectionPageLayout.Title>
      <SectionPageLayout.Content>
        <div className='flex h-full min-h-0 flex-col gap-4'>
          <div className='flex flex-col gap-4'>
            <BillingFilterBar
              filters={filters}
              activePresetDays={activePresetDays}
              onPresetChange={handlePresetChange}
              onApply={handleApply}
              onReset={handleReset}
              loading={loading}
            />
            <BillingStats stats={stats} loading={loading} />
          </div>
          <div className='min-h-0 flex-1'>
            <BillingTable logs={logs} loading={loading} />
          </div>
          {truncated ? (
            <p className='text-muted-foreground text-right text-xs'>
              {t('Showing up to')}{' '}
              {BILLING_MAX_ROWS.toLocaleString('en-US')}{' '}
              {t('records, please narrow the time range')}
            </p>
          ) : null}
        </div>
      </SectionPageLayout.Content>
    </SectionPageLayout>
  )
}
