import { describe, expect, it, vi } from 'vitest'
import { mount } from '@vue/test-utils'

import AccountBulkActionsBar from '../AccountBulkActionsBar.vue'

vi.mock('vue-i18n', async () => {
  const actual = await vi.importActual<typeof import('vue-i18n')>('vue-i18n')
  return {
    ...actual,
    useI18n: () => ({
      t: (key: string) => key
    })
  }
})

describe('AccountBulkActionsBar', () => {
  it('allows selecting all results before any row is selected', async () => {
    const wrapper = mount(AccountBulkActionsBar, {
      props: {
        selectedIds: [],
        totalResults: 45,
        selectingAll: false,
        allResultsSelected: false
      }
    })

    const button = wrapper.findAll('button').find(item =>
      item.text().includes('admin.accounts.bulkActions.selectAllResults')
    )

    expect(button).toBeDefined()
    await button!.trigger('click')
    expect(wrapper.emitted('select-all-results')).toHaveLength(1)
  })

  it('preserves the upstream billing probe action from v0.1.166', async () => {
    const wrapper = mount(AccountBulkActionsBar, {
      props: {
        selectedIds: [1],
        totalResults: 45,
        selectingAll: false,
        allResultsSelected: false
      }
    })

    const button = wrapper.findAll('button').find(item =>
      item.text().includes('admin.accounts.bulkActions.probeUpstreamBilling')
    )

    expect(button).toBeDefined()
    await button!.trigger('click')
    expect(wrapper.emitted('probe-upstream-billing')).toHaveLength(1)
  })

  it('emits the batch health probe action', async () => {
    const wrapper = mount(AccountBulkActionsBar, {
      props: {
        selectedIds: [1, 2],
        totalResults: 2,
        selectingAll: false,
        allResultsSelected: true
      }
    })

    const button = wrapper.findAll('button').find(item =>
      item.text().includes('admin.accounts.bulkActions.healthProbe')
    )
    expect(button).toBeDefined()
    await button!.trigger('click')
    expect(wrapper.emitted('health-probe')).toHaveLength(1)
  })

  it('disables the batch health probe while a probe is running', () => {
    const wrapper = mount(AccountBulkActionsBar, {
      props: {
        selectedIds: [1],
        totalResults: 1,
        selectingAll: false,
        allResultsSelected: true,
        healthProbeRunning: true
      }
    })

    const button = wrapper.findAll('button').find(item =>
      item.text().includes('admin.accounts.bulkActions.healthProbe')
    )
    expect(button?.attributes('disabled')).toBeDefined()
  })

  it('emits selected-account inventory', async () => {
    const wrapper = mount(AccountBulkActionsBar, {
      props: {
        selectedIds: [1, 2],
        totalResults: 2,
        selectingAll: false,
        allResultsSelected: true
      }
    })

    const button = wrapper.findAll('button').find(item =>
      item.text().includes('admin.accounts.bulkActions.inventory')
    )
    expect(button).toBeDefined()
    await button!.trigger('click')
    expect(wrapper.emitted('inventory')).toHaveLength(1)
  })

  it('disables inventory and health actions while inventory is running', () => {
    const wrapper = mount(AccountBulkActionsBar, {
      props: {
        selectedIds: [1],
        totalResults: 1,
        selectingAll: false,
        allResultsSelected: true,
        inventoryRunning: true
      }
    })

    const inventoryButton = wrapper.findAll('button').find(item =>
      item.text().includes('admin.accounts.bulkActions.inventory')
    )
    const healthButton = wrapper.findAll('button').find(item =>
      item.text().includes('admin.accounts.bulkActions.healthProbe')
    )
    expect(inventoryButton?.attributes('disabled')).toBeDefined()
    expect(healthButton?.attributes('disabled')).toBeDefined()
  })

  it('labels the filter-wide update action explicitly', () => {
    const wrapper = mount(AccountBulkActionsBar, {
      props: {
        selectedIds: [1],
        totalResults: 2,
        selectingAll: false,
        allResultsSelected: false
      }
    })

    const button = wrapper.findAll('button').find(item => item.text().includes('editFiltered'))
    expect(button).toBeDefined()
    expect(button?.text()).toContain('admin.accounts.bulkActions.editFiltered')
  })
})
