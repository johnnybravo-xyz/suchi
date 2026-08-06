// Hash router: #/documents, #/doc/42, #/search?q=tax …
// Hash-based so the SPA works embedded at any path behind any proxy.
function parse() {
  const raw = location.hash.slice(1) || '/dashboard'
  const [path, query] = raw.split('?')
  const parts = path.split('/').filter(Boolean)
  return { path: '/' + parts.join('/'), parts, query: new URLSearchParams(query || '') }
}

export const route = $state(parse())

window.addEventListener('hashchange', () => Object.assign(route, parse()))

export function go(hash) { location.hash = hash }
