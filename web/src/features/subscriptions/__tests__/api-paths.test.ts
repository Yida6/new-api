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
import { afterEach, test } from 'node:test'

import type { AxiosAdapter } from 'axios'

import { api } from '@/lib/api'

import { getGroups } from '../api'

const originalAdapter = api.defaults.adapter

afterEach(() => {
  api.defaults.adapter = originalAdapter
})

test('loads subscription groups from the trailing-slash route', async () => {
  let requestUrl = ''
  const adapter: AxiosAdapter = async (config) => {
    requestUrl = config.url || ''
    return {
      config,
      data: { success: true, data: [] },
      headers: {},
      status: 200,
      statusText: 'OK',
    }
  }
  api.defaults.adapter = adapter

  await getGroups()

  assert.equal(requestUrl, '/api/group/')
})
