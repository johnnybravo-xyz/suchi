// Filing context only: server records remain authoritative, never cached here.
export const systems = $state({
  user: null, loaded: false, ready: false, introduced: false,
  results: [], defaultCode: '', code: '', generation: 0, error: '',
})

export function resetSystems(user = null) {
  Object.assign(systems, { user, loaded: false, ready: false, introduced: false,
    results: [], defaultCode: '', code: '', error: '', generation: systems.generation + 1 })
}

export function captureScope() {
  return { user: systems.user, code: systems.code, generation: systems.generation }
}
export function scopeCurrent(scope) {
  return scope.user === systems.user && scope.generation === systems.generation
}
export function unavailableSystem() {
  systems.ready = false
  systems.error = 'This filing system is unavailable. Ask an administrator to grant access.'
  systems.generation++
}

export function scopedURL(path, code = systems.code) {
  if (!code) return path
  const [base, query = ''] = path.split('?')
  const params = new URLSearchParams(query)
  if (!params.has('system')) params.set('system', code)
  return `${base}?${params}`
}
export function scopedHash(hash, code = systems.code) {
  if (!hash.startsWith('#/') || hash.startsWith('#/login')) return hash
  return scopedURL(hash, code)
}
export function selectSystem(code) {
  // Selections drop foreign filters, IDs and pending mutation state.
  systems.ready = false
  systems.generation++
  location.hash = scopedHash('#/dashboard', code)
  window.dispatchEvent(new HashChangeEvent('hashchange'))
}
export function isScopedAPI(path) {
  return /^\/(preview|download)\//.test(path) || /^\/api\/(documents|search|autocomplete|languages|chat|intelligence|jd\/categories|tasks|approvals|automations|share_links|stats|trash|decryption-passwords|acls|custom_fields|tags|correspondents|document_types|storage_paths|email-accounts|saved_views|tokens|mobile\/pairing)(\/|\?|$)/.test(path) || /^\/api\/admin\/(taxonomy\/|setup\/(preset|state))/.test(path)
}
