// SPDX-License-Identifier: AGPL-3.0-or-later

// Request machinery and the endpoints the app shell needs on first paint.
// Everything here ships in the initial bundle; screen-only endpoints belong in
// api.js, which re-exports this module and loads with the lazy screens.

import { systems, captureScope, scopeCurrent, scopedURL, isScopedAPI, unavailableSystem } from './systems.svelte.js'
import { t } from './i18n.svelte.js'

// All browser credentials are HttpOnly cookies, including demo sessions.
let sessionRevision = 0
let demoUpgrade

export function resetSessionRequests() {
  sessionRevision++
  pendingGets.clear()
  demoUpgrade?.controller.abort()
  demoUpgrade = null
}

class ApiError extends Error {
  constructor(status, code, data) {
    const key = status === 401 ? 'apiError.unauthorized'
      : status === 403 ? 'apiError.forbidden'
        : status === 404 ? 'apiError.notFound'
          : status === 409 ? 'apiError.conflict'
            : status === 429 ? 'apiError.rateLimited'
              : status >= 500 ? 'apiError.server'
                : 'apiError.requestFailed'
    // Server text is actionable (validation paths, 503 setup hints). For 500
    // and 502 the server sends only generic text, so the localized
    // status text replaces it.
    super(status !== 500 && status !== 502 && (data?.error || data?.message) || t(key, { status }))
    this.status = status
    this.code = code
    this.data = data
  }
}

export async function req(method, path, body, opts = {}) {
  const revision = sessionRevision
  const scope = captureScope()
  const scoped = isScopedAPI(path)
  if (scoped && !opts.intrinsic) path = scopedURL(path, opts.system ?? scope.code)
  const headers = { ...(opts.headers || {}) }
  let payload = body
  if (body !== undefined && !(body instanceof FormData)) {
    headers['Content-Type'] = 'application/json'
    payload = JSON.stringify(body)
  }
  const r = await fetch(path, { method, headers, body: payload, credentials: 'same-origin', signal: opts.signal })
  const data = r.ok && opts.responseType === 'text'
    ? await r.text()
    : await r.json().catch(() => null)
  if (revision !== sessionRevision || (!opts.keepScope && !scopeCurrent(scope))) {
    throw new DOMException(t('apiError.contextChanged'), 'AbortError')
  }
  if (data?.code === 'system_unavailable' && scoped && scopeCurrent(scope)) {
    // Continuing uploads and pairing cleanup may still target the previous system.
    const target = new URLSearchParams(path.split('?')[1] || '').get('system') || ''
    if (target === scope.code) unavailableSystem()
  }
  // At most one retry, and never after the caller's account changes.
  if (r.status === 403 && data?.code === 'demo_upgrade_required' && !opts._noUpgrade &&
      revision === sessionRevision) {
    const upgraded = await upgradeDemoSession()
    if (upgraded && revision === sessionRevision) {
      return req(method, path, body, { ...opts, _noUpgrade: true })
    }
  }
  if (r.status === 204) return null
  if (!r.ok) {
    const error = new ApiError(r.status, data?.code, data)
    error.retryAfter = Number(r.headers?.get?.('Retry-After')) || 0
    throw error
  }
  return data
}

export const mintDemoSession = () => api.post('/api/demo/session')
// Public server configuration loads while the initial filing context is reset.
export const getDemoMode = () => req('GET', '/api/demo/mode', undefined, { keepScope: true })

function upgradeDemoSession() {
  if (demoUpgrade) return demoUpgrade.promise
  const controller = new AbortController()
  const promise = fetch('/api/demo/session/upgrade', {
    method: 'POST', credentials: 'same-origin', signal: controller.signal,
  }).then(r => r.ok).finally(() => {
    if (demoUpgrade?.promise === promise) demoUpgrade = null
  })
  demoUpgrade = { controller, promise }
  return promise
}

export const api = {
  get: (p, opts) => req('GET', p, undefined, opts),
  post: (p, b) => req('POST', p, b),
  patch: (p, b) => req('PATCH', p, b),
  put: (p, b) => req('PUT', p, b),
  del: (p) => req('DELETE', p),
}

export function logout() {
  resetSessionRequests()
  return api.post('/api/logout')
}

const pendingGets = new Map()
function singleFlightGet(path) {
  path = isScopedAPI(path) ? scopedURL(path) : path
  const key = `${systems.generation}:${path}`
  const current = pendingGets.get(key)
  if (current) return current
  const pending = api.get(path).finally(() => {
    if (pendingGets.get(key) === pending) pendingGets.delete(key)
  })
  pendingGets.set(key, pending)
  return pending
}

export function qs(params) {
  const u = new URLSearchParams()
  for (const [k, v] of Object.entries(params || {})) {
    if (v !== undefined && v !== null && v !== '') u.set(k, v)
  }
  const s = u.toString()
  return s ? `?${s}` : ''
}

export const whoami = () => api.get('/api/whoami')
export const listSystems = () => api.get('/api/jd/systems')
export const getDocumentIntrinsic = (id) => req('GET', `/api/documents/${id}`, undefined, { intrinsic: true })
export const resolveAddress = (address) => api.get(`/api/jd/resolve${qs({ address })}`)
export const listDocuments = (params, signal) => api.get(`/api/documents/${qs(params)}`, { signal })
export const chatStatus = () => api.get('/api/chat/status')
export const listJDCategories = (params) => singleFlightGet(`/api/jd/categories/${qs({ page_size: 500, ...params })}`)
export const stats = () => api.get('/api/stats/')
export const setupState = () => singleFlightGet('/api/admin/setup/state')
