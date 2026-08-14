// Admin CRUD + OAuth wiring for the email_accounts table.
//
// Every handler is admin-gated at entry. sealed_secret never appears in
// a response body and is never logged. Password writes accept a
// plaintext `password` field on the wire; the server seals it via
// emailaccounts.SealPassword before storing.
//
// The OAuth start/complete pair holds device-code flows in an in-
// process map keyed by a short random handle. Flows expire five minutes
// after start; expired entries are swept lazily on the next lookup so
// there is no background janitor to reason about.

package api

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	imapclient "github.com/emersion/go-imap/client"

	"github.com/johnnybravo-xyz/suchi/core/audit"
	"github.com/johnnybravo-xyz/suchi/core/auth"
	"github.com/johnnybravo-xyz/suchi/core/emailaccounts"
	"github.com/johnnybravo-xyz/suchi/core/ingest/emailwatch"
	"github.com/johnnybravo-xyz/suchi/core/ingest/emailwatch/oauth"
	"github.com/johnnybravo-xyz/suchi/core/logx"
)

// oauthFlowTTL bounds how long a device-code flow lives in memory
// before the next lookup evicts it. Microsoft's own default is 15 min;
// we clamp shorter because a stalled operator can always start over.
const oauthFlowTTL = 5 * time.Minute

type oauthFlowEntry struct {
	flow      *oauth.Flow
	expiresAt time.Time
}

// oauthFlows is process-global on purpose: /start and /complete run in
// separate requests and must see the same map.
var oauthFlows sync.Map

// emailAccountInput is the wire shape for POST + PATCH bodies. Every
// mutable field is optional; the plaintext Password is server-sealed on
// write and never echoed back.
type emailAccountInput struct {
	Name            *string `json:"name,omitempty"`
	OwnerID         *int64  `json:"owner_id,omitempty"`
	Provider        *string `json:"provider,omitempty"`
	Host            *string `json:"host,omitempty"`
	Port            *int    `json:"port,omitempty"`
	UseTLS          *bool   `json:"use_tls,omitempty"`
	TLSCAFile       *string `json:"tls_ca_file,omitempty"`
	Folder          *string `json:"folder,omitempty"`
	ProcessedFolder *string `json:"processed_folder,omitempty"`
	PollIntervalMin *int    `json:"poll_interval_min,omitempty"`
	AuthMethod      *string `json:"auth_method,omitempty"`
	Username        *string `json:"username,omitempty"`
	Password        *string `json:"password,omitempty"`
	OAuthAccountID  *string `json:"oauth_account_id,omitempty"`
	// SealedSecretB64 carries a pre-sealed MSAL token cache from an
	// /oauth/complete call that ran without an account_id: the SPA
	// holds the bytes for one create request and passes them here so
	// xoauth2 rows can be constructed in a single POST.
	SealedSecretB64 *string `json:"sealed_secret_b64,omitempty"`
	AttachmentsOnly *bool   `json:"attachments_only,omitempty"`
	FromAllowlist   *string `json:"from_allowlist,omitempty"`
	// SyncSince is the unix-seconds initial-sync horizon. On create,
	// omit to default to time.Now() (SPA "add mailbox" only pulls fresh
	// mail). Send 0 to explicitly opt out (sync all UNSEEN). On PATCH,
	// omit = leave alone; 0 = clear (revert to sync-all); positive =
	// set. Watcher maps non-null values onto IMAP SEARCH SINCE.
	SyncSince *int64 `json:"sync_since,omitempty"`
	Enabled   *bool  `json:"enabled,omitempty"`
	// MarkSeen opts into the pre-cursor behaviour: after each successful
	// ingest the watcher STOREs +\Seen on the processed UIDs. Default
	// off — the watcher never touches \Seen so the operator's mail
	// client keeps its own read/unread state.
	MarkSeen *bool `json:"mark_seen,omitempty"`
}

// ---------- list + get + create + patch + delete ----------

// ListEmailAccounts — GET /api/admin/email-accounts.
func (s *Server) ListEmailAccounts(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	rows, err := emailaccounts.List(r.Context(), s.DB)
	if err != nil {
		s.serverErr(w, "email_accounts.list", err)
		return
	}
	if rows == nil {
		rows = []emailaccounts.Account{}
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"accounts": rows})
}

// GetEmailAccount — GET /api/admin/email-accounts/{id}.
func (s *Server) GetEmailAccount(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	id, ok := s.pathID(w, r)
	if !ok {
		return
	}
	acc, err := emailaccounts.Get(r.Context(), s.DB, id)
	if errors.Is(err, sql.ErrNoRows) {
		s.writeError(w, http.StatusNotFound, "not_found", "email account not found")
		return
	}
	if err != nil {
		s.serverErr(w, "email_accounts.get", err)
		return
	}
	s.writeJSON(w, http.StatusOK, acc)
}

// CreateEmailAccount — POST /api/admin/email-accounts.
func (s *Server) CreateEmailAccount(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	var in emailAccountInput
	if err := decodeJSON(r, &in); err != nil {
		s.writeError(w, http.StatusBadRequest, "bad_json", err.Error())
		return
	}

	acc := emailaccounts.Account{}
	if in.Name != nil {
		acc.Name = strings.TrimSpace(*in.Name)
	}
	if in.OwnerID != nil {
		acc.OwnerID = *in.OwnerID
	}
	if in.Provider != nil {
		acc.Provider = emailaccounts.Provider(strings.TrimSpace(*in.Provider))
	}
	if in.Host != nil {
		acc.Host = strings.TrimSpace(*in.Host)
	}
	if in.Port != nil {
		acc.Port = *in.Port
	}
	if in.UseTLS != nil {
		acc.UseTLS = *in.UseTLS
	}
	if in.TLSCAFile != nil {
		acc.TLSCAFile = strings.TrimSpace(*in.TLSCAFile)
	}
	if in.Folder != nil {
		acc.Folder = strings.TrimSpace(*in.Folder)
	}
	if in.ProcessedFolder != nil {
		acc.ProcessedFolder = strings.TrimSpace(*in.ProcessedFolder)
	}
	if in.PollIntervalMin != nil {
		acc.PollIntervalMin = *in.PollIntervalMin
	}
	if in.AuthMethod != nil {
		acc.AuthMethod = emailaccounts.AuthMethod(strings.TrimSpace(*in.AuthMethod))
	}
	if in.Username != nil {
		acc.Username = strings.TrimSpace(*in.Username)
	}
	if in.OAuthAccountID != nil {
		acc.OAuthAccountID = strings.TrimSpace(*in.OAuthAccountID)
	}
	if in.AttachmentsOnly != nil {
		acc.AttachmentsOnly = *in.AttachmentsOnly
	}
	if in.FromAllowlist != nil {
		acc.FromAllowlist = strings.TrimSpace(*in.FromAllowlist)
	}
	// Initial-sync horizon. Omitted → "from now on"; explicit 0 →
	// "sync all UNSEEN" (matches pre-change behaviour for operators
	// who wanted the old default).
	if in.SyncSince == nil {
		nowTS := time.Now().Unix()
		acc.SyncSince = &nowTS
	} else if *in.SyncSince > 0 {
		v := *in.SyncSince
		acc.SyncSince = &v
	}
	if in.Enabled != nil {
		acc.Enabled = *in.Enabled
	}
	if in.MarkSeen != nil {
		acc.MarkSeen = *in.MarkSeen
	}

	// Provider preset auto-fill. User-supplied values already landed in
	// acc; a preset only fills the blanks so operator overrides win.
	if acc.Provider != "" {
		p, ok := emailwatch.Presets[string(acc.Provider)]
		if !ok {
			s.writeError(w, http.StatusBadRequest, "unknown_provider",
				fmt.Sprintf("provider %q is not a known preset", acc.Provider))
			return
		}
		if acc.Host == "" {
			acc.Host = p.Host
		}
		if acc.Port == 0 {
			acc.Port = p.Port
		}
		if in.UseTLS == nil {
			acc.UseTLS = p.UseTLS
		}
		if acc.AuthMethod == "" {
			acc.AuthMethod = emailaccounts.AuthMethod(p.AuthMethod)
		}
	}

	// xoauth2 needs a pre-sealed MSAL cache from /oauth/complete;
	// password mode seals the plaintext here. Either path lands
	// SealedSecret before Create so the NOT NULL column is satisfied.
	if acc.AuthMethod == emailaccounts.AuthXOAuth2 {
		if in.SealedSecretB64 == nil || *in.SealedSecretB64 == "" {
			s.writeError(w, http.StatusBadRequest, "oauth_required",
				"xoauth2 accounts must be created with sealed_secret_b64 from /oauth/complete")
			return
		}
		sealed, err := base64.StdEncoding.DecodeString(*in.SealedSecretB64)
		if err != nil {
			s.writeError(w, http.StatusBadRequest, "bad_sealed_secret",
				"sealed_secret_b64 is not valid base64")
			return
		}
		acc.SealedSecret = sealed
	} else {
		if in.Password == nil || *in.Password == "" {
			s.writeError(w, http.StatusBadRequest, "missing_password",
				"password is required for password-auth accounts")
			return
		}
		if s.EmailwatchAEAD == nil {
			s.writeError(w, http.StatusServiceUnavailable, "no_aead",
				"server AEAD key not configured")
			return
		}
		sealed, err := emailaccounts.SealPassword(s.EmailwatchAEAD, *in.Password)
		if err != nil {
			s.serverErr(w, "email_accounts.seal", err)
			return
		}
		acc.SealedSecret = sealed
	}

	if err := s.validateOwnerID(r.Context(), acc.OwnerID); err != nil {
		s.writeError(w, http.StatusBadRequest, "bad_owner", err.Error())
		return
	}

	created, err := emailaccounts.Create(r.Context(), s.DB, acc)
	if err != nil {
		if strings.HasPrefix(err.Error(), "emailaccounts:") {
			s.writeError(w, http.StatusBadRequest, "validation", err.Error())
			return
		}
		s.serverErr(w, "email_accounts.create", err)
		return
	}

	audit.Log(r.Context(), s.DB, s.Log, audit.Event{
		Actor:      auth.FromContext(r.Context()),
		Action:     "email_account.create",
		ObjectKind: "email_account",
		ObjectID:   created.ID,
		After:      accountAuditView(created),
		RequestID:  logx.RequestID(r.Context()),
	})
	s.reloadEmailwatch(r.Context())
	s.writeJSON(w, http.StatusCreated, created)
}

// PatchEmailAccount — PATCH /api/admin/email-accounts/{id}.
func (s *Server) PatchEmailAccount(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	id, ok := s.pathID(w, r)
	if !ok {
		return
	}
	var in emailAccountInput
	if err := decodeJSON(r, &in); err != nil {
		s.writeError(w, http.StatusBadRequest, "bad_json", err.Error())
		return
	}

	before, err := emailaccounts.Get(r.Context(), s.DB, id)
	if errors.Is(err, sql.ErrNoRows) {
		s.writeError(w, http.StatusNotFound, "not_found", "email account not found")
		return
	}
	if err != nil {
		s.serverErr(w, "email_accounts.get", err)
		return
	}

	patch := emailaccounts.AccountPatch{
		Name:            trimStringPtr(in.Name),
		OwnerID:         in.OwnerID,
		Host:            trimStringPtr(in.Host),
		Port:            in.Port,
		UseTLS:          in.UseTLS,
		TLSCAFile:       trimStringPtr(in.TLSCAFile),
		Folder:          trimStringPtr(in.Folder),
		ProcessedFolder: trimStringPtr(in.ProcessedFolder),
		PollIntervalMin: in.PollIntervalMin,
		Username:        trimStringPtr(in.Username),
		OAuthAccountID:  trimStringPtr(in.OAuthAccountID),
		AttachmentsOnly: in.AttachmentsOnly,
		FromAllowlist:   trimStringPtr(in.FromAllowlist),
		SyncSince:       in.SyncSince,
		Enabled:         in.Enabled,
		MarkSeen:        in.MarkSeen,
	}
	if in.Provider != nil {
		prov := emailaccounts.Provider(strings.TrimSpace(*in.Provider))
		if _, ok := emailwatch.Presets[string(prov)]; !ok {
			s.writeError(w, http.StatusBadRequest, "unknown_provider",
				fmt.Sprintf("provider %q is not a known preset", prov))
			return
		}
		patch.Provider = &prov
	}
	if in.AuthMethod != nil {
		am := emailaccounts.AuthMethod(strings.TrimSpace(*in.AuthMethod))
		patch.AuthMethod = &am
	}

	passwordUpdated := false
	if in.Password != nil && *in.Password != "" {
		if s.EmailwatchAEAD == nil {
			s.writeError(w, http.StatusServiceUnavailable, "no_aead",
				"server AEAD key not configured")
			return
		}
		sealed, err := emailaccounts.SealPassword(s.EmailwatchAEAD, *in.Password)
		if err != nil {
			s.serverErr(w, "email_accounts.seal", err)
			return
		}
		patch.SealedSecret = &sealed
		passwordUpdated = true
	}

	if patch.OwnerID != nil {
		if err := s.validateOwnerID(r.Context(), *patch.OwnerID); err != nil {
			s.writeError(w, http.StatusBadRequest, "bad_owner", err.Error())
			return
		}
	}

	updated, err := emailaccounts.Patch(r.Context(), s.DB, id, patch)
	if errors.Is(err, sql.ErrNoRows) {
		s.writeError(w, http.StatusNotFound, "not_found", "email account not found")
		return
	}
	if err != nil {
		if strings.HasPrefix(err.Error(), "emailaccounts:") {
			s.writeError(w, http.StatusBadRequest, "validation", err.Error())
			return
		}
		s.serverErr(w, "email_accounts.patch", err)
		return
	}

	after := accountAuditView(updated)
	if passwordUpdated {
		after["password_updated"] = true
	}
	audit.Log(r.Context(), s.DB, s.Log, audit.Event{
		Actor:      auth.FromContext(r.Context()),
		Action:     "email_account.update",
		ObjectKind: "email_account",
		ObjectID:   id,
		Before:     accountAuditView(before),
		After:      after,
		RequestID:  logx.RequestID(r.Context()),
	})
	s.reloadEmailwatch(r.Context())
	s.writeJSON(w, http.StatusOK, updated)
}

// DeleteEmailAccount — DELETE /api/admin/email-accounts/{id}.
func (s *Server) DeleteEmailAccount(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	id, ok := s.pathID(w, r)
	if !ok {
		return
	}
	before, err := emailaccounts.Get(r.Context(), s.DB, id)
	if errors.Is(err, sql.ErrNoRows) {
		s.writeError(w, http.StatusNotFound, "not_found", "email account not found")
		return
	}
	if err != nil {
		s.serverErr(w, "email_accounts.get", err)
		return
	}
	if err := emailaccounts.Delete(r.Context(), s.DB, id); err != nil {
		s.serverErr(w, "email_accounts.delete", err)
		return
	}
	audit.Log(r.Context(), s.DB, s.Log, audit.Event{
		Actor:      auth.FromContext(r.Context()),
		Action:     "email_account.delete",
		ObjectKind: "email_account",
		ObjectID:   id,
		Before:     accountAuditView(before),
		RequestID:  logx.RequestID(r.Context()),
	})
	s.reloadEmailwatch(r.Context())
	w.WriteHeader(http.StatusNoContent)
}

// ---------- test dial ----------

// TestEmailAccount — POST /api/admin/email-accounts/{id}/test.
//
// Dials the stored IMAP host, authenticates, logs out. Returns
// {ok, message}. Never 500s on a connect / auth failure — that is user
// data being wrong, not a server bug.
func (s *Server) TestEmailAccount(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	id, ok := s.pathID(w, r)
	if !ok {
		return
	}
	acc, err := emailaccounts.Get(r.Context(), s.DB, id)
	if errors.Is(err, sql.ErrNoRows) {
		s.writeError(w, http.StatusNotFound, "not_found", "email account not found")
		return
	}
	if err != nil {
		s.serverErr(w, "email_accounts.get", err)
		return
	}

	if acc.AuthMethod == emailaccounts.AuthXOAuth2 && s.EmailwatchMSAL == nil {
		s.writeJSON(w, http.StatusOK, map[string]any{
			"ok":      false,
			"message": "microsoft oauth client not configured",
		})
		return
	}
	if s.EmailwatchAEAD == nil {
		s.writeJSON(w, http.StatusOK, map[string]any{
			"ok":      false,
			"message": "server AEAD key not configured",
		})
		return
	}

	var authFn func(*imapclient.Client) error
	switch acc.AuthMethod {
	case emailaccounts.AuthPassword:
		pw, err := emailaccounts.OpenPassword(s.EmailwatchAEAD, acc.SealedSecret)
		if err != nil {
			s.writeJSON(w, http.StatusOK, map[string]any{
				"ok":      false,
				"message": "unseal password: " + err.Error(),
			})
			return
		}
		authFn = func(c *imapclient.Client) error { return c.Login(acc.Username, pw) }
	case emailaccounts.AuthXOAuth2:
		cache, err := emailaccounts.OpenTokenCache(s.EmailwatchAEAD, acc.SealedSecret)
		if err != nil {
			s.writeJSON(w, http.StatusOK, map[string]any{
				"ok":      false,
				"message": "unseal token cache: " + err.Error(),
			})
			return
		}
		refreshed, err := s.EmailwatchMSAL.AcquireTokenSilent(r.Context(), cache, acc.OAuthAccountID)
		if err != nil {
			s.writeJSON(w, http.StatusOK, map[string]any{
				"ok":      false,
				"message": "acquire token: " + err.Error(),
			})
			return
		}
		token := refreshed.AccessToken
		authFn = func(c *imapclient.Client) error {
			return c.Authenticate(oauth.XOAUTH2Client(acc.Username, token))
		}
	default:
		s.writeJSON(w, http.StatusOK, map[string]any{
			"ok":      false,
			"message": "unknown auth method: " + string(acc.AuthMethod),
		})
		return
	}

	addr := fmt.Sprintf("%s:%d", acc.Host, acc.Port)
	dialer := &tls.Dialer{
		NetDialer: &net.Dialer{Timeout: 5 * time.Second},
		Config:    &tls.Config{ServerName: acc.Host, MinVersion: tls.VersionTLS12},
	}
	conn, err := dialer.DialContext(r.Context(), "tcp", addr)
	if err != nil {
		s.writeJSON(w, http.StatusOK, map[string]any{
			"ok": false, "message": "connect: " + err.Error(),
		})
		return
	}
	c, err := imapclient.New(conn)
	if err != nil {
		_ = conn.Close()
		s.writeJSON(w, http.StatusOK, map[string]any{
			"ok": false, "message": "imap handshake: " + err.Error(),
		})
		return
	}
	defer c.Logout()

	if err := authFn(c); err != nil {
		s.writeJSON(w, http.StatusOK, map[string]any{
			"ok": false, "message": "login: " + err.Error(),
		})
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "message": "Connected — mailbox reachable.",
	})
}

// ---------- oauth device-code ----------

// StartEmailAccountOAuth — POST /api/admin/email-accounts/oauth/start.
func (s *Server) StartEmailAccountOAuth(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	var body struct {
		Provider string `json:"provider"`
	}
	if err := decodeJSON(r, &body); err != nil {
		s.writeError(w, http.StatusBadRequest, "bad_json", err.Error())
		return
	}
	if body.Provider != "microsoft" {
		s.writeError(w, http.StatusBadRequest, "unsupported_provider",
			"only microsoft device-code flow is supported")
		return
	}
	if s.EmailwatchMSAL == nil {
		s.writeError(w, http.StatusServiceUnavailable, "no_msal",
			"microsoft oauth client not configured")
		return
	}
	flow, err := s.EmailwatchMSAL.DeviceCodeStart(r.Context())
	if err != nil {
		s.serverErr(w, "email_accounts.oauth.start", err)
		return
	}
	handle, err := newFlowHandle()
	if err != nil {
		s.serverErr(w, "email_accounts.oauth.handle", err)
		return
	}
	oauthFlows.Store(handle, oauthFlowEntry{
		flow:      flow,
		expiresAt: time.Now().Add(oauthFlowTTL),
	})
	s.writeJSON(w, http.StatusOK, map[string]any{
		"flow_handle":      handle,
		"user_code":        flow.UserCode,
		"verification_url": flow.VerificationURL,
		"expires_at":       flow.ExpiresAt.Unix(),
		"message":          flow.Message,
	})
}

// CompleteEmailAccountOAuth — POST /api/admin/email-accounts/oauth/complete.
func (s *Server) CompleteEmailAccountOAuth(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	var body struct {
		FlowHandle string `json:"flow_handle"`
		AccountID  *int64 `json:"account_id,omitempty"`
	}
	if err := decodeJSON(r, &body); err != nil {
		s.writeError(w, http.StatusBadRequest, "bad_json", err.Error())
		return
	}
	if body.FlowHandle == "" {
		s.writeError(w, http.StatusBadRequest, "missing_handle", "flow_handle is required")
		return
	}
	if s.EmailwatchMSAL == nil {
		s.writeError(w, http.StatusServiceUnavailable, "no_msal",
			"microsoft oauth client not configured")
		return
	}

	raw, ok := oauthFlows.Load(body.FlowHandle)
	if !ok {
		s.writeError(w, http.StatusNotFound, "flow_gone",
			"flow_handle unknown or already consumed")
		return
	}
	entry := raw.(oauthFlowEntry)
	if time.Now().After(entry.expiresAt) {
		oauthFlows.Delete(body.FlowHandle)
		s.writeError(w, http.StatusNotFound, "flow_expired",
			"flow_handle expired; start a new flow")
		return
	}

	completed, err := s.EmailwatchMSAL.DeviceCodeComplete(r.Context(), entry.flow)
	if err != nil {
		s.serverErr(w, "email_accounts.oauth.complete", err)
		return
	}

	if s.EmailwatchAEAD == nil {
		s.writeError(w, http.StatusServiceUnavailable, "no_aead",
			"server AEAD key not configured")
		return
	}
	sealed, err := emailaccounts.SealTokenCache(s.EmailwatchAEAD, completed.CacheJSON)
	if err != nil {
		s.serverErr(w, "email_accounts.oauth.seal", err)
		return
	}

	if body.AccountID != nil {
		am := emailaccounts.AuthXOAuth2
		oid := completed.HomeAccountID
		user := completed.PreferredUsername
		patch := emailaccounts.AccountPatch{
			AuthMethod:     &am,
			SealedSecret:   &sealed,
			OAuthAccountID: &oid,
			Username:       &user,
		}
		updated, err := emailaccounts.Patch(r.Context(), s.DB, *body.AccountID, patch)
		if errors.Is(err, sql.ErrNoRows) {
			s.writeError(w, http.StatusNotFound, "not_found", "email account not found")
			return
		}
		if err != nil {
			s.serverErr(w, "email_accounts.oauth.patch", err)
			return
		}
		oauthFlows.Delete(body.FlowHandle)
		audit.Log(r.Context(), s.DB, s.Log, audit.Event{
			Actor:      auth.FromContext(r.Context()),
			Action:     "email_account.oauth_complete",
			ObjectKind: "email_account",
			ObjectID:   updated.ID,
			After: map[string]any{
				"oauth_account_id":    completed.HomeAccountID,
				"username":            completed.PreferredUsername,
				"token_cache_updated": true,
			},
			RequestID: logx.RequestID(r.Context()),
		})
		s.reloadEmailwatch(r.Context())
		s.writeJSON(w, http.StatusOK, map[string]any{
			"ok":               true,
			"username":         completed.PreferredUsername,
			"oauth_account_id": completed.HomeAccountID,
		})
		return
	}

	// No account_id: return the sealed bytes so a follow-up POST create
	// can consume them. Wire is JSON so we base64 the bytes.
	oauthFlows.Delete(body.FlowHandle)
	s.writeJSON(w, http.StatusOK, map[string]any{
		"ok":                true,
		"username":          completed.PreferredUsername,
		"oauth_account_id":  completed.HomeAccountID,
		"sealed_secret_b64": base64.StdEncoding.EncodeToString(sealed),
	})
}

// RevokeEmailAccountOAuth — POST /api/admin/email-accounts/{id}/oauth/revoke.
//
// Clears the sealed token cache, drops the OAuth account id, flips auth
// back to password, and disables the row. The account is unusable until
// the operator either re-runs the OAuth flow or supplies a password via
// PATCH.
func (s *Server) RevokeEmailAccountOAuth(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	id, ok := s.pathID(w, r)
	if !ok {
		return
	}
	before, err := emailaccounts.Get(r.Context(), s.DB, id)
	if errors.Is(err, sql.ErrNoRows) {
		s.writeError(w, http.StatusNotFound, "not_found", "email account not found")
		return
	}
	if err != nil {
		s.serverErr(w, "email_accounts.get", err)
		return
	}

	// The store rejects a zero-length sealed_secret because the column
	// is NOT NULL. A single-byte placeholder keeps the write valid while
	// making the row obviously unusable until re-credentialed.
	placeholder := []byte{0}
	empty := ""
	am := emailaccounts.AuthPassword
	off := false
	patch := emailaccounts.AccountPatch{
		SealedSecret:   &placeholder,
		OAuthAccountID: &empty,
		AuthMethod:     &am,
		Enabled:        &off,
	}
	updated, err := emailaccounts.Patch(r.Context(), s.DB, id, patch)
	if err != nil {
		s.serverErr(w, "email_accounts.oauth.revoke", err)
		return
	}
	audit.Log(r.Context(), s.DB, s.Log, audit.Event{
		Actor:      auth.FromContext(r.Context()),
		Action:     "email_account.oauth_revoke",
		ObjectKind: "email_account",
		ObjectID:   id,
		Before:     accountAuditView(before),
		After:      accountAuditView(updated),
		RequestID:  logx.RequestID(r.Context()),
	})
	s.reloadEmailwatch(r.Context())
	w.WriteHeader(http.StatusNoContent)
}

// ---------- helpers ----------

// accountAuditView drops the sealed_secret before an audit row lands.
// Callers must use this helper — never marshal an Account directly into
// audit.Event.After / .Before.
func accountAuditView(a *emailaccounts.Account) map[string]any {
	if a == nil {
		return nil
	}
	return map[string]any{
		"id":                a.ID,
		"name":              a.Name,
		"owner_id":          a.OwnerID,
		"provider":          string(a.Provider),
		"host":              a.Host,
		"port":              a.Port,
		"use_tls":           a.UseTLS,
		"folder":            a.Folder,
		"processed_folder":  a.ProcessedFolder,
		"poll_interval_min": a.PollIntervalMin,
		"auth_method":       string(a.AuthMethod),
		"username":          a.Username,
		"oauth_account_id":  a.OAuthAccountID,
		"attachments_only":  a.AttachmentsOnly,
		"from_allowlist":    a.FromAllowlist,
		"sync_since":        syncSinceAudit(a.SyncSince),
		"enabled":           a.Enabled,
		"mark_seen":         a.MarkSeen,
	}
}

// syncSinceAudit returns 0 for nil (== "sync all") and the unix
// timestamp otherwise. The map value must be a plain int64 so audit
// diffs stay stable across restarts (a *int64 pointer would render
// as a pointer address).
func syncSinceAudit(p *int64) int64 {
	if p == nil {
		return 0
	}
	return *p
}

func (s *Server) pathID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		s.writeError(w, http.StatusBadRequest, "bad_id", "id must be a positive integer")
		return 0, false
	}
	return id, true
}

func (s *Server) validateOwnerID(ctx context.Context, uid int64) error {
	if uid <= 0 {
		return errors.New("owner_id required")
	}
	var got int64
	err := s.DB.Read.QueryRowContext(ctx,
		`SELECT id FROM users WHERE id = ?`, uid).Scan(&got)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("owner_id %d not found", uid)
	}
	return err
}

// reloadEmailwatch is a no-op when the reloader wasn't wired (tests,
// headless smoke suites). Errors are logged and swallowed — a reload
// failure never blocks a write that already committed.
func (s *Server) reloadEmailwatch(ctx context.Context) {
	if s.EmailwatchReload == nil {
		return
	}
	if err := s.EmailwatchReload(ctx); err != nil {
		s.Log.Warn("email_accounts.reload_failed", "err", err.Error())
	}
}

// newFlowHandle returns a URL-safe 16-byte random handle.
func newFlowHandle() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// trimStringPtr preserves the sparse-patch contract: nil → nil (leave
// column alone), non-nil → trimmed value (may be empty, meaning "clear
// this column").
func trimStringPtr(p *string) *string {
	if p == nil {
		return nil
	}
	v := strings.TrimSpace(*p)
	return &v
}
