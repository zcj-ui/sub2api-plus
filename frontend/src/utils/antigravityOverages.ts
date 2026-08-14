const asRecord = (value: unknown): Record<string, unknown> | undefined =>
  value && typeof value === 'object' && !Array.isArray(value)
    ? value as Record<string, unknown>
    : undefined

const isProTierValue = (value: unknown) => {
  const normalized = String(value ?? '').trim().toLowerCase()
  if (normalized === 'google_ai_pro' || normalized === 'g1-pro-tier') return true
  return /(^|[-_])pro($|[-_])/.test(normalized)
}

export const isAntigravityProTier = (
  credentials?: Record<string, unknown>,
  extra?: Record<string, unknown>
) => {
  const loadCodeAssist = asRecord(extra?.load_code_assist)
  const paidTier = asRecord(loadCodeAssist?.paidTier)
  const currentTier = asRecord(loadCodeAssist?.currentTier)
  return [
    credentials?.tier_id,
    credentials?.tier,
    extra?.tier_id,
    extra?.gemini_tier,
    paidTier?.id,
    paidTier?.name,
    currentTier?.id,
    currentTier?.name
  ].some(isProTierValue)
}
