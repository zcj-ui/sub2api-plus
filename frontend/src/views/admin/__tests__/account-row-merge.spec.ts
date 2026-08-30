import { describe, expect, it } from 'vitest'

import { mergeDefinedAccountFields } from '../account-row-merge'

describe('mergeDefinedAccountFields', () => {
  it('preserves required fields omitted by a partial refresh', () => {
    const current = {
      id: 1,
      status: 'active',
      schedulable: true,
      extra: { codex_fingerprint_mode: 'session' },
    }
    const merged = mergeDefinedAccountFields(current, {
      id: 1,
      current_concurrency: 2,
      status: undefined,
      extra: undefined,
    })

    expect(merged).toEqual({
      id: 1,
      status: 'active',
      schedulable: true,
      extra: { codex_fingerprint_mode: 'session' },
      current_concurrency: 2,
    })
  })

  it('keeps explicit null as an intentional clear', () => {
    const current = { id: 1, rate_limit_reset_at: 'later' as string | null }
    expect(mergeDefinedAccountFields(current, { rate_limit_reset_at: null })).toEqual({
      id: 1,
      rate_limit_reset_at: null,
    })
  })
})
