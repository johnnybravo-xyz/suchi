// Curate the complete audit stream into concise Activity drawer rows.
export const HIDDEN_KINDS = new Set([
  'documents.bulk_edit',
  'heuristics.autoapply',
  'documents.rescan',
  'audit.pruned',
  'backup.pruned',
])

// Unknown kinds fall back to readable text instead of blank rows.
const TEMPLATES = {
  'email_account.update':  () => 'Mail account settings updated',
  'dev_admin.provision':   () => 'Dev instance provisioned',
  'auth.login':            () => 'Signed in',
  'auth.reject':           () => 'Failed sign-in attempt',
  'api_token.create':      () => 'API token created',
  'api_token.revoke':      () => 'API token revoked',
  'approval.task_created': () => 'Approval waiting on you',
  'backup.written':        () => 'Snapshot written',
  'backup.disabled':       () => 'Snapshots are disabled',
  'share_link.create':     () => 'Share link created',
  'share_link.revoke':     () => 'Share link revoked',
  'share_link.delete':     () => 'Share link revoked',
}

function fallbackFromKind(kind) {
  if (!kind) return ''
  const spaced = kind.replace(/[._]/g, ' ')
  return spaced.charAt(0).toUpperCase() + spaced.slice(1)
}

function shape(ev, text) {
  return {
    id: ev.id,
    kind: ev.kind,
    doc_id: ev.doc_id,
    created_at: ev.created_at,
    text,
  }
}

export function presentEvent(ev) {
  if (!ev || !ev.kind) return null
  if (HIDDEN_KINDS.has(ev.kind)) return null
  if (ev.kind.endsWith('.list') || ev.kind.endsWith('.get')) return null

  // New ingest pipelines inherit server summaries without UI changes.
  if (ev.kind.startsWith('ingest.')) {
    const text = ev.summary || fallbackFromKind(ev.kind)
    return shape(ev, text)
  }

  const tpl = TEMPLATES[ev.kind]
  if (tpl) return shape(ev, tpl(ev))

  return shape(ev, ev.summary || fallbackFromKind(ev.kind))
}

const COLLAPSE_WINDOW_SEC = 600

// Collapse adjacent same-kind events within ten minutes, preserving order.
export function curateEvents(events) {
  if (!Array.isArray(events)) return []
  const out = []
  for (const raw of events) {
    const p = presentEvent(raw)
    if (!p) continue
    const last = out[out.length - 1]
    if (last && last.kind === p.kind &&
        Math.abs(last.created_at - p.created_at) <= COLLAPSE_WINDOW_SEC) {
      last.count = (last.count ?? 1) + 1
      continue
    }
    out.push(p)
  }
  return out
}
