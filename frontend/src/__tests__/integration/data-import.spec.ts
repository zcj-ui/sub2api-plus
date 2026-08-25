import { describe, it, expect, vi, beforeEach } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import ImportDataModal from '@/components/admin/account/ImportDataModal.vue'

const showError = vi.fn()
const showSuccess = vi.fn()
const showWarning = vi.fn()

vi.mock('@/stores/app', () => ({
  useAppStore: () => ({
    showError,
    showSuccess,
    showWarning
  })
}))

vi.mock('@/api/admin', () => ({
  adminAPI: {
    accounts: {
      importData: vi.fn()
    }
  }
}))

vi.mock('vue-i18n', () => ({
  useI18n: () => ({
    t: (key: string) => key
  })
}))

const mountModal = () =>
  mount(ImportDataModal, {
    props: { show: true },
    global: {
      stubs: {
        BaseDialog: { template: '<div><slot /><slot name="footer" /></div>' }
      }
    }
  })

const makeJsonFile = (name: string, content: string, type = 'application/json') => {
  const file = new File([content], name, { type })
  Object.defineProperty(file, 'text', {
    value: () => Promise.resolve(content)
  })
  return file
}

const setInputFiles = (element: Element, files: File[]) => {
  Object.defineProperty(element, 'files', {
    value: files,
    configurable: true
  })
}

const makeAccount = (name: string) => ({
  name,
  platform: 'openai',
  type: 'oauth',
  credentials: { fixture: name },
  concurrency: 1,
  priority: 1
})

describe('ImportDataModal', () => {
  beforeEach(async () => {
    showError.mockReset()
    showSuccess.mockReset()
    showWarning.mockReset()
    const { adminAPI } = await import('@/api/admin')
    vi.mocked(adminAPI.accounts.importData).mockReset()
  })

  it('未选择文件时提示错误', async () => {
    const wrapper = mountModal()

    await wrapper.find('form').trigger('submit')
    expect(showError).toHaveBeenCalledWith('admin.accounts.dataImportSelectFile')
  })

  it('无效 JSON 时按文件名提示解析失败', async () => {
    const { adminAPI } = await import('@/api/admin')
    const wrapper = mountModal()

    const input = wrapper.find('input[type="file"]')
    setInputFiles(input.element, [makeJsonFile('data.json', 'invalid json')])

    await input.trigger('change')
    await wrapper.find('form').trigger('submit')
    await flushPromises()

    expect(showError).toHaveBeenCalledWith('admin.accounts.dataImportParseFailedFile')
    expect(adminAPI.accounts.importData).not.toHaveBeenCalled()
  })

  it('不是导出数据的 JSON 按文件名拒绝', async () => {
    const { adminAPI } = await import('@/api/admin')
    const wrapper = mountModal()

    const input = wrapper.find('input[type="file"]')
    setInputFiles(input.element, [makeJsonFile('random.json', JSON.stringify({ name: 'test' }))])

    await input.trigger('change')
    await wrapper.find('form').trigger('submit')
    await flushPromises()

    expect(showError).toHaveBeenCalledWith('admin.accounts.dataImportInvalidFile')
    expect(adminAPI.accounts.importData).not.toHaveBeenCalled()
  })

  it('无有效 JSON 的选择不清空已有选择', async () => {
    const { adminAPI } = await import('@/api/admin')
    vi.mocked(adminAPI.accounts.importData).mockResolvedValue({
      proxy_created: 0,
      proxy_reused: 0,
      proxy_failed: 0,
      account_created: 1,
      account_failed: 0
    })

    const wrapper = mountModal()
    const input = wrapper.find('input[type="file"]')

    const valid = makeJsonFile(
      'valid.json',
      JSON.stringify({ exported_at: '2026-07-05T00:00:00Z', proxies: [], accounts: [makeAccount('a')] })
    )
    setInputFiles(input.element, [valid])
    await input.trigger('change')

    setInputFiles(input.element, [new File(['hello'], 'notes.txt', { type: 'text/plain' })])
    await input.trigger('change')
    expect(showError).toHaveBeenCalledWith('admin.accounts.dataImportSelectFile')

    await wrapper.find('form').trigger('submit')
    await flushPromises()

    const importPayload = vi.mocked(adminAPI.accounts.importData).mock.calls[0]?.[0]
    expect(importPayload).toMatchObject({
      data: expect.objectContaining({
        accounts: [makeAccount('a')]
      }),
      skip_default_group_bind: true,
      confirm_overages_risk: false
    })
    expect(importPayload).not.toHaveProperty('codex_429_guard_enabled')
  })

  it('merges multiple selected JSON files before importing', async () => {
    const { adminAPI } = await import('@/api/admin')
    vi.mocked(adminAPI.accounts.importData).mockResolvedValue({
      proxy_created: 0,
      proxy_reused: 0,
      proxy_failed: 0,
      account_created: 2,
      account_failed: 0
    })

    const wrapper = mountModal()

    const input = wrapper.find('input[type="file"]')
    const first = makeJsonFile(
      'first.json',
      JSON.stringify({ exported_at: '2026-07-05T00:00:00Z', proxies: [], accounts: [makeAccount('a')] })
    )
    const second = makeJsonFile(
      'second.json',
      JSON.stringify({
        exported_at: '2026-07-05T00:00:01Z',
        proxies: [{ proxy_key: 'p' }],
        accounts: [makeAccount('b')]
      })
    )
    setInputFiles(input.element, [first, second])

    await input.trigger('change')
    await wrapper.find('form').trigger('submit')
    await flushPromises()

    const importPayload = vi.mocked(adminAPI.accounts.importData).mock.calls[0]?.[0]
    expect(importPayload).toMatchObject({
      data: expect.objectContaining({
        proxies: [{ proxy_key: 'p' }],
        accounts: [makeAccount('a'), makeAccount('b')]
      }),
      skip_default_group_bind: true,
      confirm_overages_risk: false
    })
    expect(importPayload).not.toHaveProperty('codex_429_guard_enabled')
    expect(showSuccess).toHaveBeenCalledWith('admin.accounts.dataImportSuccess')
  })

  it('accepts all supported OpenAI-compatible providers and preserves account metadata', async () => {
    const { adminAPI } = await import('@/api/admin')
    vi.mocked(adminAPI.accounts.importData).mockResolvedValue({
      proxy_created: 0,
      proxy_reused: 0,
      proxy_failed: 0,
      account_created: 3,
      account_failed: 0
    })

    const metadata = {
      chatgpt_account_id: 'team-workspace-1',
      chatgpt_user_id: 'member-1',
      plan_type: 'team',
      k12: true,
      subscription_profile: {
        id: 'subscription-1',
        tier: 'k12'
      }
    }
    const accounts = ['kimi', 'zhipu', 'deepseek'].map((platform) => ({
      ...makeAccount(platform),
      platform,
      credentials: {
        access_token: `${platform}-token`,
        ...metadata
      },
      extra: {
        subscription_profile: metadata.subscription_profile,
        custom_import_marker: platform
      }
    }))

    const wrapper = mountModal()
    const input = wrapper.find('input[type="file"]')
    setInputFiles(input.element, [
      makeJsonFile(
        'providers.json',
        JSON.stringify({ exported_at: '2026-08-13T00:00:00Z', proxies: [], accounts })
      )
    ])

    await input.trigger('change')
    await wrapper.find('form').trigger('submit')
    await flushPromises()

    expect(adminAPI.accounts.importData).toHaveBeenCalledTimes(1)
    const importedAccounts = vi.mocked(adminAPI.accounts.importData).mock.calls[0]?.[0]?.data.accounts
    expect(importedAccounts).toEqual(accounts)
  })

  it('preserves the backup 429 setting unless an explicit import override is selected', async () => {
    const { adminAPI } = await import('@/api/admin')
    vi.mocked(adminAPI.accounts.importData).mockResolvedValue({
      proxy_created: 0,
      proxy_reused: 0,
      proxy_failed: 0,
      account_created: 1,
      account_failed: 0
    })
    const wrapper = mountModal()
    const input = wrapper.find('input[type="file"]')
    setInputFiles(input.element, [
      makeJsonFile('codex.json', JSON.stringify({ exported_at: '2026-08-13T00:00:00Z', proxies: [], accounts: [makeAccount('codex')] }))
    ])
    await input.trigger('change')
    await wrapper.find('form').trigger('submit')
    await flushPromises()

    const importPayload = vi.mocked(adminAPI.accounts.importData).mock.calls[0]?.[0]
    expect(importPayload).not.toHaveProperty('codex_429_guard_enabled')
  })

  it('preserves an explicit Codex fingerprint lifecycle in the submitted import', async () => {
    const { adminAPI } = await import('@/api/admin')
    vi.mocked(adminAPI.accounts.importData).mockResolvedValue({
      proxy_created: 0,
      proxy_reused: 0,
      proxy_failed: 0,
      account_created: 1,
      account_failed: 0
    })
    const wrapper = mountModal()
    const input = wrapper.find('input[type="file"]')
    const legacyAccount = {
      ...makeAccount('legacy-codex'),
      extra: {
        codex_fingerprint_mode: 'full',
        codex_fingerprint_seed: '01234567-89ab-1cde-8fab-0123456789ab',
        openai_device_id: 'legacy-device',
        openai_session_id: 'legacy-session',
        openai_codex_429_guard_enabled: true,
        preserved_setting: 'keep-me'
      }
    }
    setInputFiles(input.element, [
      makeJsonFile(
        'legacy-codex.json',
        JSON.stringify({ exported_at: '2026-08-13T00:00:00Z', proxies: [], accounts: [legacyAccount] })
      )
    ])

    await input.trigger('change')
    await wrapper.find('form').trigger('submit')
    await flushPromises()

    expect(adminAPI.accounts.importData).toHaveBeenCalledTimes(1)
    const extra = vi.mocked(adminAPI.accounts.importData).mock.calls[0]?.[0]?.data.accounts[0]?.extra
    expect(extra).toEqual({
      codex_fingerprint_mode: 'full',
      codex_fingerprint_seed: '01234567-89ab-1cde-8fab-0123456789ab',
      openai_device_id: 'legacy-device',
      openai_session_id: 'legacy-session',
      openai_codex_429_guard_enabled: true,
      preserved_setting: 'keep-me'
    })
  })

  it('rejects malformed or misplaced fingerprint data', async () => {
    const { adminAPI } = await import('@/api/admin')
    vi.mocked(adminAPI.accounts.importData).mockResolvedValue({
      proxy_created: 0,
      proxy_reused: 0,
      proxy_failed: 0,
      account_created: 1,
      account_failed: 0
    })
    const wrapper = mountModal()
    const input = wrapper.find('input[type="file"]')
    const legacyAccount = {
      ...makeAccount('legacy-non-openai'),
      platform: 'anthropic',
      type: 'apikey',
      extra: {
        codex_fingerprint_mode: { stale: true },
        codex_fingerprint_seed: false,
        openai_device_id: ['legacy-device'],
        openai_session_id: { stale: 'session' },
        preserved_setting: 'keep-me'
      }
    }
    setInputFiles(input.element, [
      makeJsonFile(
        'legacy-non-openai.json',
        JSON.stringify({ exported_at: '2026-08-13T00:00:00Z', proxies: [], accounts: [legacyAccount] })
      )
    ])

    await input.trigger('change')
    await wrapper.find('form').trigger('submit')
    await flushPromises()

    expect(adminAPI.accounts.importData).not.toHaveBeenCalled()
    expect(showError).toHaveBeenCalledWith('admin.accounts.dataImportInvalidFile')
  })

  it('sends an explicit 429 override after the import switch is changed', async () => {
    const { adminAPI } = await import('@/api/admin')
    vi.mocked(adminAPI.accounts.importData).mockResolvedValue({
      proxy_created: 0,
      proxy_reused: 0,
      proxy_failed: 0,
      account_created: 1,
      account_failed: 0
    })
    const wrapper = mountModal()
    const input = wrapper.find('input[type="file"]')
    setInputFiles(input.element, [
      makeJsonFile('codex.json', JSON.stringify({ exported_at: '2026-08-13T00:00:00Z', proxies: [], accounts: [makeAccount('codex')] }))
    ])
    await input.trigger('change')
    await flushPromises()
    await wrapper.get('[data-test="codex-429-guard-toggle"]').trigger('click')
    await wrapper.find('form').trigger('submit')
    await flushPromises()

    expect(adminAPI.accounts.importData).toHaveBeenCalledWith(expect.objectContaining({
      codex_429_guard_enabled: true
    }))
  })

  it('rejects invalid Codex 429 guard data before creating a partial import', async () => {
    const { adminAPI } = await import('@/api/admin')
    const wrapper = mountModal()
    const input = wrapper.find('input[type="file"]')
    const invalidAccount = {
      ...makeAccount('invalid-guard'),
      extra: { openai_codex_429_guard_enabled: 'true' }
    }
    setInputFiles(input.element, [
      makeJsonFile('invalid-guard.json', JSON.stringify({ exported_at: '2026-08-13T00:00:00Z', proxies: [], accounts: [invalidAccount] }))
    ])
    await input.trigger('change')
    await wrapper.find('form').trigger('submit')
    await flushPromises()

    expect(showError).toHaveBeenCalledWith('admin.accounts.dataImportInvalidFile')
    expect(adminAPI.accounts.importData).not.toHaveBeenCalled()
  })

  it('部分成功时关闭弹窗仍通知父组件刷新', async () => {
    const { adminAPI } = await import('@/api/admin')
    vi.mocked(adminAPI.accounts.importData).mockResolvedValue({
      proxy_created: 0,
      proxy_reused: 0,
      proxy_failed: 0,
      account_created: 1,
      account_failed: 1
    })

    const wrapper = mountModal()
    const input = wrapper.find('input[type="file"]')
    setInputFiles(input.element, [
      makeJsonFile(
        'mixed.json',
        JSON.stringify({
          exported_at: '2026-07-05T00:00:00Z',
          proxies: [],
          accounts: [makeAccount('a'), makeAccount('b')]
        })
      )
    ])

    await input.trigger('change')
    await wrapper.find('form').trigger('submit')
    await flushPromises()

    expect(showError).toHaveBeenCalledWith('admin.accounts.dataImportCompletedWithErrors')
    expect(wrapper.emitted('imported')).toBeUndefined()

    // 第二个 btn-secondary 是 footer 的取消按钮(第一个是选择文件)
    await wrapper.findAll('button.btn-secondary')[1]!.trigger('click')

    expect(wrapper.emitted('imported')).toHaveLength(1)
    expect(wrapper.emitted('close')).toHaveLength(1)
  })
})
