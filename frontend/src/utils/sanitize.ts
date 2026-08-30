import DOMPurify from 'dompurify'

/**
 * The home page is rendered from a persisted setting.  Keep this allow-list
 * deliberately small: the setting is useful for branding and documentation,
 * but it must not become an arbitrary application/extension surface.
 *
 * In particular, iframe/script/form/style/SVG content is not part of the HTML
 * mode.  An iframe can still be configured explicitly by entering a validated
 * URL in the dedicated URL mode (which is sandboxed by the view).
 */
const HOME_ALLOWED_TAGS = [
  'a',
  'abbr',
  'article',
  'b',
  'blockquote',
  'br',
  'caption',
  'code',
  'col',
  'colgroup',
  'dd',
  'div',
  'dl',
  'dt',
  'em',
  'figcaption',
  'figure',
  'footer',
  'h1',
  'h2',
  'h3',
  'h4',
  'h5',
  'h6',
  'header',
  'hr',
  'i',
  'img',
  'kbd',
  'li',
  'main',
  'mark',
  'nav',
  'ol',
  'p',
  'pre',
  'q',
  's',
  'section',
  'small',
  'span',
  'strong',
  'sub',
  'sup',
  'table',
  'tbody',
  'td',
  'tfoot',
  'th',
  'thead',
  'time',
  'tr',
  'u',
  'ul',
  'var',
  'wbr',
] as string[]

const HOME_ALLOWED_ATTR = [
  'alt',
  'aria-describedby',
  'aria-hidden',
  'aria-label',
  'aria-labelledby',
  'class',
  'colspan',
  'datetime',
  'decoding',
  'height',
  'href',
  'id',
  'lang',
  'loading',
  'rel',
  'role',
  'rowspan',
  'scope',
  'src',
  'tabindex',
  'target',
  'title',
  'translate',
  'width',
] as string[]

// Keep URI-bearing attributes useful for ordinary links/images while
// excluding javascript:, data:, protocol-relative and other active schemes.
const HOME_ALLOWED_URI_REGEXP = /^(?:(?:https?|mailto|tel):|\/(?!\/)|#)/i

function removeUnsafeHomeUris(sanitized: string): string {
  // DOMPurify intentionally permits data:image/* in a few configurations.
  // Home content does not need data URLs, so perform a second, explicit URI
  // pass to make the policy independent of DOMPurify's protocol heuristics.
  if (typeof DOMParser === 'undefined') return sanitized
  const document = new DOMParser().parseFromString(`<div>${sanitized}</div>`, 'text/html')
  const root = document.body.firstElementChild
  if (!root) return ''
  root.querySelectorAll<HTMLElement>('[href], [src]').forEach((element) => {
    for (const attribute of ['href', 'src']) {
      const value = element.getAttribute(attribute)
      if (value !== null && !HOME_ALLOWED_URI_REGEXP.test(value.trim())) {
        element.removeAttribute(attribute)
      }
    }
  })
  return root.innerHTML
}

/**
 * Sanitize persisted home HTML with a strict, presentation-only policy.
 * This function is intentionally separate from sanitizeSvg: SVG needs a
 * different DOMPurify profile and must never be widened implicitly here.
 */
export function sanitizeHomeHtml(html: string): string {
  if (!html) return ''
  const sanitized = DOMPurify.sanitize(html, {
    ALLOWED_TAGS: HOME_ALLOWED_TAGS,
    ALLOWED_ATTR: HOME_ALLOWED_ATTR,
    ALLOW_DATA_ATTR: false,
    ALLOWED_URI_REGEXP: HOME_ALLOWED_URI_REGEXP,
    FORBID_ATTR: ['style', 'srcset', 'action', 'formaction', 'xlink:href', 'xmlns'],
    FORBID_TAGS: [
      'audio',
      'base',
      'button',
      'embed',
      'form',
      'iframe',
      'input',
      'link',
      'meta',
      'object',
      'option',
      'script',
      'select',
      'source',
      'style',
      'svg',
      'template',
      'textarea',
      'track',
      'video',
    ],
  })
  return removeUnsafeHomeUris(sanitized)
}

export function sanitizeSvg(svg: string): string {
  if (!svg) return ''
  return DOMPurify.sanitize(svg, { USE_PROFILES: { svg: true, svgFilters: true } })
}
