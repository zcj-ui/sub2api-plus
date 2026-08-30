import { defineComponent } from 'vue'
import { describe, expect, it, vi } from 'vitest'
import { mount } from '@vue/test-utils'
import AccountTableFilters from '../AccountTableFilters.vue'

vi.mock('vue-i18n', async () => {
  const actual = await vi.importActual<typeof import('vue-i18n')>('vue-i18n')
  return {
    ...actual,
    useI18n: () => ({
      t: (key: string) => key
    })
  }
})

const SelectStub = defineComponent({
  name: 'SelectStub',
  props: {
    options: {
      type: Array,
      required: true
    }
  },
  template: `
    <div data-testid="filter-select">
      <span
        v-for="option in options"
        :key="String(option.value)"
        :data-value="String(option.value)"
      >{{ option.label }}</span>
    </div>
  `
})

const SearchInputStub = defineComponent({
  name: 'SearchInputStub',
  template: '<div data-testid="account-search" />'
})

describe('AccountTableFilters', () => {
  it('exposes every backend-supported account type, including service accounts and upstream accounts', () => {
    const wrapper = mount(AccountTableFilters, {
      props: {
        searchQuery: '',
        filters: {
          platform: '',
          type: '',
          status: '',
          privacy_mode: '',
          group: ''
        },
        groups: []
      },
      global: {
        stubs: {
          Select: SelectStub,
          SearchInput: SearchInputStub
        }
      }
    })

    const typeSelect = wrapper.findAll('[data-testid="filter-select"]')[1]
    expect(typeSelect.exists()).toBe(true)
    expect(typeSelect.find('[data-value="oauth"]').exists()).toBe(true)
    expect(typeSelect.find('[data-value="setup-token"]').exists()).toBe(true)
    expect(typeSelect.find('[data-value="apikey"]').exists()).toBe(true)
    expect(typeSelect.find('[data-value="service_account"]').text()).toBe('admin.accounts.serviceAccountType')
    expect(typeSelect.find('[data-value="upstream"]').text()).toBe('admin.accounts.upstreamType')
    expect(typeSelect.find('[data-value="bedrock"]').exists()).toBe(true)
  })
})
