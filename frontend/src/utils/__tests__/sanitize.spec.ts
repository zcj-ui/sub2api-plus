import { describe, expect, it } from 'vitest'

import { sanitizeHomeHtml } from '../sanitize'

describe('sanitizeHomeHtml', () => {
  it('keeps presentation markup and removes active HTML', () => {
    const result = sanitizeHomeHtml(
      '<section id="welcome" class="prose"><h1>Hello</h1><p>World</p>' +
        '<a href="https://example.com" target="_blank">Docs</a>' +
        '<img src="https://cdn.example/logo.png" alt="Logo"></section>' +
        '<script>alert(1)</script><iframe src="https://evil.example"></iframe>' +
        '<form action="https://evil.example"><input name="token"></form>' +
        '<style>body{display:none}</style>',
    )

    expect(result).toContain('<section')
    expect(result).toContain('<h1>Hello</h1>')
    expect(result).toContain('href="https://example.com"')
    expect(result).not.toMatch(/<(?:script|iframe|form|input|style)\b/i)
    expect(result).not.toMatch(/\son\w+\s*=/i)
  })

  it('removes dangerous URI schemes and data attributes', () => {
    const result = sanitizeHomeHtml(
      '<a id="link" href="javascript:alert(1)" data-action="run" onclick="run()">link</a>' +
        '<img src="data:image/svg+xml,<svg onload=alert(1)>x</svg>" data-src="x">',
    )

    expect(result).toContain('<a id="link">link</a>')
    expect(result).not.toContain('javascript:')
    expect(result).not.toContain('data-action')
    expect(result).not.toContain('data-src')
    expect(result).not.toContain('onload')
  })
})
