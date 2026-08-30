import { describe, expect, it, vi } from 'vitest'
import { mount } from '@vue/test-utils'

import AccountInventoryModal from '../AccountInventoryModal.vue'
import type { BatchAccountInventoryResponse } from '@/api/admin/accounts'

vi.mock('vue-i18n', () => ({
  useI18n: () => ({
    t: (key: string) => key
  })
}))

const response: BatchAccountInventoryResponse = {
  healthy: 2,
  failed: 0,
  skipped: 1,
  quota_fetched: 1,
  results: [
    {
      account_id: 11,
      name: 'Codex OAuth',
      platform: 'openai',
      type: 'oauth',
      healthy: true,
      dead: false,
      attempts: 1,
      mode: 'openai_oauth_quota',
      quota: {
        fetched_at: 1,
        credits: {
          has_credits: true,
          unlimited: false,
          overage_limit_reached: false,
          balance: '50'
        },
        rate_limit_reset_credits: { available_count: 3 },
        rate_limit: {
          allowed: true,
          limit_reached: false,
          primary_window: {
            used_percent: 20,
            limit_window_seconds: 604800,
            reset_after_seconds: 1,
            reset_at: 2
          },
          secondary_window: {
            used_percent: 40,
            limit_window_seconds: 18000,
            reset_after_seconds: 1,
            reset_at: 2
          }
        }
      }
    },
    {
      account_id: 12,
      name: 'OpenAI Key',
      platform: 'openai',
      type: 'apikey',
      healthy: true,
      dead: false,
      attempts: 1,
      mode: 'openai_apikey_connection'
    },
    {
      account_id: 13,
      name: 'Claude',
      platform: 'anthropic',
      type: 'oauth',
      healthy: false,
      dead: false,
      attempts: 0,
      mode: '',
      reason: 'health mode supports OpenAI OAuth and API Key accounts only'
    }
  ]
}

describe('AccountInventoryModal', () => {
  it('renders quota, USD reference, reset credits, usage windows, API Key note, and skipped rows', () => {
    const wrapper = mount(AccountInventoryModal, {
      props: {
        show: true,
        response
      },
      global: {
        stubs: {
          BaseDialog: {
            props: ['show', 'title'],
            template: '<div v-if="show"><slot /><slot name="footer" /></div>'
          }
        }
      }
    })

    const text = wrapper.text()
    expect(text).toContain('50 Credit')
    expect(text).toContain('≈ $2.00')
    expect(text).toContain('3')
    expect(text).toContain('40.0%')
    expect(text).toContain('20.0%')
    expect(text).toContain('admin.accounts.inventory.apiKeyNoQuota')
    expect(text).toContain('admin.accounts.inventory.skipped')
    expect(text).toContain('health mode supports OpenAI OAuth and API Key accounts only')
  })

  it('accepts numeric credits.balance values from compatibility relays', () => {
    const wrapper = mount(AccountInventoryModal, {
      props: {
        show: true,
        response: {
          healthy: 1,
          failed: 0,
          skipped: 0,
          quota_fetched: 1,
          results: [{
            account_id: 21,
            name: 'Numeric credit relay',
            platform: 'openai',
            type: 'oauth',
            healthy: true,
            dead: false,
            attempts: 1,
            mode: 'openai_oauth_quota',
            quota: {
              fetched_at: 1,
              credits: {
                has_credits: true,
                unlimited: false,
                overage_limit_reached: false,
                balance: 12.5
              }
            }
          }]
        }
      },
      global: {
        stubs: {
          BaseDialog: {
            props: ['show', 'title'],
            template: '<div v-if="show"><slot /><slot name="footer" /></div>'
          }
        }
      }
    })

    expect(wrapper.text()).toContain('12.5 Credit')
    expect(wrapper.text()).toContain('≈ $0.50')
  })
})
