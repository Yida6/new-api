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
import { useMemo, useState, type ReactNode } from 'react'
import { useTranslation } from 'react-i18next'

import { StatusBadge } from '@/components/status-badge'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Card, CardContent } from '@/components/ui/card'
import {
  NativeSelect,
  NativeSelectOption,
} from '@/components/ui/native-select'
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from '@/components/ui/table'
import { formatLogQuota } from '@/lib/format'
import { cn } from '@/lib/utils'

import { parseLogOther } from '@/features/usage-logs/lib'

import {
  formatBillingTime,
  formatTokenCount,
  getBillingStatus,
  getBillingTypeLabel,
  taskTotalTokensByTaskId,
} from '../lib/format'
import { BILLING_LOG_TYPES, BILLING_PAGE_SIZES, type BillingLog } from '../types'

interface BillingTableProps {
  logs: BillingLog[]
  loading: boolean
}

function ellipsisId(id: string): string {
  if (id.length <= 24) return id
  return `${id.slice(0, 16)}…${id.slice(-8)}`
}

export function BillingTable(props: BillingTableProps) {
  const { t } = useTranslation()
  const { logs, loading } = props
  const [page, setPage] = useState(1)
  const [pageSize, setPageSize] = useState<number>(20)

  const totalPages = Math.max(1, Math.ceil(logs.length / pageSize))
  const safePage = Math.min(page, totalPages)

  const pageRows = useMemo(() => {
    const start = (safePage - 1) * pageSize
    return logs.slice(start, start + pageSize)
  }, [logs, safePage, pageSize])

  // async 任务的总 token 由调整日志（退款/补扣）的 other.total_tokens 提供，
  // 通过 task_id 反查到提交消费日志所在的行，保证"任务一次提交对应一次 token 计入"。
  const taskTokens = useMemo(
    () => taskTotalTokensByTaskId(logs),
    [logs]
  )

  const pageNumbers = useMemo(() => {
    const max = 7
    const half = Math.floor(max / 2)
    let s = Math.max(1, safePage - half)
    const e = Math.min(totalPages, s + max - 1)
    s = Math.max(1, e - max + 1)
    const nums: number[] = []
    for (let i = s; i <= e; i++) nums.push(i)
    return nums
  }, [safePage, totalPages])

  const body = useMemo<ReactNode>(() => {
    if (loading) {
      return (
        <TableRow className='hover:bg-transparent'>
          <TableCell colSpan={7} className='text-muted-foreground py-10 text-center'>
            {t('Loading')}
          </TableCell>
        </TableRow>
      )
    }
    if (logs.length === 0) {
      return (
        <TableRow className='hover:bg-transparent'>
          <TableCell colSpan={7} className='text-muted-foreground py-10 text-center'>
            {t('No billing records')}
          </TableCell>
        </TableRow>
      )
    }
    return pageRows.map((log) => {
      const status = getBillingStatus(log)
      const billingType = getBillingTypeLabel(log)
      const other = parseLogOther(log.other)
      // 异步任务的 token 归属于"提交消费日志"（is_task=true，且非补扣/退款
      // 调整行），由同集合内的退款/补扣日志按 task_id 提供 total_tokens。
      // 调整行不再二次显示，避免同任务在明细里出现两次。
      const baseInput = log.prompt_tokens || 0
      const baseOutput = log.completion_tokens || 0
      const isSubmissionTask = !!other?.is_task && !other?.pre_consumed_quota
      const resolvedTokens =
        baseInput > 0 || baseOutput > 0
          ? baseInput + baseOutput
          : isSubmissionTask && other?.task_id
            ? (taskTokens.get(other.task_id) ?? 0)
            : 0
      const isAsyncTaskTokens =
        baseInput === 0 && baseOutput === 0 && resolvedTokens > 0
      // 退款记录显示负号，使明细金额可直接与"消费减退款"的累计消费核对。
      const amount =
        log.type === BILLING_LOG_TYPES.REFUND ? -log.quota : log.quota
      return (
        <TableRow key={log.id}>
          <TableCell className='text-muted-foreground whitespace-nowrap font-mono text-xs tabular-nums'>
            {formatBillingTime(log.created_at)}
          </TableCell>
          <TableCell className='max-w-64 truncate font-mono text-xs'>
            {log.model_name || '—'}
          </TableCell>
          <TableCell>
            <Badge variant='secondary' className='font-normal'>
              {t(billingType.labelKey)}
            </Badge>
          </TableCell>
          <TableCell className='text-right tabular-nums'>
            {isAsyncTaskTokens ? (
              <div className='flex flex-col items-end gap-0.5'>
                <span className='font-mono text-xs font-medium tabular-nums'>
                  {formatTokenCount(resolvedTokens)}
                </span>
                <span className='text-muted-foreground/60 text-[11px]'>
                  {t('Task total tokens')}
                </span>
              </div>
            ) : (
              formatTokenCount(
                (log.prompt_tokens || 0) + (log.completion_tokens || 0)
              )
            )}
          </TableCell>
          <TableCell className='text-right font-medium tabular-nums'>
            {formatLogQuota(amount || 0)}
          </TableCell>
          <TableCell>
            <StatusBadge
              label={t(status.labelKey)}
              variant={status.variant}
              size='sm'
              copyable={false}
            />
          </TableCell>
          <TableCell>
            <span
              className='text-muted-foreground inline-block max-w-44 truncate font-mono text-xs'
              title={log.request_id}
            >
              {log.request_id ? ellipsisId(log.request_id) : '—'}
            </span>
          </TableCell>
        </TableRow>
      )
    })
  }, [loading, logs, pageRows, t])

  return (
    <Card className='shadow-none'>
      <CardContent className='p-0'>
        <Table>
          <TableHeader>
            <TableRow className='hover:bg-transparent'>
              <TableHead className='whitespace-nowrap'>{t('Time')}</TableHead>
              <TableHead>{t('Model Name')}</TableHead>
              <TableHead>{t('Billing Type')}</TableHead>
              <TableHead className='text-right'>{t('Tokens')}</TableHead>
              <TableHead className='text-right'>{t('Amount')}</TableHead>
              <TableHead>{t('Status')}</TableHead>
              <TableHead>{t('Request ID')}</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>{body}</TableBody>
        </Table>

        <div className='flex flex-wrap items-center justify-between gap-3 border-t p-4'>
          <p className='text-muted-foreground text-[13px]'>
            {t('Total')} {logs.length.toLocaleString('en-US')} ·{' '}
            {t('Page')} {safePage} / {totalPages}
          </p>
          <div className='flex items-center gap-2'>
            <div className='text-muted-foreground flex items-center gap-2 text-[13px]'>
              <span>{t('Rows per page')}</span>
              <NativeSelect
                className='h-8 w-auto'
                value={String(pageSize)}
                onChange={(e) => {
                  setPageSize(Number(e.target.value))
                  setPage(1)
                }}
              >
                {BILLING_PAGE_SIZES.map((size) => (
                  <NativeSelectOption key={size} value={String(size)}>
                    {size}
                  </NativeSelectOption>
                ))}
              </NativeSelect>
            </div>
            <div className='flex items-center gap-1'>
              <Button
                variant='outline'
                size='sm'
                className='h-8 px-2.5'
                disabled={safePage <= 1}
                onClick={() => setPage((p) => Math.max(1, p - 1))}
              >
                ‹
              </Button>
              {pageNumbers.map((num) => (
                <Button
                  key={num}
                  variant={num === safePage ? 'default' : 'outline'}
                  size='sm'
                  className={cn('h-8 min-w-8 px-2')}
                  onClick={() => setPage(num)}
                >
                  {num}
                </Button>
              ))}
              <Button
                variant='outline'
                size='sm'
                className='h-8 px-2.5'
                disabled={safePage >= totalPages}
                onClick={() => setPage((p) => Math.min(totalPages, p + 1))}
              >
                ›
              </Button>
            </div>
          </div>
        </div>
      </CardContent>
    </Card>
  )
}
