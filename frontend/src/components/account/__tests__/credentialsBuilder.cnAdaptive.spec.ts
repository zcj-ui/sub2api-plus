import { describe, expect, it } from 'vitest'

import {
  defaultCNAdaptiveBaseUrls,
  validateZhipuTeamIDs,
  ZHIPU_TEAM_ID_MAX_LENGTH
} from '../credentialsBuilder'

describe('defaultCNAdaptiveBaseUrls', () => {
  it('resolves Kimi endpoints by account mode', () => {
    expect(defaultCNAdaptiveBaseUrls('kimi', 'payg')).toEqual({
      chat_completions: 'https://api.moonshot.cn/v1',
      anthropic: 'https://api.moonshot.cn/anthropic',
      responses: ''
    })
    expect(defaultCNAdaptiveBaseUrls('kimi', 'coding')).toEqual({
      chat_completions: 'https://api.kimi.com/coding/v1',
      anthropic: 'https://api.kimi.com/coding',
      responses: ''
    })
  })

  it('resolves GLM endpoints by account mode', () => {
    expect(defaultCNAdaptiveBaseUrls('zhipu', 'payg')).toEqual({
      chat_completions: 'https://open.bigmodel.cn/api/paas/v4',
      anthropic: 'https://open.bigmodel.cn/api/anthropic',
      responses: ''
    })
    expect(defaultCNAdaptiveBaseUrls('zhipu', 'coding')).toEqual({
      chat_completions: 'https://open.bigmodel.cn/api/coding/paas/v4',
      anthropic: 'https://open.bigmodel.cn/api/anthropic',
      responses: ''
    })
  })

  it('includes all three native DeepSeek endpoints', () => {
    expect(defaultCNAdaptiveBaseUrls('deepseek', 'payg')).toEqual({
      chat_completions: 'https://api.deepseek.com',
      anthropic: 'https://api.deepseek.com/anthropic',
      responses: 'https://api.deepseek.com'
    })
  })
})

describe('validateZhipuTeamIDs', () => {
  it('accepts empty personal-plan fields and console IDs', () => {
    expect(validateZhipuTeamIDs('', '')).toBeNull()
    expect(validateZhipuTeamIDs(' org-0E486bA654cF4ceBbA31873c4432D407 ', 'proj_D9637f2f1DE74e57980C70E47d1ea61d')).toBeNull()
    expect(validateZhipuTeamIDs('org_team-01', 'proj_team_01')).toBeNull()
  })

  it('rejects URL/header injection and oversized values', () => {
    expect(validateZhipuTeamIDs('org-good\r\nX-Test: yes', '')).toBe('organizationInvalid')
    expect(validateZhipuTeamIDs('org-good/path', '')).toBe('organizationInvalid')
    expect(validateZhipuTeamIDs('org-good', 'proj-good\nX-Test: yes')).toBe('projectInvalid')
    expect(validateZhipuTeamIDs('org-' + 'a'.repeat(ZHIPU_TEAM_ID_MAX_LENGTH), '')).toBe('organizationInvalid')
    expect(validateZhipuTeamIDs('org-good', 'proj-' + 'a'.repeat(ZHIPU_TEAM_ID_MAX_LENGTH))).toBe('projectInvalid')
  })

  it('requires a project when an organization selects team quota mode', () => {
    expect(validateZhipuTeamIDs('org-team_01', '')).toBe('projectRequired')
    expect(validateZhipuTeamIDs('', 'proj-team_01')).toBeNull()
    expect(validateZhipuTeamIDs('', 'not-a-project')).toBeNull()
  })
})
