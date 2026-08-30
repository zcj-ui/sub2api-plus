<template>
  <BaseDialog
    :show="show"
    :title="t('admin.accounts.inventory.title')"
    width="extra-wide"
    @close="emit('close')"
  >
    <div class="space-y-4" data-testid="account-inventory-modal">
      <p class="text-sm text-gray-600 dark:text-gray-300">
        {{ t('admin.accounts.inventory.hint') }}
      </p>

      <div class="grid grid-cols-2 gap-2 sm:grid-cols-4">
        <div class="rounded-lg bg-emerald-50 px-3 py-2 dark:bg-emerald-950/30">
          <div class="text-xs text-emerald-700 dark:text-emerald-300">{{ t('admin.accounts.inventory.healthy') }}</div>
          <div class="mt-1 text-lg font-semibold text-emerald-800 dark:text-emerald-200">{{ response?.healthy ?? 0 }}</div>
        </div>
        <div class="rounded-lg bg-red-50 px-3 py-2 dark:bg-red-950/30">
          <div class="text-xs text-red-700 dark:text-red-300">{{ t('admin.accounts.inventory.failed') }}</div>
          <div class="mt-1 text-lg font-semibold text-red-800 dark:text-red-200">{{ response?.failed ?? 0 }}</div>
        </div>
        <div class="rounded-lg bg-gray-100 px-3 py-2 dark:bg-dark-800">
          <div class="text-xs text-gray-600 dark:text-gray-300">{{ t('admin.accounts.inventory.skipped') }}</div>
          <div class="mt-1 text-lg font-semibold text-gray-800 dark:text-gray-100">{{ response?.skipped ?? 0 }}</div>
        </div>
        <div class="rounded-lg bg-blue-50 px-3 py-2 dark:bg-blue-950/30">
          <div class="text-xs text-blue-700 dark:text-blue-300">{{ t('admin.accounts.inventory.quotaFetched') }}</div>
          <div class="mt-1 text-lg font-semibold text-blue-800 dark:text-blue-200">{{ response?.quota_fetched ?? 0 }}</div>
        </div>
        <div
          v-if="(response?.request_failed_accounts ?? 0) > 0"
          class="rounded-lg bg-amber-50 px-3 py-2 dark:bg-amber-950/30"
          data-testid="account-inventory-request-failed"
        >
          <div class="text-xs text-amber-700 dark:text-amber-300">{{ t('admin.accounts.inventory.requestFailedAccounts') }}</div>
          <div class="mt-1 text-lg font-semibold text-amber-800 dark:text-amber-200">{{ response?.request_failed_accounts }}</div>
        </div>
      </div>

      <div
        v-if="(response?.request_failed_accounts ?? 0) > 0"
        class="rounded-lg border border-amber-200 bg-amber-50 px-3 py-2 text-xs text-amber-800 dark:border-amber-900/50 dark:bg-amber-950/20 dark:text-amber-200"
        data-testid="account-inventory-partial-hint"
      >
        {{ t('admin.accounts.inventory.partialHint', {
          accounts: response?.request_failed_accounts ?? 0,
          batches: response?.request_failed_batches ?? 0
        }) }}
        <span v-if="response?.request_failed_reason" class="mt-1 block break-words text-amber-700/80 dark:text-amber-300/80">
          {{ response.request_failed_reason }}
        </span>
      </div>

      <div class="overflow-x-auto rounded-lg border border-gray-200 dark:border-dark-700">
        <table class="min-w-[1040px] divide-y divide-gray-200 text-left text-xs dark:divide-dark-700">
          <thead class="bg-gray-50 text-gray-600 dark:bg-dark-800 dark:text-gray-300">
            <tr>
              <th class="px-3 py-2 font-medium">{{ t('admin.accounts.inventory.account') }}</th>
              <th class="px-3 py-2 font-medium">{{ t('admin.accounts.inventory.accountType') }}</th>
              <th class="px-3 py-2 font-medium">{{ t('admin.accounts.inventory.health') }}</th>
              <th class="px-3 py-2 font-medium">{{ t('admin.accounts.inventory.credits') }}</th>
              <th class="px-3 py-2 font-medium">{{ t('admin.accounts.inventory.resetCredits') }}</th>
              <th class="px-3 py-2 font-medium">{{ t('admin.accounts.inventory.fiveHourUsage') }}</th>
              <th class="px-3 py-2 font-medium">{{ t('admin.accounts.inventory.sevenDayUsage') }}</th>
              <th class="px-3 py-2 font-medium">{{ t('admin.accounts.inventory.reason') }}</th>
            </tr>
          </thead>
          <tbody class="divide-y divide-gray-100 bg-white text-gray-700 dark:divide-dark-700 dark:bg-dark-900 dark:text-gray-200">
            <tr v-for="item in response?.results ?? []" :key="item.account_id" :data-account-id="item.account_id">
              <td class="px-3 py-2.5 align-top">
                <div class="max-w-[180px] truncate font-medium text-gray-900 dark:text-white" :title="item.name">
                  {{ item.name || `#${item.account_id}` }}
                </div>
                <div class="mt-0.5 text-gray-400">#{{ item.account_id }}</div>
              </td>
              <td class="whitespace-nowrap px-3 py-2.5 align-top">
                <div>{{ formatPlatform(item.platform) }}</div>
                <div class="mt-0.5 text-gray-400">{{ formatAccountType(item.type) }}</div>
              </td>
              <td class="px-3 py-2.5 align-top">
                <span class="inline-flex rounded-full px-2 py-0.5 font-medium" :class="healthBadgeClass(item)">
                  {{ healthLabel(item) }}
                </span>
              </td>
              <td class="px-3 py-2.5 align-top tabular-nums">
                <template v-if="item.quota">
                  <template v-if="creditBalance(item)">
                    <div class="font-medium text-emerald-700 dark:text-emerald-400">{{ creditBalance(item) }} Credit</div>
                    <div v-if="creditUsdReference(item)" class="mt-0.5 text-gray-400">≈ ${{ creditUsdReference(item) }}</div>
                  </template>
                  <div v-if="item.quota.credits?.unlimited" class="mt-0.5 font-medium text-emerald-700 dark:text-emerald-400">
                    {{ t('admin.accounts.inventory.unlimited') }}
                  </div>
                  <span v-if="!creditBalance(item) && !item.quota.credits?.unlimited" class="text-gray-400">
                    {{ t('admin.accounts.inventory.noData') }}
                  </span>
                </template>
                <span v-else-if="item.platform === 'openai' && item.type === 'apikey'" class="text-gray-500 dark:text-gray-400">
                  {{ t('admin.accounts.inventory.apiKeyNoQuota') }}
                </span>
                <span v-else class="text-gray-400">{{ t('admin.accounts.inventory.noData') }}</span>
              </td>
              <td class="px-3 py-2.5 align-top tabular-nums">
                {{ item.quota?.rate_limit_reset_credits?.available_count ?? '—' }}
              </td>
              <td class="px-3 py-2.5 align-top tabular-nums">
                {{ formatUsageWindow(item, '5h') }}
              </td>
              <td class="px-3 py-2.5 align-top tabular-nums">
                {{ formatUsageWindow(item, '7d') }}
              </td>
              <td class="max-w-[280px] break-words px-3 py-2.5 align-top text-gray-500 dark:text-gray-400">
                {{ item.reason || '—' }}
              </td>
            </tr>
            <tr v-if="!response?.results?.length">
              <td colspan="8" class="px-3 py-8 text-center text-gray-400">
                {{ t('admin.accounts.inventory.empty') }}
              </td>
            </tr>
          </tbody>
        </table>
      </div>
    </div>

    <template #footer>
      <button type="button" class="btn btn-primary" @click="emit('close')">
        {{ t('common.close') }}
      </button>
    </template>
  </BaseDialog>
</template>

<script setup lang="ts">
import { useI18n } from 'vue-i18n'
import BaseDialog from '@/components/common/BaseDialog.vue'
import type {
  AccountInventoryResult,
  BatchAccountInventoryResponse,
  OpenAIRateLimitWindow
} from '@/api/admin/accounts'

defineProps<{
  show: boolean
  response: BatchAccountInventoryResponse | null
}>()

const emit = defineEmits<{
  close: []
}>()

const { t } = useI18n()

const formatPlatform = (platform: string) => platform === 'openai' ? 'OpenAI' : (platform || '—')

const formatAccountType = (type: string) => {
  if (type === 'oauth') return 'OAuth'
  if (type === 'apikey') return 'API Key'
  if (type === 'setup-token') return 'Setup Token'
  return type || '—'
}

const healthLabel = (item: AccountInventoryResult) => {
  if (item.dead) return t('admin.accounts.inventory.failed')
  if (item.healthy) return t('admin.accounts.inventory.healthy')
  return t('admin.accounts.inventory.skipped')
}

const healthBadgeClass = (item: AccountInventoryResult) => {
  if (item.dead) return 'bg-red-100 text-red-700 dark:bg-red-950/40 dark:text-red-300'
  if (item.healthy) return 'bg-emerald-100 text-emerald-700 dark:bg-emerald-950/40 dark:text-emerald-300'
  return 'bg-gray-100 text-gray-600 dark:bg-dark-700 dark:text-gray-300'
}

const creditBalance = (item: AccountInventoryResult) => {
  const balance = item.quota?.credits?.balance
  if (typeof balance === 'number') {
    return Number.isFinite(balance) ? String(balance) : ''
  }
  return typeof balance === 'string' ? balance.trim() : ''
}

const creditUsdReference = (item: AccountInventoryResult) => {
  const balance = Number(creditBalance(item))
  return Number.isFinite(balance) ? (balance / 25).toFixed(2) : ''
}

const usageWindows = (item: AccountInventoryResult): OpenAIRateLimitWindow[] => {
  const rateLimit = item.quota?.rate_limit
  return [rateLimit?.primary_window, rateLimit?.secondary_window]
    .filter((window): window is OpenAIRateLimitWindow => Boolean(window))
}

const findUsageWindow = (item: AccountInventoryResult, period: '5h' | '7d') => {
  const windows = usageWindows(item)
  if (period === '5h') {
    return windows.find(window => window.limit_window_seconds > 0 && window.limit_window_seconds < 24 * 60 * 60)
  }
  return windows.find(window => window.limit_window_seconds >= 24 * 60 * 60)
}

const formatUsageWindow = (item: AccountInventoryResult, period: '5h' | '7d') => {
  const window = findUsageWindow(item, period)
  if (!window || !Number.isFinite(window.used_percent)) return '—'
  return `${window.used_percent.toFixed(1)}%`
}
</script>
