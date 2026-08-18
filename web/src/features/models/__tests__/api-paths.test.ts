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
import { afterEach, describe, test } from 'node:test'

import type { AxiosAdapter } from 'axios'

import { api } from '@/lib/api'

import {
  createPrefillGroup,
  getPrefillGroups,
  updatePrefillGroup,
} from '../api'

const originalAdapter = api.defaults.adapter

afterEach(() => {
  api.defaults.adapter = originalAdapter
})

function captureRequestUrl(): { adapter: AxiosAdapter; getUrl: () => string } {
  let requestUrl = ''
  return {
    adapter: async (config) => {
      requestUrl = config.url || ''
      return {
        config,
        data: { success: true, data: [] },
        headers: {},
        status: 200,
        statusText: 'OK',
      }
    },
    getUrl: () => requestUrl,
  }
}

describe('model prefill group API paths', () => {
  test('loads prefill groups from the trailing-slash route', async () => {
    const request = captureRequestUrl()
    api.defaults.adapter = request.adapter

    await getPrefillGroups()

    assert.equal(request.getUrl(), '/api/prefill_group/')
  })

  test('creates prefill groups through the trailing-slash route', async () => {
    const request = captureRequestUrl()
    api.defaults.adapter = request.adapter

    await createPrefillGroup({
      name: 'featured',
      type: 'model',
      items: ['gpt-5'],
    })

    assert.equal(request.getUrl(), '/api/prefill_group/')
  })

  test('updates prefill groups through the trailing-slash route', async () => {
    const request = captureRequestUrl()
    api.defaults.adapter = request.adapter

    await updatePrefillGroup({ id: 1, name: 'featured' })

    assert.equal(request.getUrl(), '/api/prefill_group/')
  })
})
