import { describe, expect, it } from 'vitest'
import { isAntigravityProTier } from '@/utils/antigravityOverages'

describe('isAntigravityProTier', () => {
  it('recognizes credential and canonical load_code_assist Pro tiers', () => {
    expect(isAntigravityProTier({ tier_id: 'google_ai_pro' })).toBe(true)
    expect(isAntigravityProTier({}, {
      load_code_assist: { paidTier: { id: 'g1-pro-tier' } }
    })).toBe(true)
    expect(isAntigravityProTier({}, {
      load_code_assist: { currentTier: { id: 'g1-pro-tier' } }
    })).toBe(true)
  })

  it('does not classify free or ultra tiers as Pro', () => {
    expect(isAntigravityProTier({}, { load_code_assist: { paidTier: { id: 'free-tier' } } })).toBe(false)
    expect(isAntigravityProTier({}, { load_code_assist: { paidTier: { id: 'g1-ultra-tier' } } })).toBe(false)
  })
})
