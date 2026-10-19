// SPDX-License-Identifier: AGPL-3.0-or-later

import { scopedHash } from './systems.svelte.js'

// Hash router: #/documents, #/doc/42, #/search?q=tax …
// Hash-based so the SPA works embedded at any path behind any proxy.
function parse() {
  const raw = location.hash.slice(1) || '/dashboard'
  const [path, query] = raw.split('?')
  const parts = path.split('/').filter(Boolean)
  return { path: '/' + parts.join('/'), parts, query: new URLSearchParams(query || '') }
}

export const route = $state(parse())

// The page scrolls inside `.content`, which browsers do not restore on Back.
// Each history entry gets a key; returning to an entry restores its offset once
// the reloaded page is tall enough, and a new entry starts at the top.
const offsets = new Map()
let scroller = null
let entries = 0
let restoring = 0

function enterEntry() {
  const key = history.state?.scrollKey
  restoring++
  if (!key) {
    history.replaceState({ ...history.state, scrollKey: `${Date.now()}-${++entries}` }, '')
    if (scroller) scroller.scrollTop = 0
    return
  }
  const top = offsets.get(key)
  if (!top) return
  const attempt = restoring
  const deadline = performance.now() + 3000
  const apply = () => {
    if (attempt !== restoring || !scroller) return
    if (scroller.scrollHeight - scroller.clientHeight >= top || performance.now() > deadline) {
      scroller.scrollTop = top
      restoring++
    } else requestAnimationFrame(apply)
  }
  requestAnimationFrame(apply)
}

export function scrollMemory(node) {
  scroller = node
  const remember = () => {
    const key = history.state?.scrollKey
    if (key) offsets.set(key, node.scrollTop)
  }
  node.addEventListener('scroll', remember, { passive: true })
  return { destroy() { node.removeEventListener('scroll', remember); if (scroller === node) scroller = null } }
}

enterEntry()
window.addEventListener('hashchange', () => {
  enterEntry()
  Object.assign(route, parse())
})

export function go(hash, { replace = false } = {}) {
  hash = scopedHash(hash)
  // Update route now, not on the later hashchange, so a second change made
  // before that event builds on this one instead of the previous query.
  if (replace) history.replaceState(history.state, '', hash)
  else location.hash = hash
  Object.assign(route, parse())
}
