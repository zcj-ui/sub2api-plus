/**
 * 验证并规范化 URL
 * 默认只接受绝对 URL（以 http:// 或 https:// 开头），可按需允许相对路径
 * @param value 用户输入的 URL
 * @returns 规范化后的 URL，如果无效则返回空字符串
 */
type SanitizeOptions = {
  allowRelative?: boolean
  allowDataUrl?: boolean
}

function hasControlCharacters(value: string): boolean {
  for (const character of value) {
    const code = character.charCodeAt(0)
    if (code <= 0x1f || code === 0x7f) return true
  }
  return false
}

function isPrivateIPv4(hostname: string): boolean {
  const parts = hostname.split('.')
  if (parts.length !== 4 || parts.some((part) => !/^\d+$/.test(part))) return false
  const octets = parts.map((part) => Number(part))
  if (octets.some((octet) => octet < 0 || octet > 255)) return false
  const [a, b] = octets

  // Unspecified, loopback, private, link-local, benchmark, CGNAT and
  // multicast/reserved ranges are not valid targets for an embedded page.
  return (
    a === 0 ||
    a === 10 ||
    a === 127 ||
    (a === 100 && b >= 64 && b <= 127) ||
    (a === 169 && b === 254) ||
    (a === 172 && b >= 16 && b <= 31) ||
    (a === 192 && b === 0) ||
    (a === 192 && b === 168) ||
    (a === 198 && (b === 18 || b === 19)) ||
    a >= 224
  )
}

function parseIPv4Octets(hostname: string): number[] | null {
  const parts = hostname.split('.')
  if (parts.length !== 4 || parts.some((part) => !/^\d+$/.test(part))) return null
  const octets = parts.map((part) => Number(part))
  if (octets.some((octet) => !Number.isInteger(octet) || octet < 0 || octet > 255)) return null
  return octets
}

/**
 * Parse an IPv6 literal without relying on Node's `net` module (this file is
 * also bundled for the browser).  The byte representation lets us recognise
 * IPv4-mapped addresses written in hexadecimal, such as ::ffff:c0a8:0101,
 * which string-prefix checks miss.
 */
function parseIPv6Bytes(hostname: string): number[] | null {
  if (!hostname.includes(':') || hostname.includes('%')) return null

  let value = hostname
  const groups: number[] = []
  const dottedIndex = value.lastIndexOf('.')
  if (dottedIndex >= 0) {
    const colonIndex = value.lastIndexOf(':')
    if (colonIndex < 0) return null
    const octets = parseIPv4Octets(value.slice(colonIndex + 1))
    if (!octets) return null
    groups.push((octets[0] << 8) | octets[1], (octets[2] << 8) | octets[3])
    value = value.slice(0, colonIndex)
  }

  const halves = value.split('::')
  if (halves.length > 2) return null
  const parseGroups = (part: string): number[] | null => {
    if (!part) return []
    const fields = part.split(':')
    const parsed: number[] = []
    for (const field of fields) {
      if (!/^[0-9a-f]{1,4}$/i.test(field)) return null
      parsed.push(Number.parseInt(field, 16))
    }
    return parsed
  }

  const left = parseGroups(halves[0])
  const right = parseGroups(halves.length === 2 ? halves[1] : '')
  if (!left || !right) return null
  const totalWithoutCompression = left.length + right.length + groups.length
  if (halves.length === 1) {
    if (totalWithoutCompression !== 8) return null
    return left.concat(right, groups).flatMap((group) => [group >> 8, group & 0xff])
  }
  const zeroCount = 8 - totalWithoutCompression
  // `::` must stand for at least one group; rejecting zero here also rejects
  // malformed literals with nine explicit groups.
  if (zeroCount < 1) return null
  return left
    .concat(Array.from({ length: zeroCount }, () => 0), right, groups)
    .flatMap((group) => [group >> 8, group & 0xff])
}

function isPrivateIPv6(hostname: string): boolean {
  const bytes = parseIPv6Bytes(hostname)
  if (!bytes) return false

  const allZero = bytes.every((value) => value === 0)
  const loopback = allZero === false && bytes.slice(0, 15).every((value) => value === 0) && bytes[15] === 1
  const first16 = (bytes[0] << 8) | bytes[1]

  // Unspecified/loopback, unique-local (fc00::/7), link-local (fe80::/10),
  // and multicast/reserved destinations are not public iframe targets.
  if (allZero || loopback || (first16 & 0xfe00) === 0xfc00 || (first16 & 0xffc0) === 0xfe80 || (first16 & 0xff00) === 0xff00) {
    return true
  }

  // IPv4-compatible and IPv4-mapped forms can encode a private IPv4 address
  // using either dotted notation or hexadecimal words.  Treat both forms as
  // private when the embedded address falls in a blocked range.
  const mapped = bytes.slice(0, 10).every((value) => value === 0) && bytes[10] === 0xff && bytes[11] === 0xff
  const compatible = bytes.slice(0, 12).every((value) => value === 0)
  if (mapped || compatible) {
    return isPrivateIPv4(bytes.slice(12).join('.'))
  }

  // 6to4 embeds an IPv4 address in the next 32 bits.  Blocking private
  // embedded addresses prevents a trivial tunnel representation bypass.
  if (first16 === 0x2002) {
    const embedded = [bytes[2], bytes[3], bytes[4], bytes[5]].join('.')
    if (isPrivateIPv4(embedded)) return true
  }
  return false
}

function isPrivateHostname(hostname: string): boolean {
  const host = hostname.toLowerCase().replace(/\.$/, '').replace(/^\[|\]$/g, '')
  if (!host) return true
  if (isPrivateIPv4(host)) return true

  // URL.hostname retains IPv6 notation.  Cover loopback, link-local,
  // unique-local and IPv4-mapped private addresses without relying on Node's
  // `net` module (this utility also runs in the browser).
  if (isPrivateIPv6(host)) return true

  // These names are conventionally local-only and can resolve to loopback or
  // private infrastructure.  Public registrable domains remain allowed.
  return (
    host === 'localhost' ||
    host.endsWith('.localhost') ||
    host.endsWith('.local') ||
    host.endsWith('.internal') ||
    host.endsWith('.intranet') ||
    host.endsWith('.home.arpa')
  )
}

/**
 * Validate a URL before placing it in an iframe or an external link that is
 * part of the embedded-page flow.  This is a browser-side defense-in-depth
 * check: server-side URL policies remain authoritative for server requests.
 *
 * The application origin is allowed even when it is a local development host;
 * other private/loopback targets are rejected to avoid turning a public UI
 * setting into a private-network navigation primitive.
 */
export function sanitizeIframeUrl(value: string): string {
  const trimmed = value.trim()
  if (!trimmed || hasControlCharacters(trimmed) || !/^https?:\/\//i.test(trimmed)) {
    return ''
  }

  let parsed: URL
  try {
    parsed = new URL(trimmed)
  } catch {
    return ''
  }

  if (parsed.protocol !== 'http:' && parsed.protocol !== 'https:') return ''
  // Credentials in a configured URL are sent to the target host and are easy
  // to overlook in an admin UI.  Do not permit them in embedded destinations.
  if (parsed.username || parsed.password || !parsed.hostname) return ''

  let sameOrigin = false
  if (typeof window !== 'undefined') {
    try {
      sameOrigin = parsed.origin === window.location.origin
    } catch {
      sameOrigin = false
    }
  }
  if (!sameOrigin && isPrivateHostname(parsed.hostname)) return ''

  return parsed.toString()
}

export function sanitizeUrl(value: string, options: SanitizeOptions = {}): string {
  const trimmed = value.trim()
  if (!trimmed) {
    return ''
  }

  if (options.allowRelative && trimmed.startsWith('/') && !trimmed.startsWith('//')) {
    return trimmed
  }

  // 允许 data:image/ 开头的 data URL（仅限图片类型）
  if (options.allowDataUrl && trimmed.startsWith('data:image/')) {
    return trimmed
  }

  // 只接受绝对 URL，不使用 base URL 来避免相对路径被解析为当前域名
  // 检查是否以 http:// 或 https:// 开头
  if (!trimmed.match(/^https?:\/\//i)) {
    return ''
  }

  try {
    const parsed = new URL(trimmed)
    const protocol = parsed.protocol.toLowerCase()
    if (protocol !== 'http:' && protocol !== 'https:') {
      return ''
    }
    return parsed.toString()
  } catch {
    return ''
  }
}
