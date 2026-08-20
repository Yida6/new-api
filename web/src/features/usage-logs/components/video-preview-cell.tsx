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
import { useEffect, useState } from 'react'
import { useTranslation } from 'react-i18next'

import { Dialog } from '@/components/dialog'
import { api } from '@/lib/api'

import {
  createVideoPlaybackRequest,
  parseVideoPlaybackURL,
} from '../lib/video-playback'

type VideoPreviewCellProps = {
  taskID: string
}

export function VideoPreviewCell(props: VideoPreviewCellProps) {
  const { t } = useTranslation()
  const [open, setOpen] = useState(false)
  const [loading, setLoading] = useState(false)
  const [failed, setFailed] = useState(false)
  const [playbackURL, setPlaybackURL] = useState<string>()

  useEffect(() => {
    if (!open) return

    let cancelled = false
    setLoading(true)
    setFailed(false)
    setPlaybackURL(undefined)

    ;(async () => {
      try {
        const request = createVideoPlaybackRequest(props.taskID)
        const response = await api.get<unknown>(request.url, request.config)
        if (cancelled) return
        setPlaybackURL(parseVideoPlaybackURL(response.data))
      } catch {
        if (!cancelled) setFailed(true)
      } finally {
        if (!cancelled) setLoading(false)
      }
    })()

    return () => {
      cancelled = true
    }
  }, [open, props.taskID])

  let previewContent = playbackURL ? (
    <video
      src={playbackURL}
      controls
      preload='metadata'
      className='max-h-[70vh] w-full rounded-md'
      onError={() => setFailed(true)}
    />
  ) : null
  if (loading) {
    previewContent = (
      <span className='text-muted-foreground text-sm'>{t('Loading...')}</span>
    )
  } else if (failed || !playbackURL) {
    previewContent = (
      <span className='text-destructive text-sm'>{t('Failed to load')}</span>
    )
  }

  return (
    <>
      <button
        type='button'
        className='text-foreground text-xs hover:underline'
        onClick={() => setOpen(true)}
      >
        {t('Click to preview video')}
      </button>
      <Dialog
        open={open}
        onOpenChange={setOpen}
        title={t('Click to preview video')}
        description={`${t('Task ID:')} ${props.taskID}`}
        contentClassName='sm:max-w-4xl'
      >
        <div className='bg-muted/30 flex min-h-56 items-center justify-center rounded-lg border p-2'>
          {previewContent}
        </div>
      </Dialog>
    </>
  )
}
