// Auth uses a session cookie or persisted API token. Demo read tokens use a
// separate header and upgrade to an API token on the first write.
const TOKEN_KEY = 'suchi.token'
const DEMO_ANON_KEY = 'suchi.demo.anonToken'

export function getToken() { try { return localStorage.getItem(TOKEN_KEY) } catch { return null } }
export function setToken(t) { try { t ? localStorage.setItem(TOKEN_KEY, t) : localStorage.removeItem(TOKEN_KEY) } catch {} }

export function getDemoAnonToken() { try { return sessionStorage.getItem(DEMO_ANON_KEY) } catch { return null } }
export function setDemoAnonToken(t) { try { t ? sessionStorage.setItem(DEMO_ANON_KEY, t) : sessionStorage.removeItem(DEMO_ANON_KEY) } catch {} }

class ApiError extends Error {
  constructor(status, code, message, data) { super(message || code || `HTTP ${status}`); this.status = status; this.code = code; this.data = data }
}

async function req(method, path, body, opts = {}) {
  const res = await sendOnce(method, path, body, opts)
  // `_noUpgrade` prevents recursion when the upgrade endpoint refuses a token.
  if (res.status === 403 && res.data?.code === 'demo_upgrade_required' && !opts._noUpgrade) {
    const upgraded = await upgradeDemoSession()
    if (upgraded) {
      return req(method, path, body, { ...opts, _noUpgrade: true })
    }
  }
  if (res.status === 204) return null
  if (!res.ok) throw new ApiError(res.status, res.data?.code,
    res.data?.message || res.data?.detail || res.data?.error, res.data)
  return res.data
}

async function sendOnce(method, path, body, opts) {
  const headers = { ...(opts.headers || {}) }
  const token = getToken()
  if (token) {
    headers['Authorization'] = `Token ${token}`
  } else {
    const anon = getDemoAnonToken()
    if (anon) headers['X-Suchi-Demo-Token'] = anon
  }
  let payload = body
  if (body !== undefined && !(body instanceof FormData)) {
    headers['Content-Type'] = 'application/json'
    payload = JSON.stringify(body)
  }
  const r = await fetch(path, { method, headers, body: payload, credentials: 'same-origin', signal: opts.signal })
  const isJSON = (r.headers.get('content-type') || '').includes('json')
  const data = isJSON ? await r.json().catch(() => null) : null
  return { status: r.status, ok: r.ok, data }
}

export async function mintDemoSession() {
  const r = await fetch('/api/demo/session', { method: 'POST' })
  if (!r.ok) return null
  const j = await r.json().catch(() => null)
  if (!j?.token) return null
  setDemoAnonToken(j.token)
  return j
}

export const getDemoMode = () => api.get('/api/demo/mode')

async function upgradeDemoSession() {
  const anon = getDemoAnonToken()
  if (!anon) return null
  const r = await fetch('/api/demo/session/upgrade', {
    method: 'POST',
    headers: { 'X-Suchi-Demo-Token': anon },
  })
  if (!r.ok) return null
  const j = await r.json().catch(() => null)
  if (!j?.token) return null
  setToken(j.token)
  setDemoAnonToken(null)
  return j
}

const api = {
  get: (p) => req('GET', p),
  post: (p, b) => req('POST', p, b),
  patch: (p, b) => req('PATCH', p, b),
  put: (p, b) => req('PUT', p, b),
  del: (p) => req('DELETE', p),
}

export const logout = () => api.post('/api/logout')

const pendingGets = new Map()
function singleFlightGet(path) {
  const current = pendingGets.get(path)
  if (current) return current
  const pending = api.get(path).finally(() => {
    if (pendingGets.get(path) === pending) pendingGets.delete(path)
  })
  pendingGets.set(path, pending)
  return pending
}

function qs(params) {
  const u = new URLSearchParams()
  for (const [k, v] of Object.entries(params || {})) {
    if (v !== undefined && v !== null && v !== '') u.set(k, v)
  }
  const s = u.toString()
  return s ? `?${s}` : ''
}

export const whoami = () => api.get('/api/whoami')
export const login = (email, password) => api.post('/api/login', { email, password })

export const listDocuments = (params) => api.get(`/api/documents/${qs(params)}`)
export const getDocument = (id) => api.get(`/api/documents/${id}`)
export const patchDocument = (id, body) => api.patch(`/api/documents/${id}`, body)
export const deleteDocument = (id) => api.del(`/api/documents/${id}`)
export const restoreDocument = (id) => api.post(`/api/documents/${id}/restore`)
export const documentVersions = (id) => api.get(`/api/documents/${id}/versions/`)

export const search = (q, params) => api.get(`/api/search/${qs({ q, ...params })}`)
export const autocomplete = (q, limit = 8) => api.get(`/api/autocomplete/${qs({ q, limit })}`)
export const listLanguages = () => api.get('/api/languages/')
export const chatStatus = () => api.get('/api/chat/status')
export const askArchive = (body, signal) => req('POST', '/api/chat', body, { signal })
export const intelligenceSchema = () => api.get('/api/intelligence/schema')
export const listIntelligence = (params) => api.get(`/api/intelligence/${qs(params)}`)
export const extractIntelligence = (body) => api.post('/api/intelligence/extract', body)
export const resolveIntelligence = (body) => api.post('/api/intelligence/resolve', body)

// suchi-taxonomy/v1 admin import/export.
export const importTaxonomy = (b) => api.post('/api/admin/taxonomy/import', b)
export async function exportTaxonomy(format = 'huml') {
  const headers = {}
  const t = getToken()
  if (t) headers['Authorization'] = `Token ${t}`
  const r = await fetch(`/api/admin/taxonomy/export?format=${encodeURIComponent(format)}`,
    { headers, credentials: 'same-origin' })
  if (!r.ok) throw new ApiError(r.status, 'export_failed', `export failed (${r.status})`)
  return r.text()
}

export const listJDCategories = (params) => singleFlightGet(`/api/jd/categories/${qs({ page_size: 500, ...params })}`)

export const listTags = (params) => api.get(`/api/tags/${qs({ page_size: 500, ...params })}`)
export const listCorrespondents = () => api.get(`/api/correspondents/${qs({ page_size: 500 })}`)
export const listDocumentTypes = () => api.get(`/api/document_types/${qs({ page_size: 500 })}`)

export const listTasks = (params) => api.get(`/api/tasks/${qs(params)}`)
export const resolveApprovalTask = (id, body) => api.post(`/api/approvals/tasks/${id}/resolve`, body)
export const retryDeadJob = (id) => api.post(`/api/tasks/${id}/retry`)
export const dismissDeadJob = (id) => api.post(`/api/tasks/${id}/dismiss`)

export const listAutomations = () => api.get('/api/automations/')
export const createAutomation = (b) => api.post('/api/automations/', b)
export const patchAutomation = (id, b) => api.patch(`/api/automations/${id}`, b)
export const deleteAutomation = (id) => api.del(`/api/automations/${id}`)

export const listShareLinks = () => api.get('/api/share_links/')
export const createShareLink = ({ doc_ids, label = '', expires_in_sec = 0, password = '' }) =>
  api.post('/api/share_links/', { doc_ids, label, expires_in_sec, password })
export const deleteShareLink = (id) => api.del(`/api/share_links/${id}`)

export const stats = () => api.get('/api/stats/')

export const listTrash = () => api.get('/api/trash/')

export const bulkEdit = (documents, method, parameters) =>
  api.post('/api/documents/bulk_edit', { documents, method, parameters })

export const similarDocs = (id) => api.get(`/api/documents/${id}/similar`)

export const decryptDocument = (id, b) => api.post(`/api/documents/${id}/decrypt`, b)
export const decryptBatch = (b) => api.post('/api/documents/decrypt-batch', b)

// Vault responses expose metadata only, never plaintext passwords.
export const listDecryptionPasswords = (params) => api.get(`/api/decryption-passwords/${qs(params)}`)
export const renameDecryptionPassword = (id, label) => api.patch(`/api/decryption-passwords/${id}`, { label })
export const deleteDecryptionPassword = (id) => api.del(`/api/decryption-passwords/${id}`)

// ---- admin CRUD ----
export const listGroups = () => api.get('/api/groups/')
export const createGroup = (b) => api.post('/api/groups/', b)
export const deleteGroup = (id) => api.del(`/api/groups/${id}`)
export const groupMembers = (id) => api.get(`/api/groups/${id}/members`)
export const addGroupMember = (id, user_id) => api.post(`/api/groups/${id}/members`, { user_id })
export const removeGroupMember = (id, uid) => api.del(`/api/groups/${id}/members/${uid}`)

export const listGrants = (kind, id) => api.get(`/api/acls/${kind}/${id}`)
export const putGrant = (kind, id, body) => api.put(`/api/acls/${kind}/${id}`, body)
export const deleteGrant = (kind, id, principal_kind, principal_id) =>
  api.del(`/api/acls/${kind}/${id}${qs({ principal_kind, principal_id })}`)

export const listCustomFields = () => api.get('/api/custom_fields/')
export const createCustomField = (b) => api.post('/api/custom_fields/', b)
export const patchCustomField = (id, b) => api.patch(`/api/custom_fields/${id}`, b)
export const deleteCustomField = (id) => api.del(`/api/custom_fields/${id}`)

export const createTaxon = (kind, b) => api.post(`/api/${kind}/`, b)
export const patchTaxon = (kind, id, b) => api.patch(`/api/${kind}/${id}`, b)
export const deleteTaxon = (kind, id) => api.del(`/api/${kind}/${id}`)
export const listStoragePaths = () => api.get(`/api/storage_paths/${qs({ page_size: 500 })}`)

// Mailbox secrets remain sealed server-side; list visibility is capability-scoped.
export const listEmailAccounts = () => api.get('/api/email-accounts')
export const createEmailAccount = (b) => api.post('/api/email-accounts', b)
export const patchEmailAccount = (id, b) => api.patch(`/api/email-accounts/${id}`, b)
export const deleteEmailAccount = (id) => api.del(`/api/email-accounts/${id}`)
export const testEmailAccount = (id) => api.post(`/api/email-accounts/${id}/test`)
export const previewEmailAccount = (id, intake_policy) =>
  api.post(`/api/email-accounts/${id}/preview`, { intake_policy })
export const startEmailOAuth = (provider) => api.post('/api/email-accounts/oauth/start', { provider })
export const completeEmailOAuth = (flow_handle, { account_id, signal } = {}) =>
  req('POST', '/api/email-accounts/oauth/complete',
    account_id ? { flow_handle, account_id } : { flow_handle }, { signal })
export const revokeEmailOAuth = (id) => api.post(`/api/email-accounts/${id}/oauth/revoke`)
export const thumbPath = (id, reveal) => `/api/documents/${id}/thumb${reveal ? '?reveal=1' : ''}`
export const automationsSchema = () => api.get('/api/automations/schema')
export const listPresets = () => api.get('/api/presets/')

export const listSavedViews = (params) => api.get(`/api/saved_views/${qs(params)}`)
export const createSavedView = (b) => api.post('/api/saved_views/', b)
export const deleteSavedView = (id) => api.del(`/api/saved_views/${id}`)

export const setupState = () => singleFlightGet('/api/admin/setup/state')

export const patchMe = (b) => api.patch('/api/users/me', b)
export const uploadAvatar = (file) => {
  const fd = new FormData()
  fd.append('avatar', file)
  return req('POST', '/api/users/me/avatar', fd)
}

export const listTokens = () => api.get('/api/tokens/')
export const createToken = (b) => api.post('/api/tokens/', b)
export const deleteToken = (id) => api.del(`/api/tokens/${id}`)

export const previewPath = (id, reveal) => `/preview/${id}${reveal ? '?reveal=1' : ''}`
export const downloadPath = (id) => `/download/${id}`

// ---- setup wizard (admin) ----
export const setupComplete = () => api.post('/api/admin/setup/complete')
export const saveSetupIntent = (intent) => api.post('/api/admin/setup/intent', { intent })
export const adminCreateUser = (b) => api.post('/api/admin/users', b)
export const adminListUsers = () => api.get('/api/admin/users')
export const adminPatchUser = (id, b) => api.patch(`/api/admin/users/${id}`, b)
export const applyPreset = (b) => api.post('/api/admin/setup/preset', b)
export const getLLMSettings = () => api.get('/api/admin/settings/llm')
export const saveLLMSettings = (b) => api.post('/api/admin/settings/llm', b)
export const saveResearchContextMode = (research_context_mode) =>
  api.patch('/api/admin/settings/llm', { research_context_mode })
export const testLLMSettings = (b) => api.post('/api/admin/settings/llm/test', b)
export const getPreferences = () => api.get('/api/admin/settings/preferences')
export const savePreferences = (b) => api.post('/api/admin/settings/preferences', b)
export const getIngestSettings = () => api.get('/api/admin/settings/ingest')
export const saveIngestSettings = (b) => api.post('/api/admin/settings/ingest', b)

export function uploadDocument(file) {
  const fd = new FormData()
  fd.append('document', file)
  // Preserve source mtime separately from ingestion time.
  if (file?.lastModified) {
    fd.append('source_mtime', String(Math.floor(file.lastModified / 1000)))
  }
  return req('POST', '/api/documents/', fd)
}
