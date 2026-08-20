import { beforeEach, describe, expect, it, vi } from 'vitest'

const { get, post } = vi.hoisted(() => ({
  get: vi.fn(),
  post: vi.fn(),
}))

vi.mock('../client', () => ({
  apiClient: {
    get,
    post,
  },
}))

import {
  getRollbackVersions,
  performUpdate,
  restartService,
  rollback,
  type RollbackVersionInfo
} from '@/api/admin/system'

describe('admin system rollback API', () => {
  beforeEach(() => {
    get.mockReset()
    post.mockReset()
  })

  it('getRollbackVersions fetches the rollback version list', async () => {
    const versions: RollbackVersionInfo[] = [
      {
        version: '0.1.146',
        published_at: '2026-07-07T00:00:00Z',
        html_url: 'https://github.com/zcj-ui/sub2api-plus/releases/tag/v0.1.146'
      }
    ]
    get.mockResolvedValue({ data: { versions } })

    const result = await getRollbackVersions()

    expect(get).toHaveBeenCalledWith('/admin/system/rollback-versions')
    expect(result.versions).toEqual(versions)
  })

  it('rollback posts the target version in the request body', async () => {
    post.mockResolvedValue({ data: { message: 'ok', need_restart: true } })

    const result = await rollback('0.1.146', { idempotencyKey: 'rollback-operation-key' })

    expect(post).toHaveBeenCalledWith(
      '/admin/system/rollback',
      { version: '0.1.146' },
      {
        timeout: 15 * 60 * 1000,
        headers: { 'Idempotency-Key': 'rollback-operation-key' }
      }
    )
    expect(result.need_restart).toBe(true)
  })

  it('rollback without a version posts no body (legacy backup rollback)', async () => {
    post.mockResolvedValue({ data: { message: 'ok', need_restart: true } })

    await rollback(undefined, { idempotencyKey: 'legacy-rollback-key' })

    expect(post).toHaveBeenCalledWith(
      '/admin/system/rollback',
      undefined,
      {
        timeout: 15 * 60 * 1000,
        headers: { 'Idempotency-Key': 'legacy-rollback-key' }
      }
    )
  })

  it('attaches stable idempotency keys to update and restart operations', async () => {
    post.mockResolvedValue({ data: { message: 'ok', need_restart: true } })

    await performUpdate({ idempotencyKey: 'update-operation-key' })
    await restartService({ idempotencyKey: 'restart-operation-key' })

    expect(post).toHaveBeenNthCalledWith(
      1,
      '/admin/system/update',
      undefined,
      {
        timeout: 15 * 60 * 1000,
        headers: { 'Idempotency-Key': 'update-operation-key' }
      }
    )
    expect(post).toHaveBeenNthCalledWith(
      2,
      '/admin/system/restart',
      undefined,
      { headers: { 'Idempotency-Key': 'restart-operation-key' } }
    )
  })

  it('generates an idempotency key when a caller does not provide one', async () => {
    post.mockResolvedValue({ data: { message: 'ok', need_restart: true } })

    await performUpdate()

    const config = post.mock.calls[0][2] as { headers?: Record<string, string> }
    expect(config.headers?.['Idempotency-Key']).toMatch(/^system-update-/)
  })
})
