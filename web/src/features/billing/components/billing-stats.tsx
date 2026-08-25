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
import { useTranslation } from 'react-i18next'

import { Card, CardContent } from '@/components/ui/card'
import { formatLogQuota } from '@/lib/format'

import type { BillingStats } from '../types'

interface BillingStatsProps {
  stats: BillingStats | null
  loading: boolean
}

function StatCard(props: {
  label: string
  value: React.ReactNode
  loading: boolean
}) {
  return (
    <Card className='shadow-none'>
      <CardContent className='pt-6'>
        <p className='text-muted-foreground text-[13px]'>{props.label}</p>
        <p
          className={`text-2xl font-semibold tracking-tight tabular-nums ${
            props.loading ? 'opacity-40' : ''
          }`}
        >
          {props.value}
        </p>
      </CardContent>
    </Card>
  )
}

export function BillingStats(props: BillingStatsProps) {
  const { t } = useTranslation()
  const { stats, loading } = props

  return (
    <div className='grid grid-cols-2 gap-4 lg:grid-cols-4'>
      <StatCard
        label={t('Cumulative Spending')}
        loading={loading}
        value={
          stats ? (
            <span className='text-chart-1'>
              {formatLogQuota(stats.netQuota)}
            </span>
          ) : (
            '—'
          )
        }
      />
      <StatCard
        label={t('Total Requests')}
        loading={loading}
        value={
          stats ? (
            <span className='text-chart-1'>
              {stats.requestCount.toLocaleString('en-US')}
            </span>
          ) : (
            '—'
          )
        }
      />
      <StatCard
        label={t('Tokens')}
        loading={loading}
        value={
          stats ? (
            <span className='text-chart-1'>
              {stats.totalTokens.toLocaleString('en-US')}
            </span>
          ) : (
            '—'
          )
        }
      />
    </div>
  )
}
