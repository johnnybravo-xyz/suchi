// Package api owns the JSON HTTP surface — endpoints under /api/*.
//
// This is where machine clients (mobile apps, agents, ingest producers,
// automation scripts) talk to suchi. Everything is JSON in and JSON out;
// no HTML, no template rendering. The UI package handles the browser
// surface.
//
// Register(mux) attaches every route this package owns. Each handler is
// a plain http.HandlerFunc — no framework, no reflection, no middleware
// magic. The auth chain runs upstream via httpx.Authenticate.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"github.com/johnnybravo-xyz/suchi/core/authz"
	"github.com/johnnybravo-xyz/suchi/core/blob"
	"github.com/johnnybravo-xyz/suchi/core/db"
	"github.com/johnnybravo-xyz/suchi/core/jobs"
	"github.com/johnnybravo-xyz/suchi/core/mailsetup"
)

// Server bundles the state every /api handler needs. Constructed once
// at boot; safe for concurrent use.
//
// Jobs is optional — when set, the upload handler nudges the
// dispatcher after enqueuing post-ingest work. Tests can leave it nil.
type Server struct {
	DB        *db.DB
	CAS       *blob.CAS
	Log       *slog.Logger
	Jobs      *jobs.Dispatcher
	decrypt   DecryptDeps
	webhooks  WebhookDeps
	mailSetup mailsetup.Options
	// PasswordHasher is set at boot by main.go from the local-auth
	// plugin so /api/admin/users can hash new passwords without this
	// package importing plugins/*. Nil-check in handlers.
	PasswordHasher func(pw string) (string, error)
	// PasswordVerifier is set at boot by main.go from the local-auth
	// plugin so /api/share_links/ can check password-protected shares
	// without a separate hashing lib.
	PasswordVerifier func(encoded, pw string) error
	// LLMReloader is called after /api/admin/settings/llm writes so the
	// running classifier picks up the new config without a restart.
	// Main.go closes over the plugin instance; api/* doesn't import
	// plugins/*. Nil means the wizard just writes the setting.
	LLMReloader func(ctx context.Context) error
	// TokenIssuer mints a fresh API token for an authenticated user.
	// Wired at boot from the local-auth plugin so the session-authed
	// (cookie / OIDC) caller can mint per-device tokens via
	// /api/tokens/ without password re-entry. Nil disables the
	// endpoint (returns 501).
	TokenIssuer func(ctx context.Context, userID int64, name, scopes string) (string, error)
	// Authz is the permission decision layer. Wired in main.go at
	// boot — defaults to ACLAuthorizer, which is backward-compatible
	// (empty object_acls table falls through to owner+admin). Nil
	// means "no decision layer wired" and handlers fall through to
	// the legacy owner_id check — kept for the test constructor.
	Authz authz.Authorizer
}

// New returns a Server. The zero value isn't runnable — DB, CAS, Log
// are required. Jobs stays nil until wired via WithJobs.
//
// Authz defaults to ACLAuthorizer. That is backward-compatible: an
// empty object_acls table means every non-owner non-admin caller is
// still denied, matching the legacy owner_id-scoped queries. The only
// visible change is that inserting a grant now unlocks access — which
// is the point of Phase 6.
func New(d *db.DB, cas *blob.CAS, log *slog.Logger) (*Server, error) {
	if d == nil || cas == nil || log == nil {
		return nil, errors.New("api.New: DB, CAS, and Log are required")
	}
	return &Server{
		DB:    d,
		CAS:   cas,
		Log:   log.With("component", "api"),
		Authz: authz.ACLAuthorizer{DB: d},
	}, nil
}

// WithJobs attaches a Dispatcher so the upload handler can nudge it
// when a fresh doc row lands. Returns s for chaining.
func (s *Server) WithJobs(disp *jobs.Dispatcher) *Server {
	s.Jobs = disp
	return s
}

// Register attaches every /api route this package owns to mux. Called
// from main.go after the auth chain is wired — httpx.Authenticate runs
// upstream, so handlers here can rely on auth.FromContext.
//
// Not every /api route lives here: blob mirrors
// (GET /api/documents/{id}/preview, .../download) are registered in
// distro/cmd/suchi/main.go because they reuse the ui.Server's
// serveBlob (sensitivity gate, ETag, sandbox CSP). A grep for those
// routes finds them there, not in this file.
func (s *Server) Register(mux *http.ServeMux) {
	// Documents.
	mux.HandleFunc("POST /api/documents/", s.UploadDocument)
	// Paginated list — every SPA list view + third-party client
	// walks the archive through this. See documents_list.go for
	// filter surface + ACL splicing.
	mux.HandleFunc("GET /api/documents/", s.ListDocuments)
	mux.HandleFunc("GET /api/documents/{id}", s.GetDocument)
	mux.HandleFunc("PATCH /api/documents/{id}", s.PatchDocument)
	mux.HandleFunc("DELETE /api/documents/{id}", s.SoftDeleteDocument)
	mux.HandleFunc("POST /api/documents/{id}/restore", s.RestoreDocument)
	// Bulk metadata edit. One tx, one audit event, per-id ACL check
	// with a granular result array. See documents_bulk.go.
	mux.HandleFunc("POST /api/documents/bulk_edit", s.BulkEdit)
	// Page-1 thumbnail — generated at ingest by post-ingest.thumb.
	mux.HandleFunc("GET /api/documents/{id}/thumb", s.GetDocumentThumb)
	// "Documents like this" — pure-Go FTS5 more-like-this today; a
	// vec0 layer stacks on top once sqlite-vec ships (see
	// docs/wishlist/similar-documents.mdx).
	mux.HandleFunc("GET /api/documents/{id}/similar", s.GetSimilarDocuments)
	// Auto-file-from-archive proposals surface (heuristics engine).
	// GET/POST resolve per doc + bulk endpoint the SPA bulk bar uses.
	mux.HandleFunc("GET /api/documents/{id}/proposals", s.ListDocumentProposals)
	mux.HandleFunc("POST /api/documents/{id}/proposals/{proposal_id}/resolve", s.ResolveDocumentProposal)
	mux.HandleFunc("POST /api/proposals/resolve_bulk", s.ResolveBulkProposals)

	// Document correspondents (multi-party per doc).
	mux.HandleFunc("GET /api/documents/{id}/correspondents/", s.ListDocCorrespondents)
	mux.HandleFunc("POST /api/documents/{id}/correspondents/", s.AddDocCorrespondent)
	mux.HandleFunc("DELETE /api/documents/{id}/correspondents/{cid}/{role}", s.RemoveDocCorrespondent)

	// Jobs (the mobile wire-compat surface uses this shape too).
	mux.HandleFunc("GET /api/tasks/", s.ListTasks)

	// Mobile wire-compat handshake — apps hit these on connect.
	mux.HandleFunc("GET /api/remote_version/", s.RemoteVersion)
	mux.HandleFunc("GET /api/next_asn/", s.NextASN)

	// Rules — deterministic classifier config surface.
	mux.HandleFunc("GET /api/rules/", s.ListRules)
	mux.HandleFunc("POST /api/rules/", s.CreateRule)
	mux.HandleFunc("PATCH /api/rules/{id}", s.UpdateRule)
	mux.HandleFunc("DELETE /api/rules/{id}", s.DeleteRule)

	// Tags — read + parent-hierarchy operations.
	mux.HandleFunc("GET /api/tags/", s.ListTags)
	mux.HandleFunc("POST /api/tags/", s.CreateTag)
	mux.HandleFunc("PATCH /api/tags/{id}", s.UpdateTag)
	mux.HandleFunc("DELETE /api/tags/{id}", s.DeleteTag)
	mux.HandleFunc("PATCH /api/tags/{id}/parent", s.SetTagParent)

	// Taxonomy CRUD (correspondents, document_types, storage_paths).
	// Shared shape via core/api/taxonomy_crud.go. Admin-only mutation;
	// any authed user can list/read.
	mux.HandleFunc("GET /api/correspondents/", s.ListCorrespondents)
	mux.HandleFunc("POST /api/correspondents/", s.CreateCorrespondent)
	mux.HandleFunc("PATCH /api/correspondents/{id}", s.UpdateCorrespondent)
	mux.HandleFunc("DELETE /api/correspondents/{id}", s.DeleteCorrespondent)

	mux.HandleFunc("GET /api/document_types/", s.ListDocumentTypes)
	mux.HandleFunc("POST /api/document_types/", s.CreateDocumentType)
	mux.HandleFunc("PATCH /api/document_types/{id}", s.UpdateDocumentType)
	mux.HandleFunc("DELETE /api/document_types/{id}", s.DeleteDocumentType)

	mux.HandleFunc("GET /api/storage_paths/", s.ListStoragePaths)
	mux.HandleFunc("POST /api/storage_paths/", s.CreateStoragePath)
	mux.HandleFunc("PATCH /api/storage_paths/{id}", s.UpdateStoragePath)
	mux.HandleFunc("DELETE /api/storage_paths/{id}", s.DeleteStoragePath)

	// Custom field DEFINITIONS (schema). Per-doc values stay at
	// PUT/DELETE /api/documents/{id}/custom_fields/{field} below.
	mux.HandleFunc("GET /api/custom_fields/", s.ListCustomFieldDefs)
	mux.HandleFunc("POST /api/custom_fields/", s.CreateCustomFieldDef)
	mux.HandleFunc("PATCH /api/custom_fields/{id}", s.UpdateCustomFieldDef)
	mux.HandleFunc("DELETE /api/custom_fields/{id}", s.DeleteCustomFieldDef)

	// Search + autocomplete over FTS5.
	mux.HandleFunc("GET /api/search/", s.Search)
	mux.HandleFunc("GET /api/autocomplete/", s.Autocomplete)

	// Language facet — distinct languages present in the archive
	// with per-code doc counts. Powers the search-page facet + doc-
	// detail language chip.
	mux.HandleFunc("GET /api/languages/", s.ListLanguages)

	// Saved views (per-user filter+display presets).
	mux.HandleFunc("GET /api/saved_views/", s.ListSavedViews)
	mux.HandleFunc("POST /api/saved_views/", s.CreateSavedView)
	mux.HandleFunc("PATCH /api/saved_views/{id}", s.UpdateSavedView)
	mux.HandleFunc("DELETE /api/saved_views/{id}", s.DeleteSavedView)

	// UI settings (opaque per-user JSON blob).
	mux.HandleFunc("GET /api/ui_settings/", s.GetUISettings)
	mux.HandleFunc("PUT /api/ui_settings/", s.PutUISettings)

	// Trash listing (soft-deleted docs — owner-scoped for members).
	mux.HandleFunc("GET /api/trash/", s.ListTrash)

	// Share links (creator-facing CRUD).
	mux.HandleFunc("GET /api/share_links/", s.ListShareLinks)
	mux.HandleFunc("POST /api/share_links/", s.CreateShareLink)
	mux.HandleFunc("DELETE /api/share_links/{id}", s.RevokeShareLink)

	// Public share surface. Unauthenticated — token in URL is the
	// only credential; optional ?password guards it further.
	mux.HandleFunc("GET /s/{token}", s.GetSharePublic)
	mux.HandleFunc("GET /s/{token}/{doc_id}/download", s.GetSharePublicDownload)

	// OpenAPI 3.1 spec. Unauthenticated; documents the surface but
	// every operation still enforces its own auth.
	mux.HandleFunc("GET /api/schema/", s.GetSchema)

	// Document versions (chain of previous_version_id).
	mux.HandleFunc("GET /api/documents/{id}/versions/", s.ListVersions)
	mux.HandleFunc("POST /api/documents/{id}/versions/", s.UploadNewVersion)

	// Custom-field values — typed write + delete per (doc, field).
	mux.HandleFunc("PUT /api/documents/{id}/custom_fields/{field}", s.SetCustomField)
	mux.HandleFunc("DELETE /api/documents/{id}/custom_fields/{field}", s.DeleteCustomField)

	// Agent surface v1 — claim/complete/release + enqueue over the
	// existing tasks endpoint family. See docs/agents.mdx.
	s.RegisterAgent(mux)

	// Mail-setup wizard — admin-only. Endpoint 404s when the wizard
	// isn't configured (MAIL_SETUP_ENV_PATH unset).
	mux.HandleFunc("POST /api/admin/mail-setup", s.MailSetupApply)
	// SPA Admin panel: mail-intake settings for the in-process
	// emailwatch poller (distinct surface from the mail-mbsync
	// sidecar wizard above).
	mux.HandleFunc("GET /api/admin/settings/mail", s.GetMailSettings)
	mux.HandleFunc("PUT /api/admin/settings/mail", s.PutMailSettings)
	mux.HandleFunc("POST /api/admin/settings/mail/test", s.TestMailSettings)

	// Setup wizard surface (admin-only).
	s.registerSetup(mux)

	// Approvals engine surface — routing/sign-off state machines at
	// /api/approvals/*. Distinct from automations below.
	s.registerApprovals(mux)

	// Automations — trigger→conditions→actions rules. Named for what
	// they do, not for what any other project called them. See
	// docs/automations.mdx.
	mux.HandleFunc("GET /api/automations/", s.ListAutomations)
	mux.HandleFunc("GET /api/automations/schema", s.GetAutomationSchema)
	mux.HandleFunc("POST /api/automations/", s.CreateAutomation)
	mux.HandleFunc("GET /api/automations/{id}", s.GetAutomation)
	mux.HandleFunc("PATCH /api/automations/{id}", s.UpdateAutomation)
	mux.HandleFunc("DELETE /api/automations/{id}", s.DeleteAutomation)

	// Groups (Phase 6). Named user collections. Admin-only writes; any
	// authed user can list/get.
	mux.HandleFunc("GET /api/groups/", s.ListGroups)
	mux.HandleFunc("POST /api/groups/", s.CreateGroup)
	mux.HandleFunc("GET /api/groups/{id}", s.GetGroup)
	mux.HandleFunc("PATCH /api/groups/{id}", s.UpdateGroup)
	mux.HandleFunc("DELETE /api/groups/{id}", s.DeleteGroup)
	mux.HandleFunc("GET /api/groups/{id}/members", s.ListGroupMembers)
	mux.HandleFunc("POST /api/groups/{id}/members", s.AddGroupMember)
	mux.HandleFunc("DELETE /api/groups/{id}/members/{uid}", s.RemoveGroupMember)

	// Object ACLs. Per-object permission grants naming a user or group.
	mux.HandleFunc("GET /api/acls/{kind}/{id}", s.ListGrants)
	mux.HandleFunc("PUT /api/acls/{kind}/{id}", s.PutGrant)
	mux.HandleFunc("DELETE /api/acls/{kind}/{id}", s.DeleteGrant)

	// Refile — one-shot admin action to re-run rules + re-render every
	// live doc after a preset/template/rule change. See docs/refile.mdx.
	mux.HandleFunc("POST /api/admin/refile", s.Refile)

	// JD taxonomy — read-only listing so pickers + MCP tools + agents
	// can enumerate categories and resolve jd_category_id → label
	// without a second round-trip. Creation is a preset swap.
	mux.HandleFunc("GET /api/jd/categories/", s.ListJDCategories)
	mux.HandleFunc("GET /api/jd/presets/", s.ListJDPresets)

	// Activity feed. Cursor over audit_events; the SPA drawer and
	// any agent that wants a change stream reads this. See events.go
	// for visibility rules and summary rendering.
	mux.HandleFunc("GET /api/events/", s.ListEvents)

	// Dashboard one-shot counters. Replaces four page_size=1 list
	// probes with one index-backed query set.
	mux.HandleFunc("GET /api/stats/", s.GetStats)

	// User profile — whoami + patch self + avatar. whoami used to
	// live in main.go as a hand-formatted JSON literal; it's a proper
	// handler now so display_name + avatar_url land on the same shape.
	mux.HandleFunc("GET /api/whoami", s.Whoami)
	mux.HandleFunc("PATCH /api/users/me", s.PatchSelf)
	mux.HandleFunc("POST /api/users/me/avatar", s.PostSelfAvatar)
	mux.HandleFunc("GET /api/users/{id}/avatar", s.GetUserAvatar)

	// Self-service API-token management for session/OIDC callers.
	// Sibling to /api/token/ (credential-exchange, mobile-compat).
	mux.HandleFunc("GET /api/tokens/", s.ListTokens)
	mux.HandleFunc("POST /api/tokens/", s.CreateToken)
	mux.HandleFunc("DELETE /api/tokens/{id}", s.DeleteToken)
}

// ---------- shared helpers ----------

// writeJSON writes v as JSON with the given status code. Errors from
// the encoder are logged but otherwise swallowed — by the time we start
// writing the body, we can't send a different status.
func (s *Server) writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		s.Log.Warn("api.writeJSON", "err", err.Error())
	}
}

// writeError emits the canonical error shape:
//
//	{"error": "human message", "code": "snake_case_kind"}
//
// The code is programmatically stable across releases; error text isn't.
// See docs/api.md#errors.
func (s *Server) writeError(w http.ResponseWriter, status int, code, msg string) {
	s.writeJSON(w, status, errBody{Code: code, Error: msg})
}

type errBody struct {
	Error string `json:"error"`
	Code  string `json:"code"`
}
