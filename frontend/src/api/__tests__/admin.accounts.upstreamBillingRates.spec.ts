import { beforeEach, describe, expect, it, vi } from 'vitest'

const { get } = vi.hoisted(() => ({ get: vi.fn() }))

vi.mock('@/api/client', () => ({ apiClient: { get } }))

import { getUpstreamBillingRatesWithEtag } from '@/api/admin/accounts'

describe('admin account upstream billing rate snapshot API', () => {
  beforeEach(() => get.mockReset())

  it('passes the current page filters and returns the ETag', async () => {
    const data = { items: [{ account_id: 7, snapshot: null }], total: 1, page: 2, page_size: 20 }
    get.mockResolvedValueOnce({ status: 200, headers: { etag: '"rate-1"' }, data })

    await expect(getUpstreamBillingRatesWithEtag(2, 20, { platform: 'openai', sort_by: 'name', sort_order: 'asc' }))
      .resolves.toEqual({ notModified: false, etag: '"rate-1"', data })
    expect(get).toHaveBeenCalledWith('/admin/accounts/upstream-billing-rates', {
      params: { page: 2, page_size: 20, platform: 'openai', sort_by: 'name', sort_order: 'asc' },
      headers: {},
      signal: undefined,
      validateStatus: expect.any(Function)
    })
  })

  it('handles 304 without discarding the caller ETag', async () => {
    get.mockResolvedValueOnce({ status: 304, headers: { etag: '"rate-2"' }, data: null })

    await expect(getUpstreamBillingRatesWithEtag(1, 20, undefined, { etag: '"rate-1"' }))
      .resolves.toEqual({ notModified: true, etag: '"rate-2"', data: null })
    const options = get.mock.calls[0][1]
    expect(options.headers).toEqual({ 'If-None-Match': '"rate-1"' })
    expect(options.validateStatus(304)).toBe(true)
    expect(options.validateStatus(500)).toBe(false)
  })
})
