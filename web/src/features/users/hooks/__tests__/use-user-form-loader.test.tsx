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
import assert from 'node:assert/strict'
import { after, describe, test } from 'node:test'

import { Window } from 'happy-dom'
import type { TFunction } from 'i18next'
import type { UseFormReset } from 'react-hook-form'

import type { UserFormValues } from '../../lib'
import type { ApiResponse, User } from '../../types'

const domWindow = new Window()
for (const key of [
  'window',
  'document',
  'HTMLElement',
  'HTMLFormElement',
] as const) {
  Object.defineProperty(globalThis, key, {
    configurable: true,
    value: domWindow[key],
  })
}

type Deferred = {
  promise: Promise<ApiResponse<User>>
  resolve: (value: ApiResponse<User>) => void
}

const requests = new Map<number, Deferred>()
function deferred(): Deferred {
  let resolve!: Deferred['resolve']
  const promise = new Promise<ApiResponse<User>>((resolvePromise) => {
    resolve = resolvePromise
  })
  return { promise, resolve }
}

const loadUser = (id: number) => {
  const request = deferred()
  requests.set(id, request)
  return request.promise
}

const translate = ((key: string) => key) as TFunction

const { act } = await import('react')
const { createRoot } = await import('react-dom/client')
const { useUserFormLoader } = await import('../use-user-form-loader')

const reactTestGlobals = globalThis as typeof globalThis & {
  IS_REACT_ACT_ENVIRONMENT?: boolean
}
reactTestGlobals.IS_REACT_ACT_ENVIRONMENT = true

function user(id: number, username: string): User {
  return {
    id,
    username,
    display_name: username,
    role: 1,
    quota: 0,
    group: 'default',
  } as User
}

function Harness(props: {
  userId: number
  reset: UseFormReset<UserFormValues>
  load?: typeof loadUser
  notifyError?: (message: string) => void
}) {
  useUserFormLoader({
    open: true,
    userId: props.userId,
    reset: props.reset,
    t: translate,
    loadUser: props.load ?? loadUser,
    notifyError: props.notifyError,
  })
  return null
}

describe('user form loader', () => {
  after(() => {
    domWindow.close()
  })

  test('ignores a stale response after the selected user changes', async () => {
    const loadedUsers: string[] = []
    const reset = ((values: UserFormValues) => {
      loadedUsers.push(values.username)
    }) as UseFormReset<UserFormValues>
    const container = document.createElement('div')
    document.body.append(container)
    const root = createRoot(container)

    await act(async () => root.render(<Harness reset={reset} userId={1} />))
    await act(async () => root.render(<Harness reset={reset} userId={2} />))

    await act(async () => {
      requests.get(2)?.resolve({ success: true, data: user(2, 'second') })
      await requests.get(2)?.promise
    })
    await act(async () => {
      requests.get(1)?.resolve({ success: true, data: user(1, 'first') })
      await requests.get(1)?.promise
    })

    assert.deepEqual(loadedUsers, ['second'])

    await act(async () => root.unmount())
    container.remove()
  })

  test('reports an active request failure without leaving an unhandled rejection', async () => {
    const messages: string[] = []
    const reset = (() => undefined) as UseFormReset<UserFormValues>
    const rejectingLoader = async () => {
      throw new Error('network unavailable')
    }
    const container = document.createElement('div')
    document.body.append(container)
    const root = createRoot(container)

    await act(async () =>
      root.render(
        <Harness
          load={rejectingLoader}
          notifyError={(message) => messages.push(message)}
          reset={reset}
          userId={3}
        />
      )
    )
    await act(() => Promise.resolve())

    assert.deepEqual(messages, ['Failed to load users'])

    await act(async () => root.unmount())
    container.remove()
  })
})
