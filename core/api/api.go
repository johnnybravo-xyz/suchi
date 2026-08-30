// Package api owns the JSON HTTP surface — endpoints under /api/*.
//
// This is where machine clients (mobile apps, ingest producers,
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
	suchicrypto "github.com/johnnybravo-xyz/suchi/core/crypto"
	"github.com/johnnybravo-xyz/suchi/core/db"
	"github.com/johnnybravo-xyz/suchi/core/ingest/emailwatch/oauth"
	"github.com/johnnybravo-xyz/suchi/core/jobs"
)

// LLMSettingsStatus is the masked admin-facing classifier state. API keys are
// represented only by HasAPIKey and never cross back to the browser.
type LLMSettingsStatus struct {
	Enabled             bool    `json:"enabled"`
	Active              bool    `json:"active"`
	EndpointURL         string  `json:"endpoint_url"`
	Model               string  `json:"model"`
	EgressAck           bool    `json:"egress_ack"`
	HasAPIKey           bool    `json:"has_api_key"`
	ConfidenceThreshold float64 `json:"confidence_threshold"`
	ArchiveEnabled      bool    `json:"archive_enabled"`
	ArchiveAuto         float64 `json:"archive_auto_threshold"`
	ArchiveReview       float64 `json:"archive_review_threshold"`
}

type LLMTestConfig struct {
	EndpointURL         string
	Model               string
	APIKey              string
	ClearAPIKey         bool
	EgressAck           bool
	ConfidenceThreshold float64
}

type LLMTestResult struct {
	Title         string   `json:"title"`
	Correspondent string   `json:"correspondent"`
	Tags          []string `json:"tags"`
	JDCategory    int      `json:"jd_category"`
	Confidence    float64  `json:"confidence"`
	Language      string   `json:"language,omitempty"`
	ElapsedMS     int64    `json:"elapsed_ms"`
}

type RuntimePreferencesStatus struct {
	BackupIntervalHours int      `json:"backup_interval_hours"`
	OCRLanguages        []string `json:"ocr_languages"`
}

type FSWatchSettingsStatus struct {
	Dir        string `json:"fs_watch_dir"`
	OwnerEmail string `json:"fs_watch_owner_email"`
}

type ChatCompletionMessage struct {
	Role    string
	Content string
}

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
	PublicURL string // validated external origin, wired from config at boot
	decrypt   DecryptDeps
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
	// LLMStatusReader resolves settings plus live plugin state without
	// exposing the API key. LLMTester verifies a candidate configuration
	// against synthetic text and does not persist it.
	LLMStatusReader func(ctx context.Context) (LLMSettingsStatus, error)
	LLMTester       func(ctx context.Context, cfg LLMTestConfig) (LLMTestResult, error)
	// ChatEnabled follows the active runtime model. ChatCompletion is the
	// narrow completion seam; core/api never imports the plugin.
	ChatEnabled     func() bool
	ChatCompletion  func(context.Context, string, []ChatCompletionMessage, int) (string, error)
	ChatRuntimeInfo func() (host string, local bool)
	chatGate        *chatGate
	// LLMAEAD seals API keys written by the setup wizard.
	LLMAEAD *suchicrypto.AEADKey
	// Setup-owned runtime values use readers for honest revisit forms and
	// reloaders so saves affect future work immediately.
	RuntimePreferencesReader   func(context.Context) (RuntimePreferencesStatus, error)
	RuntimePreferencesReloader func(context.Context) error
	FSWatchSettingsReader      func(context.Context) (FSWatchSettingsStatus, error)
	FSWatchReloader            func(context.Context) error
	// TokenIssuer mints a fresh API token for an authenticated user.
	// Wired at boot from the local-auth plugin so the session-authed
	// (cookie / OIDC) caller can mint per-device tokens via
	// /api/tokens/ without password re-entry. Nil disables the
	// endpoint (returns 501).
	TokenIssuer func(ctx context.Context, userID int64, name, scopes string) (string, error)
	// Authz is the permission decision layer. New wires ACLAuthorizer;
	// focused tests may substitute another implementation.
	Authz authz.Authorizer
	// demo holds the demo-mode config surfaced by GET /api/demo/mode.
	// Set via SetDemo at boot; zero value means demo is off.
	demo DemoConfig
	// demoMint mints anonymous demo tokens for POST /api/demo/session.
	// Nil in normal deployments; wired at boot when SUCHI_DEMO_MODE=1.
	demoMint DemoTokenMinter
	// demoScratch provisions a scratch user on upgrade. Nil in normal
	// deployments; wired at boot alongside demoMint.
	demoScratch DemoScratchUserProvisioner
	// EmailwatchReload nudges the IMAP supervisor after any write to
	// email_accounts so the running set matches the DB without a
	// process restart. Nil means no-op (tests / headless smoke).
	EmailwatchReload func(context.Context) error
	// EmailwatchAEAD seals plaintext IMAP passwords + MSAL token caches
	// before they land in email_accounts.sealed_secret. Nil disables
	// every write path that touches sealed_secret.
	EmailwatchAEAD *suchicrypto.AEADKey
	// EmailwatchMSAL drives the Microsoft device-code OAuth flow used
	// by email-accounts /oauth/start + /oauth/complete. Nil disables
	// those endpoints with a 503 (password-auth accounts still work).
	EmailwatchMSAL *oauth.Manager
	oauthFlows     oauthFlowStore
}

// New returns a Server. The zero value isn't runnable — DB, CAS, Log
// are required. Jobs stays nil until wired via WithJobs.
//
// Authz defaults to ACLAuthorizer: owners and admins retain full access, while
// explicit user or group grants can add narrower permissions.
func New(d *db.DB, cas *blob.CAS, log *slog.Logger) (*Server, error) {
	if d == nil || cas == nil || log == nil {
		return nil, errors.New("api.New: DB, CAS, and Log are required")
	}
	return &Server{
		DB:       d,
		CAS:      cas,
		Log:      log.With("component", "api"),
		Authz:    authz.ACLAuthorizer{DB: d},
		chatGate: newChatGate(),
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
	// "Documents like this" uses pure-Go FTS5 more-like-this ranking.
	mux.HandleFunc("GET /api/documents/{id}/similar", s.GetSimilarDocuments)
	// Document correspondents (multi-party per doc).
	mux.HandleFunc("GET /api/documents/{id}/correspondents/", s.ListDocCorrespondents)
	mux.HandleFunc("POST /api/documents/{id}/correspondents/", s.AddDocCorrespondent)
	mux.HandleFunc("DELETE /api/documents/{id}/correspondents/{cid}/{role}", s.RemoveDocCorrespondent)

	// Jobs (the mobile wire-compat surface uses this shape too).
	mux.HandleFunc("GET /api/tasks/", s.ListTasks)
	mux.HandleFunc("POST /api/tasks/{id}/retry", s.RetryDeadJob)
	mux.HandleFunc("POST /api/tasks/{id}/dismiss", s.DismissDeadJob)

	// Public-demo state (unauthenticated on purpose — see demo.go).
	mux.HandleFunc("GET /api/demo/mode", s.GetDemoMode)
	// Anonymous session mint — unauthenticated; returns a stateless
	// signed token. Only meaningful when demo mode is on; otherwise
	// the handler returns 404 demo_disabled.
	mux.HandleFunc("POST /api/demo/session", s.PostDemoSession)
	// Upgrade an anonymous session to a scratch user + real API token.
	// Requires an anon token on the request.
	mux.HandleFunc("POST /api/demo/session/upgrade", s.PostDemoSessionUpgrade)

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
	mux.HandleFunc("GET /api/chat/status", s.GetChatStatus)
	mux.HandleFunc("POST /api/chat", s.PostChat)
	mux.HandleFunc("GET /api/intelligence/schema", s.GetIntelligenceSchema)
	mux.HandleFunc("GET /api/intelligence/", s.ListIntelligence)
	mux.HandleFunc("POST /api/intelligence/extract", s.ExtractIntelligence)
	mux.HandleFunc("POST /api/intelligence/resolve", s.ResolveIntelligence)

	// Language facet — distinct languages present in the archive
	// with per-code doc counts. Powers the search-page facet + doc-
	// detail language chip.
	mux.HandleFunc("GET /api/languages/", s.ListLanguages)

	// Saved views (per-user filter+display presets).
	mux.HandleFunc("GET /api/saved_views/", s.ListSavedViews)
	mux.HandleFunc("POST /api/saved_views/", s.CreateSavedView)
	mux.HandleFunc("PATCH /api/saved_views/{id}", s.UpdateSavedView)
	mux.HandleFunc("DELETE /api/saved_views/{id}", s.DeleteSavedView)

	// Trash listing (soft-deleted docs — owner-scoped for members).
	mux.HandleFunc("GET /api/trash/", s.ListTrash)

	// Share links (creator-facing CRUD).
	mux.HandleFunc("GET /api/share_links/", s.ListShareLinks)
	mux.HandleFunc("POST /api/share_links/", s.CreateShareLink)
	mux.HandleFunc("DELETE /api/share_links/{id}", s.RevokeShareLink)

	// Public share surface. Unauthenticated — token in URL is the
	// primary credential; shares may also require a password.
	mux.HandleFunc("GET /s/{token}", s.GetSharePublic)
	mux.HandleFunc("POST /s/{token}", s.PostSharePublic)
	mux.HandleFunc("GET /s/{token}/{doc_id}/download", s.GetSharePublicDownload)

	// Document versions (chain of previous_version_id).
	mux.HandleFunc("GET /api/documents/{id}/versions/", s.ListVersions)
	mux.HandleFunc("POST /api/documents/{id}/versions/", s.UploadNewVersion)

	// Custom-field values — typed write + delete per (doc, field).
	mux.HandleFunc("PUT /api/documents/{id}/custom_fields/{field}", s.SetCustomField)
	mux.HandleFunc("DELETE /api/documents/{id}/custom_fields/{field}", s.DeleteCustomField)

	// Taxonomy import + export — admin-only. Accepts / emits
	// `suchi-taxonomy/v1` files (HuML / TOML / YAML).
	mux.HandleFunc("POST /api/admin/taxonomy/import", s.ImportTaxonomy)
	mux.HandleFunc("GET /api/admin/taxonomy/export", s.ExportTaxonomy)

	// Email accounts: one row per mailbox the in-process emailwatch
	// poller pulls from. The supervisor Reload runs after every write
	// so the running set matches the DB without a restart. Entry gate
	// is the mailboxes capability — admins see all rows; members see
	// their own only when granted.
	mux.HandleFunc("GET /api/email-accounts", s.ListEmailAccounts)
	mux.HandleFunc("POST /api/email-accounts", s.CreateEmailAccount)
	mux.HandleFunc("GET /api/email-accounts/{id}", s.GetEmailAccount)
	mux.HandleFunc("PATCH /api/email-accounts/{id}", s.PatchEmailAccount)
	mux.HandleFunc("DELETE /api/email-accounts/{id}", s.DeleteEmailAccount)
	mux.HandleFunc("POST /api/email-accounts/{id}/test", s.TestEmailAccount)
	mux.HandleFunc("POST /api/email-accounts/{id}/preview", s.PreviewEmailAccount)
	mux.HandleFunc("POST /api/email-accounts/oauth/start", s.StartEmailAccountOAuth)
	mux.HandleFunc("POST /api/email-accounts/oauth/complete", s.CompleteEmailAccountOAuth)
	mux.HandleFunc("POST /api/email-accounts/{id}/oauth/revoke", s.RevokeEmailAccountOAuth)

	// Setup wizard surface (admin-only).
	s.registerSetup(mux)

	// Approvals engine surface — routing/sign-off state machines at
	// /api/approvals/*. Distinct from automations below.
	s.registerApprovals(mux)

	// Automations — trigger, conditions, and ordered actions. Named for what
	// they do, not for what any other project called them. See
	// docs/automations.mdx.
	mux.HandleFunc("GET /api/automations/", s.ListAutomations)
	mux.HandleFunc("GET /api/automations/schema", s.GetAutomationSchema)
	mux.HandleFunc("POST /api/automations/", s.CreateAutomation)
	mux.HandleFunc("GET /api/automations/{id}", s.GetAutomation)
	mux.HandleFunc("PATCH /api/automations/{id}", s.UpdateAutomation)
	mux.HandleFunc("DELETE /api/automations/{id}", s.DeleteAutomation)

	// Groups are named user collections. Admin-only writes; any
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

	// Refile — one-shot admin action to re-run automations and re-render every
	// live doc after a preset/template/rule change. See docs/refile.mdx.
	mux.HandleFunc("POST /api/admin/refile", s.Refile)

	// JD taxonomy — read-only listing so pickers and MCP tools can
	// enumerate categories and resolve jd_category_id → label
	// without a second round-trip. Creation is a preset swap.
	mux.HandleFunc("GET /api/jd/categories/", s.ListJDCategories)
	mux.HandleFunc("GET /api/presets/", s.ListPresets)

	// Activity feed. Cursor over audit_events; the SPA and external
	// integrations can read this. See events.go
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
	// Sibling to /api/token/ (direct credential exchange).
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
	if status == http.StatusInternalServerError || status == http.StatusBadGateway {
		if s.Log != nil && msg != "server error" && msg != "internal error" {
			s.Log.Error("api.response_error", "status", status, "code", code, "err", msg)
		}
		if status == http.StatusBadGateway {
			msg = "upstream service error"
		} else {
			msg = "server error"
		}
	}
	s.writeJSON(w, status, errBody{Code: code, Error: msg})
}

type errBody struct {
	Error string `json:"error"`
	Code  string `json:"code"`
}
