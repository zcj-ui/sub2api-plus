interface AccountIDRow {
  id: number
}

interface AccountMetadataRow extends AccountIDRow {
  platform: string
  type: string
  parent_account_id?: number | null
}

interface AccountListPage {
  items: AccountIDRow[]
  total: number
  pages?: number
}

type AccountPageFetcher = (
  page: number,
  pageSize: number,
  filters: Record<string, unknown>
) => Promise<AccountListPage>

interface AccountMetadataPage {
  items: AccountMetadataRow[]
  total: number
  pages?: number
}

type AccountMetadataPageFetcher = (
  page: number,
  pageSize: number,
  filters: Record<string, unknown>
) => Promise<AccountMetadataPage>

const SELECT_ALL_PAGE_SIZE = 1000

export async function fetchAllAccountIds(
  fetchPage: AccountPageFetcher,
  filters: Record<string, unknown>
): Promise<number[]> {
  const requestFilters = {
    ...filters,
    lite: '1',
    include_scheduler_score: '0'
  }
  const firstPage = await fetchPage(1, SELECT_ALL_PAGE_SIZE, requestFilters)
  const pageCount = Math.max(
    firstPage.pages ?? 0,
    Math.ceil(firstPage.total / SELECT_ALL_PAGE_SIZE)
  )
  const ids = firstPage.items.map(account => account.id)

  for (let page = 2; page <= pageCount; page++) {
    const result = await fetchPage(page, SELECT_ALL_PAGE_SIZE, requestFilters)
    ids.push(...result.items.map(account => account.id))
  }

  const uniqueIDs = Array.from(new Set(ids))
  if (uniqueIDs.length !== firstPage.total) {
    throw new Error('账号列表结果不完整')
  }
  return uniqueIDs
}

export async function fetchAccountSelectionMetadata(
  fetchPage: AccountMetadataPageFetcher,
  filters: Record<string, unknown>,
  selectedAccountIds?: number[]
): Promise<{ platforms: string[]; types: string[]; hasCredentialShadows: boolean }> {
  const requestFilters = {
    ...filters,
    lite: '1',
    include_scheduler_score: '0'
  }
  const selected = selectedAccountIds ? new Set(selectedAccountIds) : null
  const firstPage = await fetchPage(1, SELECT_ALL_PAGE_SIZE, requestFilters)
  const pageCount = Math.max(
    firstPage.pages ?? 0,
    Math.ceil(firstPage.total / SELECT_ALL_PAGE_SIZE)
  )
  const seen = new Set<number>()
  const platforms = new Set<string>()
  const types = new Set<string>()
  let hasCredentialShadows = false

  const collect = (items: AccountMetadataRow[]) => {
    for (const account of items) {
      if (selected && !selected.has(account.id)) continue
      seen.add(account.id)
      platforms.add(account.platform)
      types.add(account.type)
      hasCredentialShadows ||= account.parent_account_id != null
    }
  }

  collect(firstPage.items)
  for (let page = 2; page <= pageCount; page++) {
    if (selected && seen.size === selected.size) break
    const result = await fetchPage(page, SELECT_ALL_PAGE_SIZE, requestFilters)
    collect(result.items)
  }

  const expected = selected?.size ?? firstPage.total
  if (seen.size !== expected) {
    throw new Error('账号选择元数据不完整')
  }
  return { platforms: [...platforms], types: [...types], hasCredentialShadows }
}
