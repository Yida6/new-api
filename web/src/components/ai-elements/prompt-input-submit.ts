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
import type { FileUIPart } from 'ai'
import type { FormEvent } from 'react'

type PromptInputMessage = {
  text: string
  files: FileUIPart[]
}

type SubmitPromptInputOptions = {
  text: string
  files: (FileUIPart & { id: string })[]
  event: FormEvent<HTMLFormElement>
  convertBlobUrl: (url: string) => Promise<string>
  onSubmit: (
    message: PromptInputMessage,
    event: FormEvent<HTMLFormElement>
  ) => void | Promise<void>
  onSuccess: () => void
  onConversionError: () => void
}

export async function submitPromptInput(
  options: SubmitPromptInputOptions
): Promise<void> {
  let convertedFiles: FileUIPart[]
  try {
    convertedFiles = await Promise.all(
      options.files.map(async ({ id: _id, ...item }) => {
        if (item.url?.startsWith('blob:')) {
          return {
            ...item,
            url: await options.convertBlobUrl(item.url),
          }
        }
        return item
      })
    )
  } catch {
    options.onConversionError()
    return
  }

  try {
    await options.onSubmit(
      { text: options.text, files: convertedFiles },
      options.event
    )
    options.onSuccess()
  } catch {
    // Keep the message and attachments so the caller can retry.
  }
}
