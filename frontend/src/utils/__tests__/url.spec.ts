import { describe, expect, it } from 'vitest'

import { sanitizeIframeUrl } from '../url'

describe('sanitizeIframeUrl', () => {
  it('normalizes ordinary HTTP(S) destinations', () => {
    expect(sanitizeIframeUrl('  https://example.com/path?q=1  ')).toBe(
      'https://example.com/path?q=1',
    )
  })

  it.each([
    'javascript:alert(1)',
    'data:text/html,<script>alert(1)</script>',
    '//evil.example/path',
    'https://user:password@example.com/path',
    'https://example.com/path\nheader: value',
  ])('rejects non-HTTP(S), credentialed, or control-character URL: %s', (value) => {
    expect(sanitizeIframeUrl(value)).toBe('')
  })

  it.each([
    'http://localhost/admin',
    'http://127.0.0.1:8080/admin',
    'http://10.0.0.8/internal',
    'http://172.16.0.4/internal',
    'http://192.168.1.4/internal',
    'http://169.254.169.254/latest/meta-data',
    'http://[::1]/admin',
    'http://[::ffff:7f00:1]/admin',
    'http://[::ffff:c0a8:0101]/admin',
    'http://[::ffff:ac10:0101]/admin',
    'http://[::ffff:0a00:0008]/admin',
    'http://[fc00::1]/admin',
    'http://[fe80::1]/admin',
    'http://service.internal/admin',
  ])('rejects private or local destination: %s', (value) => {
    expect(sanitizeIframeUrl(value)).toBe('')
  })

  it('does not reject a public IPv4-mapped IPv6 destination', () => {
    expect(sanitizeIframeUrl('https://[::ffff:0808:0808]/status')).toBe(
      'https://[::ffff:808:808]/status',
    )
  })

  it('allows the current application origin for local deployments', () => {
    const originalLocation = window.location
    Object.defineProperty(window, 'location', {
      value: {
        origin: 'http://localhost:5173',
        href: 'http://localhost:5173/home',
      },
      writable: true,
      configurable: true,
    })

    try {
      expect(sanitizeIframeUrl('http://localhost:5173/custom')).toBe(
        'http://localhost:5173/custom',
      )
    } finally {
      Object.defineProperty(window, 'location', {
        value: originalLocation,
        writable: true,
        configurable: true,
      })
    }
  })
})
