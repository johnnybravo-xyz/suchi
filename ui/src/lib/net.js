export function isLocalEndpoint(raw) {
  try {
    return isLocalHost(new URL(raw).hostname)
  } catch {
    return true
  }
}

export function isLocalHost(raw) {
  const host = raw.trim().toLowerCase().replace(/^\[|\]$/g, '')
  if (!host || host === 'localhost' || host.endsWith('.localhost') || host.endsWith('.local')) return true

  if (host.startsWith('::ffff:')) return isLocalHost(host.slice(7))
  if (host.includes(':')) {
    if (host === '::1') return true
    const first = Number.parseInt(host.split(':')[0], 16)
    return (first >= 0xfc00 && first <= 0xfdff) || (first >= 0xfe80 && first <= 0xfebf)
  }

  const parts = host.split('.')
  if (parts.length !== 4 || parts.some((part) => !/^\d+$/.test(part))) return false
  const octets = parts.map(Number)
  if (octets.some((octet, i) => octet > 255 || String(octet) !== parts[i])) return false
  return octets[0] === 127 ||
    octets[0] === 10 ||
    (octets[0] === 172 && octets[1] >= 16 && octets[1] <= 31) ||
    (octets[0] === 192 && octets[1] === 168) ||
    (octets[0] === 169 && octets[1] === 254)
}
