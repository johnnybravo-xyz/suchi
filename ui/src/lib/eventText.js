// Curates rows from GET /api/events/ for the Activity drawer.
// The audit log stays complete on the server; this module decides
// what actually reaches the drawer chrome — hide side-effect kinds
// the operator just performed in the UI, humanize the ones worth
// showing, and collapse rapid same-kind runs into one row.

// Greppable + reviewable. Add a kind here to silence it in the drawer
// without touching the audit table.
export const HIDDEN_KINDS = new Set([
  'documents.bulk_edit',
  'heuristics.autoapply',
  'documents.rescan',
  'audit.pruned',
  'backup.pruned',
])

// Kind → sentence. Templates receive the raw EventRow so they can
// splice ev.summary in when the server already produced a useful
// string. Anything not in this table falls through to a readable
// form of the kind so new backend kinds degrade to English, not to
// blank drawer rows.
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

  // ingest.* rows already ship a good server-side summary
  // ("Ingested Foo Bar.pdf"). Pass through so newly-added
  // ingest.<pipeline> kinds inherit the behaviour.
  if (ev.kind.startsWith('ingest.')) {
    const text = ev.summary || fallbackFromKind(ev.kind)
    return shape(ev, text)
  }

  const tpl = TEMPLATES[ev.kind]
  if (tpl) return shape(ev, tpl(ev))

  // Prefer a server-provided summary over the auto-generated fallback
  // when we don't have a template — the summary is usually more
  // specific than "Document update".
  return shape(ev, ev.summary || fallbackFromKind(ev.kind))
}

const COLLAPSE_WINDOW_SEC = 600

// Applies presentEvent to each row, drops nulls, then collapses
// consecutive same-kind rows within 10 minutes into a single row
// with a count field. Input order is preserved (drawer feeds
// newest-first).
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
