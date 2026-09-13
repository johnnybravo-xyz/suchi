import { getDocumentIntrinsic, listSystems } from './api.js'
import { systems, captureScope, scopeCurrent, unavailableSystem } from './systems.svelte.js'

export async function refreshSystems() {
  const scope = captureScope()
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
