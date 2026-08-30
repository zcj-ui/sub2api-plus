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
  listHealthProbeFailures,
  showError,
  showSuccess
} = vi.hoisted(() => ({
  listAccounts: vi.fn(),
  listWithEtag: vi.fn(),
  getBatchTodayStats: vi.fn(),
  getUpstreamBillingProbeSettings: vi.fn(),
  getAllProxies: vi.fn(),
  getAllGroups: vi.fn(),
  listHealthProbeFailures: vi.fn(),
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
      listHealthProbeFailures,
      batchHealthProbe: vi.fn(),
      batchInventory: vi.fn(),
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

const fullAccount = (id: number) => ({
  id,
  name: `account-${id}`,
  platform: 'openai',
  type: 'oauth',
  status: 'active',
  schedulable: true,
  current_concurrency: 0,
  extra: { codex_fingerprint_mode: 'session' },
  created_at: '2026-08-14T00:00:00Z',
  updated_at: '2026-08-14T00:00:00Z'
})

const page = (items: ReturnType<typeof fullAccount>[]) => ({
  items,
  total: items.length,
  page: 1,
  page_size: 20,
  pages: 1
})

const mountView = () => mount(AccountsView, {
  global: {
    stubs: {
      AppLayout: { template: '<div><slot /></div>' },
      TablePageLayout: { template: '<div><slot name="filters" /><slot name="table" /><slot name="pagination" /></div>' },
      DataTable: {
        props: ['data'],
        template: '<div><div v-for="row in data" :key="row.id" class="test-row">{{ row.status }}|{{ row.extra?.codex_fingerprint_mode }}</div></div>'
      },
      Pagination: true,
      ConfirmDialog: true,
      AccountTableActions: { template: '<div><slot name="beforeCreate" /><slot name="after" /></div>' },
      AccountTableFilters: true,
      AccountBulkActionsBar: true,
      AccountInventoryModal: true,
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

describe('admin AccountsView initial page load', () => {
  beforeEach(() => {
    localStorage.clear()
    vi.clearAllMocks()
    listWithEtag.mockResolvedValue({ notModified: true, etag: null, data: null })
    getBatchTodayStats.mockResolvedValue({ stats: {} })
    getUpstreamBillingProbeSettings.mockResolvedValue({ enabled: true, interval_minutes: 30 })
    getAllProxies.mockResolvedValue([])
    getAllGroups.mockResolvedValue([])
    listHealthProbeFailures.mockResolvedValue([])
  })

  it('首次列表加载不使用 lite 精简响应，保证整表字段完整', async () => {
    listAccounts.mockResolvedValue(page([fullAccount(1), fullAccount(2)]))

    const wrapper = mountView()
    await flushPromises()

    expect(listAccounts).toHaveBeenCalled()
    // 精简响应只含 id/name/platform/type/健康快照，会让状态、额度、指纹等列渲染损坏；
    // 首屏必须走完整列表（lite 仅保留给全选元数据这类只读 ID 的场景）。
    const pageLoadCalls = listAccounts.mock.calls.filter(([_page, size]) => size !== 1000)
    expect(pageLoadCalls.length).toBeGreaterThan(0)
    for (const call of pageLoadCalls) {
      const requestParams = (call[2] ?? {}) as Record<string, unknown>
      expect(requestParams.lite).toBeUndefined()
    }

    // 完整响应的 status / extra 必须原样进入表格渲染
    const rows = wrapper.findAll('.test-row')
    expect(rows.length).toBe(2)
    expect(rows[0].text()).toBe('active|session')
    expect(rows[1].text()).toBe('active|session')
  })
})
