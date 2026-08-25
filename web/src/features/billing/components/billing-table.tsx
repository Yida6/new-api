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

import {
  formatBillingTime,
  formatTokenCount,
  getBillingStatus,
  getBillingTypeLabel,
} from '../lib/format'
import { BILLING_PAGE_SIZES, type BillingLog } from '../types'

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
          <TableCell colSpan={8} className='text-muted-foreground py-10 text-center'>
            {t('Loading')}
          </TableCell>
        </TableRow>
      )
    }
    if (logs.length === 0) {
      return (
        <TableRow className='hover:bg-transparent'>
          <TableCell colSpan={8} className='text-muted-foreground py-10 text-center'>
            {t('No billing records')}
          </TableCell>
        </TableRow>
      )
    }
    return pageRows.map((log) => {
      const status = getBillingStatus(log)
      const billingType = getBillingTypeLabel(log)
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
            {formatTokenCount(log.prompt_tokens)}
          </TableCell>
          <TableCell className='text-right tabular-nums'>
            {formatTokenCount(log.completion_tokens)}
          </TableCell>
          <TableCell className='text-right font-medium tabular-nums'>
            {formatLogQuota(log.quota || 0)}
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
              <TableHead className='text-right'>
                {t('Input Tokens')}
              </TableHead>
              <TableHead className='text-right'>
                {t('Output Tokens')}
              </TableHead>
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
