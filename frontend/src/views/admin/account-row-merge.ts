// Merge an incremental account response without allowing omitted fields from
// a mixed-version/partial endpoint to erase the complete row already rendered.
// Explicit null remains meaningful for nullable account fields and is kept.
export function mergeDefinedAccountFields<T extends Record<string, unknown>>(
  current: T,
  next: Partial<T>,
): T {
  const merged = { ...current }
  for (const [key, value] of Object.entries(next)) {
    if (value !== undefined) {
      merged[key as keyof T] = value as T[keyof T]
    }
  }
  return merged
}
