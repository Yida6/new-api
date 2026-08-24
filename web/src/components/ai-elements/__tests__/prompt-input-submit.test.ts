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

import type { FileUIPart } from 'ai'
import type { FormEvent } from 'react'

import { submitPromptInput } from '../prompt-input-submit'

const formEvent = {} as FormEvent<HTMLFormElement>

describe('prompt input submission', () => {
  test('keeps the draft and attachments when blob conversion fails', async () => {
    let submitCount = 0
    let successCount = 0
    let conversionErrorCount = 0

    await submitPromptInput({
      text: 'keep this draft',
      files: [
        {
          id: 'attachment-1',
          type: 'file',
          mediaType: 'image/png',
          url: 'blob:expired',
        },
      ],
      event: formEvent,
      convertBlobUrl: async () => {
        throw new Error('blob URL expired')
      },
      onSubmit: () => {
        submitCount += 1
      },
      onSuccess: () => {
        successCount += 1
      },
      onConversionError: () => {
        conversionErrorCount += 1
      },
    })

    assert.equal(submitCount, 0)
    assert.equal(successCount, 0)
    assert.equal(conversionErrorCount, 1)
  })

  test('cleans up only after conversion and submission both succeed', async () => {
    let submittedFiles: FileUIPart[] = []
    let successCount = 0

    await submitPromptInput({
      text: 'send this draft',
      files: [
        {
          id: 'attachment-1',
          type: 'file',
          mediaType: 'image/png',
          url: 'blob:ready',
        },
      ],
      event: formEvent,
      convertBlobUrl: async () => 'data:image/png;base64,AAAA',
      onSubmit: async (message) => {
        submittedFiles = message.files
      },
      onSuccess: () => {
        successCount += 1
      },
      onConversionError: () => {
        assert.fail('conversion should succeed')
      },
    })

    assert.deepEqual(submittedFiles, [
      {
        type: 'file',
        mediaType: 'image/png',
        url: 'data:image/png;base64,AAAA',
      },
    ])
    assert.equal(successCount, 1)
  })
})
