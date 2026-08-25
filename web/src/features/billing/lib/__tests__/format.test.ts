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

import type { UsageLog } from '@/features/usage-logs/data/schema'

// format.ts 的依赖链（zustand 配置 store / dayjs locale 等）依赖浏览器全局，
// 先注入 happy-dom 全局再动态导入被测模块。
const domWindow = new Window()
const GLOBALS_TO_INJECT: Array<keyof Window> = [
  'window',
  'document',
  'navigator',
  'localStorage',
  'HTMLElement',
  'Node',
  'Event',
  'CustomEvent',
]
for (const key of GLOBALS_TO_INJECT) {
  try {
    // @ts-expect-error -- test globals injection
    globalThis[key] = domWindow[key]
  } catch {
    // 某些全局（如 Node 的 navigator）只读，改用 defineProperty 覆盖。
    Object.defineProperty(globalThis, key, {
      value: domWindow[key],
      configurable: true,
      writable: true,
    })
  }
}
after(() => {
  for (const key of GLOBALS_TO_INJECT) {
    try {
      // @ts-expect-error -- cleanup test globals
      delete globalThis[key]
    } catch {
      // ignore read-only globals during cleanup
    }
  }
  domWindow.close()
})

const {
  isRealRequest,
  rowTokenInfo,
  submissionTaskIds,
  sumTokens,
  taskTotalTokensByTaskId,
} = await import('../format')

function makeLog(overrides: Partial<UsageLog>): UsageLog {
  return {
    id: 1,
    user_id: 1,
    created_at: 1_700_000_000,
    type: 2,
    content: '',
    username: '',
    token_name: '',
    model_name: '',
    quota: 0,
    prompt_tokens: 0,
    completion_tokens: 0,
    use_time: 0,
    is_stream: false,
    channel: 0,
    channel_name: '',
    token_id: 0,
    group: '',
    ip: '',
    other: '',
    request_id: '',
    upstream_request_id: '',
    ...overrides,
  }
}

/** JSON.stringify a LogOtherData-like object into the `other` string field. */
function other(obj: Record<string, unknown>): string {
  return JSON.stringify(obj)
}

function taskMap(logs: UsageLog[]): Map<string, number> {
  return taskTotalTokensByTaskId(logs)
}

describe('billing aggregation', () => {
  test('sync request: input + output tokens', () => {
    const logs = [
      makeLog({
        id: 1,
        type: 2,
        prompt_tokens: 100,
        completion_tokens: 50,
      }),
      makeLog({
        id: 2,
        type: 5,
        prompt_tokens: 30,
        completion_tokens: 10,
      }),
    ]
    assert.equal(sumTokens(logs), 190)
    assert.equal(isRealRequest(logs[0]), true)
    assert.equal(isRealRequest(logs[1]), true)
  })

  test('new async task: submission + refund share task_id, token counted once on submission', () => {
    const logs = [
      makeLog({
        id: 1,
        type: 2,
        other: other({ is_task: true, task_id: 't1' }),
      }),
      makeLog({
        id: 2,
        type: 6,
        other: other({ task_id: 't1', total_tokens: 40594 }),
      }),
    ]
    // 顶部只统计一次 40594。
    assert.equal(sumTokens(logs), 40594)

    const map = taskMap(logs)
    const submissionIds = submissionTaskIds(logs)
    assert.deepEqual([...submissionIds], ['t1'])

    // 提交行（is_task 命中 map）显示任务总 token。
    assert.deepEqual(rowTokenInfo(logs[0], map, submissionIds), {
      value: 40594,
      isTaskTotal: true,
    })
    // 调整行（存在可匹配提交行）不重复显示。
    assert.deepEqual(rowTokenInfo(logs[1], map, submissionIds), {
      value: 0,
      isTaskTotal: false,
    })
  })

  test('historical async task: submission has no task_id, refund row shows 40594', () => {
    const logs = [
      makeLog({
        id: 1,
        type: 2,
        other: other({ is_task: true }),
      }),
      makeLog({
        id: 2,
        type: 6,
        other: other({ task_id: 't-hist', total_tokens: 40594 }),
      }),
    ]
    // 顶部必须显示 40594，即使提交日志没有 task_id。
    assert.equal(sumTokens(logs), 40594)

    const map = taskMap(logs)
    const submissionIds = submissionTaskIds(logs)
    // 提交行没有 task_id，不会进入 submissionTaskIds。
    assert.equal(submissionIds.size, 0)

    // 提交行（无 task_id，无 token 字段）显示 0。
    assert.deepEqual(rowTokenInfo(logs[0], map, submissionIds), {
      value: 0,
      isTaskTotal: false,
    })
    // 退款行携带 total_tokens 且无匹配提交行 → 显示 40594。
    assert.deepEqual(rowTokenInfo(logs[1], map, submissionIds), {
      value: 40594,
      isTaskTotal: true,
    })
  })

  test('same task_id with both 补扣 and 退款: counted only once', () => {
    const logs = [
      makeLog({
        id: 1,
        type: 2,
        other: other({ is_task: true, task_id: 't1' }),
      }),
      makeLog({
        id: 2,
        type: 2,
        other: other({
          task_id: 't1',
          pre_consumed_quota: 1000,
          total_tokens: 40594,
        }),
      }),
      makeLog({
        id: 3,
        type: 6,
        other: other({ task_id: 't1', total_tokens: 40594 }),
      }),
    ]
    assert.equal(sumTokens(logs), 40594)
  })

  test('two different task_ids accumulate independently', () => {
    const logs = [
      makeLog({ id: 1, type: 2, other: other({ is_task: true, task_id: 'a' }) }),
      makeLog({ id: 2, type: 6, other: other({ task_id: 'a', total_tokens: 100 }) }),
      makeLog({ id: 3, type: 2, other: other({ is_task: true, task_id: 'b' }) }),
      makeLog({ id: 4, type: 6, other: other({ task_id: 'b', total_tokens: 200 }) }),
    ]
    assert.equal(sumTokens(logs), 300)

    const map = taskMap(logs)
    const submissionIds = submissionTaskIds(logs)
    assert.deepEqual([...submissionIds].sort(), ['a', 'b'])
    assert.equal(map.get('a'), 100)
    assert.equal(map.get('b'), 200)
    // 各自的提交行命中各自的任务总 token。
    assert.deepEqual(rowTokenInfo(logs[0], map, submissionIds), {
      value: 100,
      isTaskTotal: true,
    })
    assert.deepEqual(rowTokenInfo(logs[2], map, submissionIds), {
      value: 200,
      isTaskTotal: true,
    })
  })

  test('refund / 补扣 adjustments do not count towards total requests', () => {
    const logs = [
      makeLog({ id: 1, type: 2 }), // 成功消费 = 1 次请求
      makeLog({ id: 2, type: 5 }), // 失败请求 = 1 次请求
      makeLog({ id: 3, type: 6 }), // 退款 ≠ 请求
      makeLog({
        id: 4,
        type: 2,
        other: other({ task_id: 't1', pre_consumed_quota: 1000 }),
      }), // 差额补扣 ≠ 新请求
      makeLog({
        id: 5,
        type: 2,
        other: other({ is_task: true, task_id: 't2' }),
      }), // 异步提交 = 1 次请求
    ]
    assert.equal(logs.filter(isRealRequest).length, 3)
  })

  test('no fuzzy matching by time, model or amount', () => {
    // 仅凭时间/模型/金额相同的同步消费日志，绝不能串到任务总 token。
    const logs = [
      makeLog({
        id: 1,
        type: 2,
        model_name: 'seedance',
        prompt_tokens: 10,
        completion_tokens: 20,
        created_at: 1000,
      }),
      makeLog({
        id: 2,
        type: 6,
        model_name: 'seedance',
        created_at: 1000,
        other: other({ task_id: 'unrelated', total_tokens: 9999 }),
      }),
    ]
    // 顶部 = 普通 30 + 独立任务 9999，不会用同步日志去模糊命中 total_tokens。
    assert.equal(sumTokens(logs), 30 + 9999)

    const map = taskMap(logs)
    const submissionIds = submissionTaskIds(logs)
    // 同步消费行没有 task_id，走 input+output。
    assert.deepEqual(rowTokenInfo(logs[0], map, submissionIds), {
      value: 30,
      isTaskTotal: false,
    })
    // 退款行：task_id 不在任何提交集合，携带 total_tokens → 显示自身值。
    assert.deepEqual(rowTokenInfo(logs[1], map, submissionIds), {
      value: 9999,
      isTaskTotal: true,
    })
  })
})
