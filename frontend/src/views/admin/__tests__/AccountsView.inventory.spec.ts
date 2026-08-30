import { beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'

import AccountsView from '../AccountsView.vue'

const {
  listAccounts,
  listWithEtag,
  getBatchTodayStats,
  getUpstreamBillingProbeSettings,
  getAllProxies,
  getAllGroups,
  batchHealthProbe,
  batchInventory,
  showError,
  showSuccess
} = vi.hoisted(() => ({
  listAccounts: vi.fn(),
  listWithEtag: vi.fn(),
  getBatchTodayStats: vi.fn(),
  getUpstreamBillingProbeSettings: vi.fn(),
  getAllProxies: vi.fn(),
  getAllGroups: vi.fn(),
  batchHealthProbe: vi.fn(),
  batchInventory: vi.fn(),
  showError: vi.fn(),
  showSuccess: vi.fn()
}))

vi.mock('@/api/admin', () => ({
  adminAPI: {
    accounts: {
      list: listAccounts,
      listWithEtag,
      getBatchTodayStats,
      getUpstreamBillingProbeSettings,
      batchHealthProbe,
      batchInventory,
      batchDelete: vi.fn(),
      batchClearError: vi.fn(),
      batchRefresh: vi.fn(),
      bulkUpdate: vi.fn()
    },
    proxies: { getAll: getAllProxies },
    groups: { getAll: getAllGroups }
  }
}))

vi.mock('@/stores/app', () => ({
  useAppStore: () => ({ showError, showSuccess, showInfo: vi.fn() })
}))

vi.mock('@/stores/auth', () => ({
  useAuthStore: () => ({ token: 'test-token' })
}))

vi.mock('vue-i18n', async () => {
  const actual = await vi.importActual<typeof import('vue-i18n')>('vue-i18n')
  return { ...actual, useI18n: () => ({ t: (key: string) => key }) }
})

const failedSnapshot = (reason: string) => ({
  status: 'failed',
  mode: 'openai_oauth_quota',
  attempts: 2,
  checked_at: '2026-08-14T00:00:00Z',
  reason
})

const account = (id: number, snapshot?: ReturnType<typeof failedSnapshot>) => ({
  id,
  name: `account-${id}`,
  platform: 'openai',
  type: 'oauth',
  status: 'active',
  schedulable: true,
  extra: snapshot ? { account_health_probe: snapshot } : {},
  created_at: '2026-08-14T00:00:00Z',
  updated_at: '2026-08-14T00:00:00Z'
})

const page = (items: ReturnType<typeof account>[]) => ({
  items,
  total: items.length,
  page: 1,
  page_size: 20,
  pages: 1
})

const DataTableStub = {
  props: ['data'],
  template: '<div><div v-for="row in data" :key="row.id"><slot name="cell-select" :row="row" /></div></div>'
}

const BulkActionsStub = {
  props: ['selectedIds', 'healthProbeRunning', 'inventoryRunning'],
  emits: ['health-probe', 'inventory'],
  template: `
    <div>
      <button data-test="health" @click="$emit('health-probe')">health</button>
      <button data-test="inventory" @click="$emit('inventory')">inventory</button>
      <span data-test="selected">{{ selectedIds.length }}</span>
    </div>
  `
}

const mountView = () => mount(AccountsView, {
  global: {
    stubs: {
      AppLayout: { template: '<div><slot /></div>' },
      TablePageLayout: { template: '<div><slot name="filters" /><slot name="table" /><slot name="pagination" /></div>' },
      DataTable: DataTableStub,
      Pagination: true,
      ConfirmDialog: true,
      AccountTableActions: { template: '<div><slot name="beforeCreate" /><slot name="after" /></div>' },
      AccountTableFilters: true,
      AccountBulkActionsBar: BulkActionsStub,
      AccountInventoryModal: {
        props: ['show', 'response'],
        template: '<div v-if="show" data-test="inventory-modal"><span data-test="request-failed">{{ response?.request_failed_accounts ?? 0 }}</span><span data-test="result-count">{{ response?.results?.length ?? 0 }}</span></div>'
      },
      AccountActionMenu: true,
      ImportDataModal: true,
      ReAuthAccountModal: true,
      AccountTestModal: true,
      AccountStatsModal: true,
      ScheduledTestsPanel: true,
      SyncFromCrsModal: true,
      TempUnschedStatusModal: true,
      ErrorPassthroughRulesModal: true,
      TLSFingerprintProfilesModal: true,
      CreateAccountModal: true,
      EditAccountModal: true,
      BulkEditAccountModal: true,
      PlatformTypeBadge: true,
      AccountCapacityCell: true,
      AccountStatusIndicator: true,
      AccountTodayStatsCell: true,
      AccountGroupsCell: true,
      AccountUsageCell: true,
      Icon: true,
      HelpTooltip: true
    }
  }
})

describe('admin AccountsView selected-account inventory', () => {
  beforeEach(() => {
    localStorage.clear()
    vi.clearAllMocks()
    listWithEtag.mockResolvedValue({ notModified: true, etag: null, data: null })
    getBatchTodayStats.mockResolvedValue({ stats: {} })
    getUpstreamBillingProbeSettings.mockResolvedValue({ enabled: true, interval_minutes: 30 })
    getAllProxies.mockResolvedValue([])
    getAllGroups.mockResolvedValue([])
  })

  it('probes only selected IDs and preserves failures outside the current batch', async () => {
    const initial = [account(1, failedSnapshot('old-1')), account(2, failedSnapshot('old-2'))]
    const refreshed = [account(1), account(2, failedSnapshot('old-2'))]
    listAccounts.mockResolvedValueOnce(page(initial)).mockResolvedValueOnce(page(refreshed))
    batchHealthProbe.mockResolvedValue({
      healthy: 1,
      failed: 0,
      skipped: 0,
      results: [{
        account_id: 1,
        name: 'account-1',
        platform: 'openai',
        type: 'oauth',
        healthy: true,
        dead: false,
        attempts: 1,
        mode: 'openai_oauth_quota'
      }]
    })

    const wrapper = mountView()
    await flushPromises()
    await wrapper.findAll('input[type="checkbox"]')[0].setValue(true)
    expect(wrapper.get('[data-test="selected"]').text()).toBe('1')
    await wrapper.get('[data-test="health"]').trigger('click')
    await flushPromises()

    expect(batchHealthProbe).toHaveBeenCalledWith([1])
    expect(wrapper.get('[data-testid="account-health-failure-pool"]').text()).toContain('old-2')
    expect(wrapper.get('[data-testid="account-health-failure-pool"]').text()).not.toContain('old-1')
  })

  it('keeps completed health-probe results when a later batch request fails', async () => {
    const accounts = Array.from({ length: 201 }, (_, index) => account(index + 1))
    listAccounts.mockResolvedValue(page(accounts))
    batchHealthProbe
      .mockResolvedValueOnce({
        healthy: 200,
        failed: 0,
        skipped: 0,
        results: accounts.slice(0, 200).map(item => ({
          account_id: item.id,
          name: item.name,
          platform: 'openai',
          type: 'oauth',
          healthy: true,
          dead: false,
          attempts: 1,
          mode: 'openai_oauth_quota'
        }))
      })
      .mockRejectedValueOnce(new Error('second probe batch unavailable'))

    const wrapper = mountView()
    await flushPromises()
    ;(wrapper.vm as unknown as { setSelectedIds: (ids: number[]) => void }).setSelectedIds(
      accounts.map(item => item.id)
    )
    await flushPromises()
    await wrapper.get('[data-test="health"]').trigger('click')
    await flushPromises()

    expect(batchHealthProbe).toHaveBeenCalledTimes(2)
    expect(wrapper.get('[data-testid="account-health-partial"]').text()).toContain('admin.accounts.healthProbe.partial')
    expect(wrapper.get('[data-testid="account-health-partial"]').text()).toContain('second probe batch unavailable')
    expect(showError).toHaveBeenCalledWith('admin.accounts.healthProbe.partial')
  })

  it('keeps a successful inventory result when the following account-list refresh fails', async () => {
    listAccounts.mockResolvedValueOnce(page([account(1)])).mockRejectedValueOnce(new Error('reload failed'))
    batchInventory.mockResolvedValue({
      healthy: 1,
      failed: 0,
      skipped: 0,
      quota_fetched: 1,
      results: [{
        account_id: 1,
        name: 'account-1',
        platform: 'openai',
        type: 'oauth',
        healthy: true,
        dead: false,
        attempts: 1,
        mode: 'openai_oauth_quota',
        quota: { fetched_at: 1 }
      }]
    })

    const wrapper = mountView()
    await flushPromises()
    await wrapper.findAll('input[type="checkbox"]')[0].setValue(true)
    await wrapper.get('[data-test="inventory"]').trigger('click')
    await flushPromises()

    expect(batchInventory).toHaveBeenCalledWith([1])
    expect(wrapper.find('[data-test="inventory-modal"]').exists()).toBe(true)
    expect(showSuccess).toHaveBeenCalledWith('admin.accounts.inventory.completed')
    expect(showError).toHaveBeenCalledTimes(1)
    expect(showError.mock.calls[0]?.[0]).not.toBe('admin.accounts.inventory.requestFailed')
  })

  it('chunks more than 200 selected accounts and merges inventory results', async () => {
    const accounts = Array.from({ length: 205 }, (_, index) => account(index + 1))
    listAccounts.mockResolvedValue(page(accounts))
    batchInventory.mockImplementation(async (ids: number[]) => ({
      healthy: ids.length,
      failed: 0,
      skipped: 0,
      quota_fetched: ids.length,
      results: ids.map(id => ({
        account_id: id,
        name: `account-${id}`,
        platform: 'openai',
        type: 'oauth',
        healthy: true,
        dead: false,
        attempts: 1,
        mode: 'openai_oauth_quota',
        health_persisted: true,
        quota_persisted: true,
        quota: { fetched_at: 1 }
      }))
    }))

    const wrapper = mountView()
    await flushPromises()
    ;(wrapper.vm as unknown as { setSelectedIds: (ids: number[]) => void }).setSelectedIds(
      accounts.map(item => item.id)
    )
    await flushPromises()
    await wrapper.get('[data-test="inventory"]').trigger('click')
    await flushPromises()

    expect(batchInventory).toHaveBeenCalledTimes(2)
    expect(batchInventory.mock.calls[0]?.[0]).toHaveLength(200)
    expect(batchInventory.mock.calls[1]?.[0]).toEqual([201, 202, 203, 204, 205])
    expect(showSuccess).toHaveBeenCalledWith('admin.accounts.inventory.completed')
  })

  it('keeps completed inventory results when a later batch request fails', async () => {
    const accounts = Array.from({ length: 201 }, (_, index) => account(index + 1))
    listAccounts.mockResolvedValue(page(accounts))
    batchInventory
      .mockResolvedValueOnce({
        healthy: 200,
        failed: 0,
        skipped: 0,
        quota_fetched: 200,
        results: accounts.slice(0, 200).map(item => ({
          account_id: item.id,
          name: item.name,
          platform: 'openai',
          type: 'oauth',
          healthy: true,
          dead: false,
          attempts: 1,
          mode: 'openai_oauth_quota',
          quota: { fetched_at: 1 }
        }))
      })
      .mockRejectedValueOnce(new Error('second batch unavailable'))

    const wrapper = mountView()
    await flushPromises()
    ;(wrapper.vm as unknown as { setSelectedIds: (ids: number[]) => void }).setSelectedIds(
      accounts.map(item => item.id)
    )
    await flushPromises()
    await wrapper.get('[data-test="inventory"]').trigger('click')
    await flushPromises()

    expect(batchInventory).toHaveBeenCalledTimes(2)
    expect(wrapper.get('[data-test="inventory-modal"]').exists()).toBe(true)
    expect(wrapper.get('[data-test="request-failed"]').text()).toBe('1')
    expect(wrapper.get('[data-test="result-count"]').text()).toBe('200')
    expect(showError).toHaveBeenCalledWith('admin.accounts.inventory.partial')
  })
})
