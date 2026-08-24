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
import type { TFunction } from 'i18next'
import { useEffect } from 'react'
import type { UseFormReset } from 'react-hook-form'
import { toast } from 'sonner'

import { getUser } from '../api'
import { ERROR_MESSAGES } from '../constants'
import { transformUserToFormDefaults, type UserFormValues } from '../lib'

type UserFormLoaderOptions = {
  open: boolean
  userId?: number
  reset: UseFormReset<UserFormValues>
  t: TFunction
  loadUser?: typeof getUser
  notifyError?: (message: string) => void
}

export function useUserFormLoader(options: UserFormLoaderOptions) {
  const { open, userId, reset, t, loadUser = getUser, notifyError } = options

  useEffect(() => {
    if (!open || userId === undefined) {
      return
    }

    let active = true

    void loadUser(userId)
      .then((result) => {
        if (!active) {
          return
        }
        if (result.success && result.data) {
          reset(transformUserToFormDefaults(result.data))
          return
        }
        const message = result.message || t(ERROR_MESSAGES.LOAD_FAILED)
        if (notifyError) {
          notifyError(message)
        } else {
          toast.error(message)
        }
      })
      .catch(() => {
        if (active) {
          const message = t(ERROR_MESSAGES.LOAD_FAILED)
          if (notifyError) {
            notifyError(message)
          } else {
            toast.error(message)
          }
        }
      })

    return () => {
      active = false
    }
  }, [open, userId, reset, t, loadUser, notifyError])
}
