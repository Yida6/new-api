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

import type { TFunction } from 'i18next'

import { getApiKeyFormSchema } from '../api-key-form'

const t = ((key: string) => key) as TFunction

function quotaIssue(result: { success: boolean; error?: { issues: unknown[] } }) {
  assert.equal(result.success, false)
  const issues = (result.error?.issues ?? []) as Array<{
    path: (string | number)[]
    message: string
  }>
  return issues.filter((issue) => issue.path.includes('remain_quota_dollars'))
}

describe('API key quota ceiling validation', () => {
  test('accepts large finite quota within the int64 business ceiling', () => {
    const schema = getApiKeyFormSchema(t)
    const result = schema.safeParse({
      name: 'big-quota',
      remain_quota_dollars: 111_111,
      unlimited_quota: false,
      model_limits: [],
      auto_groups_mode: 'inherit',
      auto_groups: [],
    })
    assert.equal(result.success, true)
  })

  test('accepts the exact ceiling amount', () => {
    const schema = getApiKeyFormSchema(t)
    const result = schema.safeParse({
      name: 'at-ceiling',
      // 默认配置：maxQuota 1e15 / quotaPerUnit 500000 = 2e9 USD
      remain_quota_dollars: 2_000_000_000,
      unlimited_quota: false,
      model_limits: [],
      auto_groups_mode: 'inherit',
      auto_groups: [],
    })
    assert.equal(result.success, true)
  })

  test('rejects quota above the ceiling with the maximum message', () => {
    const schema = getApiKeyFormSchema(t)
    const issues = quotaIssue(
      schema.safeParse({
        name: 'above-ceiling',
        remain_quota_dollars: 2_000_000_001,
        unlimited_quota: false,
        model_limits: [],
        auto_groups_mode: 'inherit',
        auto_groups: [],
      })
    )
    assert.equal(issues.length, 1)
    assert.equal(issues[0].message, 'Quota exceeds the maximum allowed value')
  })

  test('still rejects negative quota with the original message', () => {
    const schema = getApiKeyFormSchema(t)
    const issues = quotaIssue(
      schema.safeParse({
        name: 'negative',
        remain_quota_dollars: -1,
        unlimited_quota: false,
        model_limits: [],
        auto_groups_mode: 'inherit',
        auto_groups: [],
      })
    )
    assert.equal(issues.length, 1)
    assert.equal(issues[0].message, 'Quota must be zero or greater')
  })

  test('unlimited quota bypasses the ceiling check', () => {
    const schema = getApiKeyFormSchema(t)
    const result = schema.safeParse({
      name: 'unlimited',
      remain_quota_dollars: 1e12,
      unlimited_quota: true,
      model_limits: [],
      auto_groups_mode: 'inherit',
      auto_groups: [],
    })
    assert.equal(result.success, true)
  })
})
