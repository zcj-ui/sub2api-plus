import { mount } from '@vue/test-utils'
import { describe, expect, it, vi } from 'vitest'

import ProxySelector from '../ProxySelector.vue'

vi.mock('vue-i18n', async () => {
  const actual = await vi.importActual<typeof import('vue-i18n')>('vue-i18n')
  return {
    ...actual,
    useI18n: () => ({
      t: (key: string, params?: Record<string, unknown>) =>
        params?.id === undefined ? key : `${key}:${params.id}`
    })
  }
})

const activeProxy = {
  id: 7,
  name: 'fixed-egress',
  protocol: 'http',
  host: '127.0.0.1',
  port: 1080,
  username: null,
  status: 'active',
  expires_at: null,
  fallback_mode: 'none',
  expiry_warn_days: 0,
  created_at: '',
  updated_at: ''
} as const

describe('ProxySelector configured proxy state', () => {
  it('shows no proxy only when the account has no proxy id', () => {
    const wrapper = mount(ProxySelector, { props: { modelValue: null, proxies: [] } })
    expect(wrapper.get('.select-value').text()).toBe('admin.accounts.noProxy')
  })

  it('shows the configured id when its proxy is inactive, deleted, or not loaded', () => {
    const wrapper = mount(ProxySelector, { props: { modelValue: 99, proxies: [] } })
    expect(wrapper.get('.select-value').text()).toBe('admin.accounts.configuredProxyUnavailable:99')
  })

  it('shows full endpoint details for a loaded proxy', () => {
    const wrapper = mount(ProxySelector, { props: { modelValue: 7, proxies: [activeProxy] as any } })
    expect(wrapper.get('.select-value').text()).toContain('fixed-egress')
    expect(wrapper.get('.select-value').text()).toContain('http://127.0.0.1:1080')
  })
})
