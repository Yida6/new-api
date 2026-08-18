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
import { describe, test } from 'node:test'

import { API_DEMOS } from '../hero-terminal-demo'

describe('home page mainstream model demos', () => {
  test('shows the selected mainstream model providers', () => {
    assert.deepEqual(
      API_DEMOS.map((demo) => demo.label),
      ['DeepSeek', '豆包', '通义千问', '智谱 GLM']
    )
  })

  test('keeps every provider behind the same compatible endpoint', () => {
    assert.equal(
      API_DEMOS.every((demo) => demo.endpoint === '/v1/chat/completions'),
      true
    )
  })

  test('uses a stable identifier for every provider demo', () => {
    assert.equal(
      new Set(API_DEMOS.map((demo) => demo.id)).size,
      API_DEMOS.length
    )
  })
})
