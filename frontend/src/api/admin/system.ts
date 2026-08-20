/**
 * System API endpoints for admin operations
 */

import { apiClient } from '../client'

export interface ReleaseInfo {
  name: string
  body: string
  published_at: string
  html_url: string
}

export interface InPlaceUpdateCapability {
  supported: boolean
  restriction_message?: string
}

export interface VersionInfo {
  current_version: string
  latest_version: string
  has_update: boolean
  release_info?: ReleaseInfo
  cached: boolean
  warning?: string
  build_type: 'source' | 'dev' | 'release'
  update_repo: string
  // Absent on older backends. The UI must treat an absent capability as
  // unsupported so a browser never attempts to replace its own binary.
  in_place_update?: InPlaceUpdateCapability
}

/**
 * Get current version
 */
export async function getVersion(): Promise<{ version: string }> {
  const { data } = await apiClient.get<{ version: string }>('/admin/system/version')
  return data
}

/**
 * Check for updates
 * @param force - Force refresh from GitHub API
 */
export async function checkUpdates(force = false): Promise<VersionInfo> {
  const { data } = await apiClient.get<VersionInfo>('/admin/system/check-updates', {
    params: force ? { force: 'true' } : undefined
  })
  return data
}

export interface UpdateResult {
  message: string
  need_restart: boolean
  already_up_to_date?: boolean
  current_version?: string
  latest_version?: string
}

export interface RollbackVersionInfo {
  version: string
  published_at: string
  html_url: string
}

/**
 * Get versions available for rollback (up to 3 versions older than current)
 */
export async function getRollbackVersions(): Promise<{ versions: RollbackVersionInfo[] }> {
  const { data } = await apiClient.get<{ versions: RollbackVersionInfo[] }>(
    '/admin/system/rollback-versions'
  )
  return data
}

/**
 * In-place update/rollback downloads a full release binary from GitHub, which
 * can take several minutes on slow links. The global 30s axios timeout would
 * abort the request mid-download (#4504), so these calls wait as long as the
 * backend allows (15 minutes server-side).
 */
const UPDATE_REQUEST_TIMEOUT_MS = 15 * 60 * 1000

export interface SystemOperationOptions {
  /**
   * Supplying a key is useful for callers that deliberately retry one logical
   * operation. Axios authentication retries reuse the same request config and
   * therefore the same key automatically.
   */
  idempotencyKey?: string
}

function createSystemOperationIdempotencyKey(operation: string): string {
  const randomUUID = globalThis.crypto?.randomUUID
  const unique =
    typeof randomUUID === 'function'
      ? randomUUID.call(globalThis.crypto)
      : `${Date.now().toString(36)}-${Math.random().toString(36).slice(2)}`
  return `system-${operation}-${unique}`
}

function systemOperationHeaders(operation: string, options?: SystemOperationOptions) {
  return {
    'Idempotency-Key': options?.idempotencyKey || createSystemOperationIdempotencyKey(operation)
  }
}

/**
 * Perform system update
 * Downloads and applies the latest version
 */
export async function performUpdate(options?: SystemOperationOptions): Promise<UpdateResult> {
  const { data } = await apiClient.post<UpdateResult>('/admin/system/update', undefined, {
    timeout: UPDATE_REQUEST_TIMEOUT_MS,
    headers: systemOperationHeaders('update', options)
  })
  return data
}

/**
 * Rollback to a previous version
 * @param version - Target version (e.g. "0.1.146"); omit to restore the local backup binary
 */
export async function rollback(
  version?: string,
  options?: SystemOperationOptions
): Promise<UpdateResult> {
  const { data } = await apiClient.post<UpdateResult>(
    '/admin/system/rollback',
    version ? { version } : undefined,
    {
      timeout: UPDATE_REQUEST_TIMEOUT_MS,
      headers: systemOperationHeaders('rollback', options)
    }
  )
  return data
}

/**
 * Restart the service
 */
export async function restartService(options?: SystemOperationOptions): Promise<{ message: string }> {
  const { data } = await apiClient.post<{ message: string }>('/admin/system/restart', undefined, {
    headers: systemOperationHeaders('restart', options)
  })
  return data
}

export const systemAPI = {
  getVersion,
  checkUpdates,
  performUpdate,
  getRollbackVersions,
  rollback,
  restartService
}

export default systemAPI
