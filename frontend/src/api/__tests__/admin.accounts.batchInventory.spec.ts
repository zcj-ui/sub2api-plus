import { beforeEach, describe, expect, it, vi } from 'vitest'

const { post } = vi.hoisted(() => ({ post: vi.fn() }))

vi.mock('@/api/client', () => ({
  apiClient: { post }
}))

import { batchHealthProbe, batchInventory } from '@/api/admin/accounts'

describe('admin accounts batch probe API', () => {
  beforeEach(() => {
    post.mockReset()
    post.mockResolvedValue({ data: { results: [], healthy: 0, failed: 0, skipped: 0, quota_fetched: 0 } })
  })

  it('allows a full selected-account batch enough time to finish through slow proxies', async () => {
    await batchHealthProbe([1])
    await batchInventory([2])

    expect(post).toHaveBeenNthCalledWith(
      1,
      '/admin/accounts/batch-health-probe',
      { account_ids: [1] },
      { timeout: 20 * 60_000 }
    )
    expect(post).toHaveBeenNthCalledWith(
      2,
      '/admin/accounts/batch-inventory',
      { account_ids: [2] },
      { timeout: 20 * 60_000 }
    )
  })
})
