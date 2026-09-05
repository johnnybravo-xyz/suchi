import { logout, whoami } from './api.js'
import { getLoginPath, usesExternalLogin } from './login.js'

export const session = $state({
  user: null,        // { user_id, email, role, ... } | null
  checked: false,    // whoami attempted at least once
  theme: 'light',
})

export async function refreshSession() {
  const user = session.user
  try {
    const current = await whoami()
    if (session.user !== user) return
    session.user = current
  } catch {
    if (session.user !== user) return
    session.user = null
  }
  session.checked = true
}

export async function signOut() {
  try { await logout() } catch {}
  session.user = null
  if (usesExternalLogin()) {
    location.assign(getLoginPath())
  } else {
    location.hash = '#/login'
  }
}

export function initTheme() {
  const preferred = matchMedia('(prefers-color-scheme: dark)').matches ? 'dark' : 'light'
  let saved = null
  try { saved = localStorage.getItem('suchi.theme') } catch {}
  setTheme(saved || preferred)
}
export function setTheme(t) {
  session.theme = t
  document.documentElement.dataset.theme = t
  try { localStorage.setItem('suchi.theme', t) } catch {}
}
