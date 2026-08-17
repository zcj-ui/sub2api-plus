import { describe, expect, it, vi } from 'vitest'
import { fetchAccountSelectionMetadata } from '../accountSelection'

describe('fetchAccountSelectionMetadata selected-account paging', () => {
  it('stops paging once every selected account has been found', async () => {
    const fetchPage = vi.fn().mockResolvedValue({
      items: [{ id: 1, platform: 'openai', type: 'oauth' }],
      total: 3000,
      pages: 3
    })

    const metadata = await fetchAccountSelectionMetadata(fetchPage, {}, [1])

    expect(metadata).toEqual({
      platforms: ['openai'],
      types: ['oauth'],
      hasCredentialShadows: false
    })
    expect(fetchPage).toHaveBeenCalledTimes(1)
  })
})
