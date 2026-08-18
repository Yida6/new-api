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
import { Activity, BadgeDollarSign, GitBranch, Waypoints } from 'lucide-react'
import { useTranslation } from 'react-i18next'

interface StatsProps {
  className?: string
}

export function Stats(_props: StatsProps) {
  const { t } = useTranslation()

  const capabilities = [
    {
      icon: Waypoints,
      title: t('One unified API'),
      desc: t('Switch models without rebuilding your application'),
    },
    {
      icon: GitBranch,
      title: t('Resilient routing'),
      desc: t('Route by priority, weight and channel health'),
    },
    {
      icon: BadgeDollarSign,
      title: t('Unified billing'),
      desc: t('Track model usage and cost with clear rules'),
    },
    {
      icon: Activity,
      title: t('Full observability'),
      desc: t('Inspect requests, latency, tokens and failures'),
    },
  ]

  return (
    <section className='border-border/40 bg-muted/10 relative z-10 border-y'>
      <div className='mx-auto grid max-w-6xl grid-cols-1 divide-y px-6 sm:grid-cols-2 sm:divide-y-0 md:grid-cols-4 md:divide-x'>
        {capabilities.map((capability) => (
          <div
            key={capability.title}
            className='border-border/40 flex items-start gap-3 py-6 sm:px-5 md:px-6'
          >
            <div className='border-border/50 bg-background flex size-9 shrink-0 items-center justify-center rounded-lg border text-blue-500 shadow-xs'>
              <capability.icon aria-hidden className='size-4' />
            </div>
            <div>
              <h2 className='text-sm font-semibold'>{capability.title}</h2>
              <p className='text-muted-foreground mt-1 text-xs leading-relaxed'>
                {capability.desc}
              </p>
            </div>
          </div>
        ))}
      </div>
    </section>
  )
}
