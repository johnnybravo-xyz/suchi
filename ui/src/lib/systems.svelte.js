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
export async function refreshSystems() {
  const scope = captureScope()
  const { listSystems } = await import('./api.js')
  if (!scopeCurrent(scope)) return
  const result = await listSystems()
  if (!scopeCurrent(scope)) return
  systems.introduced = !!result.introduced
  systems.results = result.results || []
  systems.defaultCode = result.default_system_code || ''
  systems.loaded = true
}

export async function enterRoute(path, query) {
  const requested = query.get('system')
  if (systems.ready && (requested || '') === systems.code) return
  systems.ready = false
  systems.error = ''
  systems.code = requested || ''
  systems.generation++
  const scope = captureScope()
  let code = requested || ''
  try {
    if (!systems.introduced && requested) {
      unavailableSystem()
      return
    }
    if (systems.introduced && !requested) {
      const match = /^\/doc\/([1-9][0-9]*)$/.exec(path)
      if (match) {
        const { getDocumentIntrinsic } = await import('./api.js')
        if (!scopeCurrent(scope)) return
        code = (await getDocumentIntrinsic(match[1])).system_code
      }
      else code = systems.defaultCode || systems.results[0]?.code || ''
      if (!scopeCurrent(scope)) return
      if (code) {
        const params = new URLSearchParams(query)
        params.set('system', code)
        history.replaceState(null, '', `#${path}?${params}`)
        window.dispatchEvent(new HashChangeEvent('hashchange'))
      }
    }
    if (systems.introduced && !systems.results.some(item => item.code === code)) {
      unavailableSystem()
      return
    }
    systems.code = code
    systems.ready = true
  } catch (ex) {
    if (scopeCurrent(scope)) {
      systems.error = ex.message || 'This filing system is unavailable.'
    }
  }
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
  return /^\/(preview|download)\//.test(path) || /^\/api\/(documents|search|autocomplete|languages|chat|intelligence|jd\/categories|tasks|approvals|automations|share_links|stats|trash|decryption-passwords|acls|custom_fields|tags|correspondents|document_types|storage_paths|email-accounts|saved_views|tokens|mobile\/pairing)(\/|\?|$)/.test(path) || /^\/api\/admin\/(taxonomy\/|setup\/(preset|state|complete))/.test(path)
}
