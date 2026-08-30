import { sanitizeIframeUrl } from './url'

/**
 * Shared URL builder for iframe-embedded pages.
 * Used by embedded views to build consistent URLs with user_id, theme, lang,
 * ui_mode and source context. Authentication tokens are deliberately not
 * propagated: query strings are observable by the destination server, browser
 * history, proxies and analytics systems.
 */

const EMBEDDED_USER_ID_QUERY_KEY = 'user_id'
const EMBEDDED_THEME_QUERY_KEY = 'theme'
const EMBEDDED_LANG_QUERY_KEY = 'lang'
const EMBEDDED_UI_MODE_QUERY_KEY = 'ui_mode'
const EMBEDDED_UI_MODE_VALUE = 'embedded'
const EMBEDDED_SRC_HOST_QUERY_KEY = 'src_host'
const EMBEDDED_SRC_QUERY_KEY = 'src_url'

export function buildEmbeddedUrl(
  baseUrl: string,
  userId?: number,
  // Kept for positional compatibility with older callers. It is intentionally
  // ignored; bearer tokens must never be placed in an iframe URL.
  _authToken?: string | null,
  theme: 'light' | 'dark' = 'light',
  lang?: string,
): string {
  const safeBaseUrl = sanitizeIframeUrl(baseUrl)
  if (!safeBaseUrl) return ''
  try {
    const url = new URL(safeBaseUrl)
    if (userId) {
      url.searchParams.set(EMBEDDED_USER_ID_QUERY_KEY, String(userId))
    }
    url.searchParams.set(EMBEDDED_THEME_QUERY_KEY, theme)
    if (lang) {
      url.searchParams.set(EMBEDDED_LANG_QUERY_KEY, lang)
    }
    url.searchParams.set(EMBEDDED_UI_MODE_QUERY_KEY, EMBEDDED_UI_MODE_VALUE)
    // Source tracking: let the embedded page know where it's being loaded from
    if (typeof window !== 'undefined') {
      try {
        const source = new URL(window.location.href)
        // Never forward query strings or fragments: OAuth/API tokens have
        // historically appeared there in some deployments.
        source.search = ''
        source.hash = ''
        if (source.origin !== 'null') {
          url.searchParams.set(EMBEDDED_SRC_HOST_QUERY_KEY, source.origin)
          url.searchParams.set(EMBEDDED_SRC_QUERY_KEY, `${source.origin}${source.pathname}`)
        }
      } catch {
        // Source context is optional; keep the validated destination usable.
      }
    }
    return url.toString()
  } catch {
    return ''
  }
}

export function detectTheme(): 'light' | 'dark' {
  if (typeof document === 'undefined') return 'light'
  return document.documentElement.classList.contains('dark') ? 'dark' : 'light'
}
