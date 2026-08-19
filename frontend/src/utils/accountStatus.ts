/**
 * Account status values that have a corresponding admin.accounts.status
 * translation. Unknown or missing backend values are rendered as `unknown`
 * instead of being interpolated into an i18n key.
 */
const ACCOUNT_STATUS_TRANSLATION_KEYS = new Set([
  'unknown',
  'active',
  'inactive',
  'disabled',
  'expired',
  'error',
  'healthDead',
  'healthDeadUnknown',
  'cooldown',
  'paused',
  'limited',
  'rateLimited',
  'overloaded',
  'tempUnschedulable',
  'quotaExceeded',
  'unschedulable'
])

export function normalizeAccountStatusTranslationKey(status: unknown): string {
  if (typeof status !== 'string') return 'unknown'

  const normalized = status.trim()
  return ACCOUNT_STATUS_TRANSLATION_KEYS.has(normalized) ? normalized : 'unknown'
}
