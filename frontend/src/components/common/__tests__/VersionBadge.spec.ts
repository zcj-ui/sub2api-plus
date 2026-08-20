import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'

const mocks = vi.hoisted(() => ({
  authStore: {
    isAdmin: true
  },
  appStore: {
    versionLoading: false,
    currentVersion: '0.2.5',
    latestVersion: '0.2.6',
    hasUpdate: false,
    releaseInfo: null as { html_url?: string } | null,
    buildType: 'release' as 'source' | 'dev' | 'release',
    versionWarning: '',
    versionCached: false,
    versionCheckError: '',
    inPlaceUpdate: null as { supported: boolean; restriction_message?: string } | null,
    versionLoaded: true,
    updateRepo: 'owner/sub2api-fork',
    fetchVersion: vi.fn(),
    clearVersionCache: vi.fn(),
    showSuccess: vi.fn(),
    showError: vi.fn()
  },
  performUpdate: vi.fn(),
  restartService: vi.fn(),
  getRollbackVersions: vi.fn(),
  rollback: vi.fn(),
  copyToClipboard: vi.fn()
}))

vi.mock('vue-i18n', () => ({
  useI18n: () => ({
    t: (key: string) => key
  })
}))

vi.mock('@/stores', () => ({
  useAuthStore: () => mocks.authStore,
  useAppStore: () => mocks.appStore
}))

vi.mock('@/api/admin/system', () => ({
  performUpdate: mocks.performUpdate,
  restartService: mocks.restartService,
  getRollbackVersions: mocks.getRollbackVersions,
  rollback: mocks.rollback
}))

vi.mock('@/composables/useClipboard', () => ({
  useClipboard: () => ({
    copied: false,
    copyToClipboard: mocks.copyToClipboard
  })
}))

import VersionBadge from '../VersionBadge.vue'

function resetStore(overrides: Partial<typeof mocks.appStore> = {}) {
  Object.assign(mocks.authStore, { isAdmin: true })
  Object.assign(mocks.appStore, {
    versionLoading: false,
    currentVersion: '0.2.5',
    latestVersion: '0.2.6',
    hasUpdate: false,
    releaseInfo: null,
    buildType: 'release',
    versionWarning: '',
    versionCached: false,
    versionCheckError: '',
    inPlaceUpdate: null,
    versionLoaded: true,
    updateRepo: 'owner/sub2api-fork',
    fetchVersion: vi.fn().mockResolvedValue(null),
    clearVersionCache: vi.fn(),
    showSuccess: vi.fn(),
    showError: vi.fn(),
    ...overrides
  })
}

async function mountBadge() {
  const wrapper = mount(VersionBadge, {
    global: {
      stubs: {
        Icon: { template: '<i />' }
      }
    }
  })
  await flushPromises()
  await wrapper.get('button').trigger('click')
  await flushPromises()
  return wrapper
}

beforeEach(() => {
  vi.clearAllMocks()
  resetStore()
  mocks.getRollbackVersions.mockResolvedValue({ versions: [] })
})

afterEach(() => {
  document.body.innerHTML = ''
})

describe('VersionBadge in-place update capability', () => {
  it('does not render the update POST action when an older backend omits the capability', async () => {
    resetStore({ hasUpdate: true, inPlaceUpdate: null })

    const wrapper = await mountBadge()

    expect(wrapper.find('[data-testid="version-update-action"]').exists()).toBe(false)
  })

  it('keeps manual rollback available when online rollback is unsupported', async () => {
    resetStore({
      inPlaceUpdate: {
        supported: false,
        restriction_message: 'This deployment requires manual rollback.'
      }
    })
    mocks.getRollbackVersions.mockResolvedValue({
      versions: [
        {
          version: '0.2.4',
          published_at: '2026-08-19T00:00:00Z',
          html_url: 'https://github.com/owner/sub2api-fork/releases/tag/v0.2.4'
        }
      ]
    })

    const wrapper = await mountBadge()
    const rollbackAction = wrapper.get('[data-testid="version-rollback-action"]')
    await rollbackAction.trigger('click')
    await flushPromises()

    expect(mocks.getRollbackVersions).toHaveBeenCalledTimes(1)
    const versionButton = wrapper
      .findAll('button')
      .find((button) => button.text().includes('v0.2.4'))
    expect(versionButton).toBeDefined()
    await versionButton!.trigger('click')

    expect(wrapper.text()).toContain('git -C <install-directory> fetch')
    expect(wrapper.get('[data-testid="version-rollback-manual-only"]').text()).toContain(
      'This deployment requires manual rollback.'
    )
    expect(mocks.rollback).not.toHaveBeenCalled()
  })

  it('shows the update action when the backend explicitly supports in-place updates', async () => {
    resetStore({ hasUpdate: true, inPlaceUpdate: { supported: true } })

    const wrapper = await mountBadge()

    expect(wrapper.find('[data-testid="version-update-action"]').exists()).toBe(true)
  })

  it('keeps warning and cached state visible without claiming the version is current', async () => {
    resetStore({
      versionWarning: 'Using cached update data.',
      versionCached: true
    })

    const wrapper = await mountBadge()

    expect(wrapper.get('[data-testid="version-check-status"]').text()).toContain(
      'Using cached update data.'
    )
    expect(wrapper.text()).not.toContain('version.upToDate')
  })

  it('uses the backend update repository for the release link', async () => {
    resetStore({ updateRepo: 'upstream/sub2api' })

    const wrapper = await mountBadge()

    expect(wrapper.get('a').attributes('href')).toBe('https://github.com/upstream/sub2api')
  })
})
