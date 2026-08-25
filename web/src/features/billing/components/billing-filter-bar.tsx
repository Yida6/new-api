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
import { useCallback, useState } from 'react'
import { useTranslation } from 'react-i18next'

import { Button } from '@/components/ui/button'
import { Card, CardContent } from '@/components/ui/card'
import { Input } from '@/components/ui/input'
import { cn } from '@/lib/utils'

import { BILLING_TIME_PRESETS, type BillingFilters } from '../types'

interface BillingFilterBarProps {
  filters: BillingFilters
  activePresetDays: number
  onPresetChange: (days: number) => void
  onApply: (filters: BillingFilters) => void
  onReset: () => void
  loading: boolean
}

export function BillingFilterBar(props: BillingFilterBarProps) {
  const { t } = useTranslation()
  const { filters, activePresetDays, onPresetChange, onApply, onReset, loading } =
    props
  const [model, setModel] = useState(filters.model)
  const [requestId, setRequestId] = useState(filters.requestId)

  const handleApply = useCallback(() => {
    onApply({ ...filters, model: model.trim(), requestId: requestId.trim() })
  }, [filters, model, requestId, onApply])

  const handleReset = useCallback(() => {
    setModel('')
    setRequestId('')
    onReset()
  }, [onReset])

  return (
    <Card className='shadow-none'>
      <CardContent className='flex flex-wrap items-center gap-3 pt-6'>
        <div className='flex flex-wrap items-center gap-2'>
          {BILLING_TIME_PRESETS.map((preset) => (
            <Button
              key={preset.days}
              variant={activePresetDays === preset.days ? 'default' : 'outline'}
              size='sm'
              className='h-9'
              onClick={() => onPresetChange(preset.days)}
              disabled={loading}
            >
              {t(preset.labelKey)}
            </Button>
          ))}
        </div>
        <div className='min-w-40 flex-1' />
        <Input
          className='h-9 w-56'
          placeholder={t('Model Name')}
          value={model}
          onChange={(e) => setModel(e.target.value)}
          onKeyDown={(e) => {
            if (e.key === 'Enter') handleApply()
          }}
          disabled={loading}
        />
        <Input
          className={cn('h-9 w-64 font-mono')}
          placeholder={t('Request ID')}
          value={requestId}
          onChange={(e) => setRequestId(e.target.value)}
          onKeyDown={(e) => {
            if (e.key === 'Enter') handleApply()
          }}
          disabled={loading}
        />
        <Button className='h-9' onClick={handleApply} disabled={loading}>
          {t('Search')}
        </Button>
        <Button
          className='h-9'
          variant='outline'
          onClick={handleReset}
          disabled={loading}
        >
          {t('Reset')}
        </Button>
      </CardContent>
    </Card>
  )
}
