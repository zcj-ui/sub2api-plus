import { describe, expect, it } from 'vitest'

import { normalizeAccountStatusTranslationKey } from '../accountStatus'

describe('normalizeAccountStatusTranslationKey', () => {
  it('preserves statuses with an account status translation', () => {
    expect(normalizeAccountStatusTranslationKey('active')).toBe('active')
    expect(normalizeAccountStatusTranslationKey('healthDead')).toBe('healthDead')
  })

  it('maps missing and unsupported values to unknown', () => {
    expect(normalizeAccountStatusTranslationKey(undefined)).toBe('unknown')
    expect(normalizeAccountStatusTranslationKey(null)).toBe('unknown')
    expect(normalizeAccountStatusTranslationKey('')).toBe('unknown')
    expect(normalizeAccountStatusTranslationKey('undefined')).toBe('unknown')
    expect(normalizeAccountStatusTranslationKey('mystery')).toBe('unknown')
    expect(normalizeAccountStatusTranslationKey(429)).toBe('unknown')
  })

  it('trims values received from loosely typed APIs', () => {
    expect(normalizeAccountStatusTranslationKey('  inactive  ')).toBe('inactive')
  })
})
