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

// zustand persist 依赖 localStorage，先注入 happy-dom 全局再导入 store 相关模块
const domWindow = new Window()
for (const key of [
  'window',
  'document',
  'navigator',
  'localStorage',
  'HTMLElement',
  'Node',
  'Event',
  'CustomEvent',
]) {
  // @ts-expect-error -- test globals injection
  globalThis[key] = domWindow[key]
}
after(() => {
  for (const key of [
    'window',
    'document',
    'navigator',
    'localStorage',
    'HTMLElement',
    'Node',
    'Event',
    'CustomEvent',
  ]) {
    // @ts-expect-error -- cleanup test globals
    delete globalThis[key]
  }
  domWindow.close()
})

const {
  formatMaxQuotaHint,
  isQuotaInputAboveMax,
  maxQuotaToDisplayAmount,
} = await import('../format')
const { DEFAULT_CURRENCY_CONFIG, useSystemConfigStore } = await import(
  '@/stores/system-config-store'
)

function setCurrency(overrides: Record<string, unknown>) {
  useSystemConfigStore.getState().setConfig({
    currency: {
      ...DEFAULT_CURRENCY_CONFIG,
      ...overrides,
    },
  })
}

/** 提取字符串中的纯数字，规避 locale 千分位/货币符号差异 */
function digitsOnly(value: string): string {
  return value.replaceAll(/[^0-9]/g, '')
}

describe('max quota display helpers', () => {
  after(() => {
    setCurrency({})
  })

  test('converts the int64 business ceiling to USD at the default config', () => {
    setCurrency({})
    // maxQuota = 1e15 内部额度，quotaPerUnit = 500000 → 2e9 USD
    assert.equal(maxQuotaToDisplayAmount(), 2_000_000_000)
  })

  test('flags only amounts above the ceiling in internal quota units', () => {
    setCurrency({})
    // int64 改造目标场景：111111 USD 必须放行
    assert.equal(isQuotaInputAboveMax(111_111), false)
    // 旧 int32 上限约 4294.96 USD，早已放行
    assert.equal(isQuotaInputAboveMax(4294.97), false)
    // 恰好等于上限：不超
    assert.equal(isQuotaInputAboveMax(2_000_000_000), false)
    // 超过上限：拒绝
    assert.equal(isQuotaInputAboveMax(2_000_000_001), true)
    assert.equal(isQuotaInputAboveMax(1e12), true)
    // 非法输入一律不触发“超限”
    assert.equal(isQuotaInputAboveMax(0), false)
    assert.equal(isQuotaInputAboveMax(-100), false)
    assert.equal(isQuotaInputAboveMax(Number.NaN), false)
    assert.equal(isQuotaInputAboveMax(Number.POSITIVE_INFINITY), false)
  })

  test('scales the ceiling with a non-default quotaPerUnit', () => {
    setCurrency({ quotaPerUnit: 500_000 * 2 })
    // 1e15 / 1e6 = 1e9 USD
    assert.equal(maxQuotaToDisplayAmount(), 1_000_000_000)
    assert.equal(isQuotaInputAboveMax(1_000_000_000), false)
    assert.equal(isQuotaInputAboveMax(1_000_000_001), true)
  })

  test('renders a localized ceiling hint for USD display', () => {
    setCurrency({})
    const hint = formatMaxQuotaHint()
    assert.equal(digitsOnly(hint), '2000000000')
  })

  test('converts the ceiling through the CNY exchange rate', () => {
    setCurrency({ quotaDisplayType: 'CNY', usdExchangeRate: 7 })
    assert.equal(maxQuotaToDisplayAmount(), 14_000_000_000)
    assert.equal(digitsOnly(formatMaxQuotaHint()), '14000000000')
    // 输入金额始终按当前展示币种口径换算（parseQuotaFromDollars 先除以汇率）
    assert.equal(isQuotaInputAboveMax(14_000_000_000), false)
    assert.equal(isQuotaInputAboveMax(14_000_000_001), true)
  })

  test('returns the raw unit ceiling for TOKENS display', () => {
    setCurrency({ quotaDisplayType: 'TOKENS' })
    assert.equal(maxQuotaToDisplayAmount(), DEFAULT_CURRENCY_CONFIG.maxQuota)
    assert.equal(digitsOnly(formatMaxQuotaHint()), '1000000000000000')
  })
})
