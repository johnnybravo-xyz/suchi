import { logout, whoami, setToken, setDemoAnonToken } from './api.js'

export const session = $state({
  user: null,        // { user_id, email, role, ... } | null
  checked: false,    // whoami attempted at least once
  theme: 'light',
})

export async function refreshSession() {
  try { session.user = await whoami() } catch { session.user = null }
  session.checked = true
}

export async function signOut() {
  try {
    await logout()
  } catch {
    // A stale Authorization token prevents cookie fallback in the auth chain.
    setToken(null)
    try { await logout() } catch {}
  }
  setToken(null)
  setDemoAnonToken(null)
  session.user = null
  location.hash = '#/login'
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
