// Thin fetch wrapper over suchi's HTTP API.
// Auth: session cookie (browser login) or `Authorization: Token <64-hex>`.
// The token, when present, is kept in localStorage so a refresh survives —
// same trust level as the cookie, and it never leaves this origin.
const TOKEN_KEY = 'suchi.token'

export function getToken() { try { return localStorage.getItem(TOKEN_KEY) } catch { return null } }
export function setToken(t) { try { t ? localStorage.setItem(TOKEN_KEY, t) : localStorage.removeItem(TOKEN_KEY) } catch {} }

export class ApiError extends Error {
  constructor(status, code, message, data) { super(message || code || `HTTP ${status}`); this.status = status; this.code = code; this.data = data }
}

export { req }
async function req(method, path, body, opts = {}) {
  const headers = { ...(opts.headers || {}) }
  const token = getToken()
  if (token) headers['Authorization'] = `Token ${token}`
  let payload = body
  if (body !== undefined && !(body instanceof FormData)) {
    headers['Content-Type'] = 'application/json'
    payload = JSON.stringify(body)
  }
  const res = await fetch(path, { method, headers, body: payload, credentials: 'same-origin' })
  if (res.status === 204) return null
  const isJSON = (res.headers.get('content-type') || '').includes('json')
  const data = isJSON ? await res.json().catch(() => null) : null
  if (!res.ok) throw new ApiError(res.status, data?.code, data?.message || data?.detail, data)
  return data
}

export const api = {
  get: (p) => req('GET', p),
  post: (p, b) => req('POST', p, b),
  patch: (p, b) => req('PATCH', p, b),
  put: (p, b) => req('PUT', p, b),
  del: (p) => req('DELETE', p),
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
export const login = (email, password) => api.post('/api/login', { email, password })

export const listDocuments = (params) => api.get(`/api/documents/${qs(params)}`)
export const getDocument = (id) => api.get(`/api/documents/${id}`)
export const patchDocument = (id, body) => api.patch(`/api/documents/${id}`, body)
export const deleteDocument = (id) => api.del(`/api/documents/${id}`)
export const restoreDocument = (id) => api.post(`/api/documents/${id}/restore`)
export const documentVersions = (id) => api.get(`/api/documents/${id}/versions/`)

export const search = (q, params) => api.get(`/api/search/${qs({ q, ...params })}`)
export const autocomplete = (q, limit = 8) => api.get(`/api/autocomplete/${qs({ q, limit })}`)

// JD taxonomy — GET /api/jd/categories/ (flat, DRF envelope).
// results: [{ id, code, name, description, area_code, area_name, system }]
// ?q= prefix on code OR name (case-insensitive) · ?area= area start code (e.g. 20)
export const listJDCategories = (params) => api.get(`/api/jd/categories/${qs({ page_size: 500, ...params })}`)

export const listTags = (params) => api.get(`/api/tags/${qs({ page_size: 500, ...params })}`)
export const listCorrespondents = () => api.get(`/api/correspondents/${qs({ page_size: 500 })}`)
export const listDocumentTypes = () => api.get(`/api/document_types/${qs({ page_size: 500 })}`)

export const listTasks = (params) => api.get(`/api/tasks/${qs(params)}`)
export const resolveApprovalTask = (id, body) => api.post(`/api/approvals/tasks/${id}/resolve`, body)

// Auto-file-from-archive proposals — surfaced in the Tasks inbox as
// heuristics_proposal cards; the SPA bulk bar posts to resolve_bulk.
export const listDocumentProposals = (docId) => api.get(`/api/documents/${docId}/proposals`)
export const resolveProposal = (docId, proposalId, action) =>
  api.post(`/api/documents/${docId}/proposals/${proposalId}/resolve`, { action })
export const resolveBulkProposals = (proposalIds, action) =>
  api.post('/api/proposals/resolve_bulk', { proposal_ids: proposalIds, action })

export const listAutomations = () => api.get('/api/automations/')
export const createAutomation = (b) => api.post('/api/automations/', b)
export const patchAutomation = (id, b) => api.patch(`/api/automations/${id}`, b)
export const deleteAutomation = (id) => api.del(`/api/automations/${id}`)

export const listShareLinks = () => api.get('/api/share_links/')
// Bundles: 1–200 doc ids under one token.
export const createShareLink = ({ doc_ids, label = '', expires_in_sec = 0, password = '' }) =>
  api.post('/api/share_links/', { doc_ids, label, expires_in_sec, password })
export const deleteShareLink = (id) => api.del(`/api/share_links/${id}`)

// One-call dashboard/badge numbers. Counts are visibility-scoped server-side.
export const stats = () => api.get('/api/stats/')

// Activity feed — cursor over audit_events.
// rows: {id, kind, created_at, doc_id?, summary}; response carries latest_id.
export const listEvents = (params) => api.get(`/api/events/${qs(params)}`)

export const listTrash = () => api.get('/api/trash/')

// One transaction, one audit event, N documents. Mobile-compat wire shape.
// methods: set_jd_category{jd_category_id} · set_sensitivity{sensitivity}
//          set_correspondent/document_type/storage_path{*_id}
//          add_tag/remove_tag{tag_id} · delete · restore
export const bulkEdit = (documents, method, parameters) =>
  api.post('/api/documents/bulk_edit', { documents, method, parameters })

export const similarDocs = (id) => api.get(`/api/documents/${id}/similar`)

// Password-protected PDFs waiting for a key.
export const listPendingDecryption = () => api.get('/api/documents/pending-decryption')
export const decryptDocument = (id, b) => api.post(`/api/documents/${id}/decrypt`, b)      // {password, remember?, label?}
export const decryptBatch = (b) => api.post('/api/documents/decrypt-batch', b)             // {password, doc_ids?, remember?, label?}

// ---- admin CRUD ----
export const listGroups = () => api.get('/api/groups/')
export const createGroup = (b) => api.post('/api/groups/', b)
export const deleteGroup = (id) => api.del(`/api/groups/${id}`)
export const groupMembers = (id) => api.get(`/api/groups/${id}/members`)
export const addGroupMember = (id, user_id) => api.post(`/api/groups/${id}/members`, { user_id })
export const removeGroupMember = (id, uid) => api.del(`/api/groups/${id}/members/${uid}`)

export const listCustomFields = () => api.get('/api/custom_fields/')
export const createCustomField = (b) => api.post('/api/custom_fields/', b)   // {name, data_type}
export const patchCustomField = (id, b) => api.patch(`/api/custom_fields/${id}`, b)
export const deleteCustomField = (id) => api.del(`/api/custom_fields/${id}`)

export const createTaxon = (kind, b) => api.post(`/api/${kind}/`, b)         // kind: tags|correspondents|document_types|storage_paths
export const patchTaxon = (kind, id, b) => api.patch(`/api/${kind}/${id}`, b)
export const deleteTaxon = (kind, id) => api.del(`/api/${kind}/${id}`)
export const listStoragePaths = () => api.get(`/api/storage_paths/${qs({ page_size: 500 })}`)

// Mail settings — PROPOSED contract (backend task #141); panel degrades on 404.
export const getMailSettings = () => api.get('/api/admin/settings/mail')
export const putMailSettings = (b) => api.put('/api/admin/settings/mail', b)
export const testMailSettings = (b) => api.post('/api/admin/settings/mail/test', b)
export const thumbPath = (id) => `/api/documents/${id}/thumb`
export const automationsSchema = () => api.get('/api/automations/schema')
export const listJDPresets = () => api.get('/api/jd/presets/')

// Saved views — filter_json is a JSON string of documents-list params
// (e.g. {"tags__id__in":"3","sensitivity":"internal"}). suchi-native shape.
export const listSavedViews = () => api.get('/api/saved_views/')
export const createSavedView = (b) => api.post('/api/saved_views/', b)
export const deleteSavedView = (id) => api.del(`/api/saved_views/${id}`)

// Setup wizard progress (admin only): { completed_at?, steps: {name: status} }
export const setupState = () => api.get('/api/admin/setup/state')

// Profile — PATCH /api/users/me is a *proposed* endpoint (see
// suchi-sweep-2.md); the Settings card degrades gracefully on 404/405.
export const patchMe = (b) => api.patch('/api/users/me', b)
export const uploadAvatar = (file) => {
  const fd = new FormData()
  fd.append('avatar', file)
  return req('POST', '/api/users/me/avatar', fd)
}

export const listTokens = () => api.get('/api/tokens/')
export const createToken = (b) => api.post('/api/tokens/', b)
export const deleteToken = (id) => api.del(`/api/tokens/${id}`)

// ---- authenticated blob access ----
// /preview/{id} and /download/{id} live on the server; iframes and <a>
// clicks cannot carry an Authorization header, and the JSON login path
// issues a token without a session cookie. So the SPA fetches blobs
// itself (header attached) and hands the browser an object URL. Works
// identically with cookie or token auth; zero reliance on the old UI's
// login flow.
export async function fetchBlobURL(path) {
  const headers = {}
  const token = getToken()
  if (token) headers['Authorization'] = `Token ${token}`
  const res = await fetch(path, { headers, credentials: 'same-origin' })
  if (!res.ok) throw new ApiError(res.status, null, `blob ${res.status}`)
  const blob = await res.blob()
  return { url: URL.createObjectURL(blob), type: blob.type, size: blob.size }
}
export const previewPath = (id, reveal) => `/preview/${id}${reveal ? '?reveal=1' : ''}`
export const downloadPath = (id) => `/download/${id}`
export async function downloadToDisk(id, filename) {
  const { url } = await fetchBlobURL(downloadPath(id))
  const a = document.createElement('a')
  a.href = url
  a.download = filename || `document-${id}`
  document.body.appendChild(a); a.click(); a.remove()
  setTimeout(() => URL.revokeObjectURL(url), 30_000)
}

// ---- setup wizard (admin) ----
// Steps: welcome, users, mail, llm, jd, rules, sources, preferences, done.
export const setupStep = (name, status) => api.post(`/api/admin/setup/step/${name}`, { status })  // "done" | "skipped"
export const setupComplete = () => api.post('/api/admin/setup/complete')
export const adminCreateUser = (b) => api.post('/api/admin/users', b)                  // {email,password,display_name,role:"admin"|"member"}
export const applyJDPreset = (b) => api.post('/api/admin/setup/jd-preset', b)          // {preset_id,confirm_blank,refile}
export const saveLLMSettings = (b) => api.post('/api/admin/settings/llm', b)           // {endpoint_url,model,api_key,egress_ack}
export const savePreferences = (b) => api.post('/api/admin/settings/preferences', b)   // {backup_interval_hours,ocr_languages[]}
export const saveIngestSettings = (b) => api.post('/api/admin/settings/ingest', b)     // {fs_watch_dir,fs_watch_owner_email}

export function uploadDocument(file) {
  const fd = new FormData()
  fd.append('document', file)
  return req('POST', '/api/documents/', fd)
}
