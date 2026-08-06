export function fmtDate(unix) {
  if (!unix) return ''
  return new Date(unix * 1000).toLocaleDateString(undefined, { year: 'numeric', month: 'short', day: 'numeric' })
}
export function fmtBytes(n) {
  if (!n && n !== 0) return ''
  const units = ['B', 'KB', 'MB', 'GB']
  let i = 0
  while (n >= 1024 && i < units.length - 1) { n /= 1024; i++ }
  return `${n < 10 && i > 0 ? n.toFixed(1) : Math.round(n)} ${units[i]}`
}
export function sensDot(s) {
  return { confidential: 'danger', internal: 'warn', public: 'ok' }[s] || ''
}
