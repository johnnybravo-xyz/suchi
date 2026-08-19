package api

// Handler-level coverage for /api/email-accounts. Every path that
// mutates state or reads sealed_secret has its own case so a future
// refactor can't silently drop the admin gate, leak the sealed blob,
// or skip the supervisor reload.

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	pluginapi "github.com/johnnybravo-xyz/suchi/plugin-api"

	"github.com/johnnybravo-xyz/suchi/core/auth"
	"github.com/johnnybravo-xyz/suchi/core/crypto"
	"github.com/johnnybravo-xyz/suchi/core/emailaccounts"
	"github.com/johnnybravo-xyz/suchi/core/ingest/emailwatch/oauth"
)

// newEmailAccountsServer wires the minimum Server the handlers touch:
// DB, log, AEAD key, and a reload counter the tests assert on.
func newEmailAccountsServer(t *testing.T) (*Server, *atomic.Int64) {
	t.Helper()
	d := openTestDB(t)
	k, err := crypto.LoadOrCreateKey(filepath.Join(t.TempDir(), ".decrypt-key"))
	if err != nil {
		t.Fatal(err)
	}
	reloads := new(atomic.Int64)
	s := &Server{
		DB:             d,
		Log:            slog.New(slog.NewTextHandler(os.Stderr, nil)),
		EmailwatchAEAD: k,
		EmailwatchReload: func(ctx context.Context) error {
			reloads.Add(1)
			return nil
		},
	}
	return s, reloads
}

// muxFor registers only the email-accounts routes; nothing else in
// Server.Register is needed for these tests. Path values ({id}) work
// only when the request rides through a ServeMux match.
func muxFor(s *Server) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/email-accounts", s.ListEmailAccounts)
	mux.HandleFunc("POST /api/email-accounts", s.CreateEmailAccount)
	mux.HandleFunc("GET /api/email-accounts/{id}", s.GetEmailAccount)
	mux.HandleFunc("PATCH /api/email-accounts/{id}", s.PatchEmailAccount)
	mux.HandleFunc("DELETE /api/email-accounts/{id}", s.DeleteEmailAccount)
	mux.HandleFunc("POST /api/email-accounts/{id}/test", s.TestEmailAccount)
	mux.HandleFunc("POST /api/email-accounts/oauth/start", s.StartEmailAccountOAuth)
	mux.HandleFunc("POST /api/email-accounts/oauth/complete", s.CompleteEmailAccountOAuth)
	mux.HandleFunc("POST /api/email-accounts/{id}/oauth/revoke", s.RevokeEmailAccountOAuth)
	return mux
}

func call(t *testing.T, s *Server, method, path, body string, p *pluginapi.Principal) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	ctx := context.Background()
	if p != nil {
		ctx = auth.WithPrincipal(ctx, p)
	}
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, path, http.NoBody).WithContext(ctx)
	} else {
		r = httptest.NewRequest(method, path, strings.NewReader(body)).WithContext(ctx)
	}
	r.Header.Set("Content-Type", "application/json")
	muxFor(s).ServeHTTP(rec, r)
	return rec
}

// ---------- admin gate ----------

func TestEmailAccounts_AdminGate(t *testing.T) {
	s, _ := newEmailAccountsServer(t)
	seedUser(t, s.DB, 1)
	member := memberPrincipal(1)

	cases := []struct {
		method, path string
	}{
		{"GET", "/api/email-accounts"},
		{"POST", "/api/email-accounts"},
		{"GET", "/api/email-accounts/1"},
		{"PATCH", "/api/email-accounts/1"},
		{"DELETE", "/api/email-accounts/1"},
		{"POST", "/api/email-accounts/1/test"},
		{"POST", "/api/email-accounts/oauth/start"},
		{"POST", "/api/email-accounts/oauth/complete"},
		{"POST", "/api/email-accounts/1/oauth/revoke"},
	}
	for _, c := range cases {
		t.Run(c.method+" "+c.path, func(t *testing.T) {
			rec := call(t, s, c.method, c.path, `{}`, member)
			if rec.Code != http.StatusForbidden {
				t.Fatalf("non-admin got %d, want 403; body=%s", rec.Code, rec.Body.String())
			}
			// Anonymous → 401.
			rec = call(t, s, c.method, c.path, `{}`, nil)
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("anonymous got %d, want 401; body=%s", rec.Code, rec.Body.String())
			}
		})
	}
}

// ---------- create ----------

func TestEmailAccounts_Create_HappyPath(t *testing.T) {
	s, reloads := newEmailAccountsServer(t)
	seedUser(t, s.DB, 1)

	body := `{
		"name":"primary",
		"owner_id":1,
		"provider":"fastmail",
		"username":"a@example.com",
		"password":"hunter2",
		"enabled":true
	}`
	rec := call(t, s, "POST", "/api/email-accounts", body, adminPrincipal(1))
	if rec.Code != http.StatusCreated {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var out emailaccounts.Account
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.ID == 0 || out.Host != "imap.fastmail.com" || out.Port != 993 || !out.UseTLS {
		t.Fatalf("preset auto-fill missed: %+v", out)
	}
	// SyncSince defaults to ~time.Now() when the caller omits it —
	// keeps "add mailbox" zero-config safe. Non-nil is the contract.
	if out.SyncSince == nil {
		t.Fatalf("sync_since should default to now, got nil")
	}
	if strings.Contains(rec.Body.String(), "sealed_secret") {
		t.Fatalf("sealed_secret must not appear in response body: %s", rec.Body.String())
	}
	// DB row exists with a non-empty sealed_secret.
	row, err := emailaccounts.Get(context.Background(), s.DB, out.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(row.SealedSecret) == 0 {
		t.Fatal("sealed_secret should be set after create")
	}
	// Reload was called.
	if reloads.Load() != 1 {
		t.Fatalf("reload count=%d, want 1", reloads.Load())
	}
	// Audit row landed.
	var count int
	if err := s.DB.Read.QueryRow(`SELECT COUNT(*) FROM audit_events WHERE action='email_account.create'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("audit rows=%d, want 1", count)
	}
}

func TestEmailAccounts_Create_Validation(t *testing.T) {
	s, _ := newEmailAccountsServer(t)
	seedUser(t, s.DB, 1)
	cases := []struct{ name, body, wantCode string }{
		{"missing_name",
			`{"owner_id":1,"provider":"fastmail","username":"a@example.com","password":"p"}`,
			"validation"},
		{"missing_owner",
			`{"name":"x","provider":"fastmail","username":"a@example.com","password":"p"}`,
			"bad_owner"},
		{"missing_username",
			`{"name":"x","owner_id":1,"provider":"fastmail","password":"p"}`,
			"validation"},
		{"missing_password",
			`{"name":"x","owner_id":1,"provider":"fastmail","username":"a@example.com"}`,
			"missing_password"},
		{"missing_host_no_preset",
			`{"name":"x","owner_id":1,"provider":"custom","username":"a@example.com","password":"p"}`,
			"validation"},
		{"unknown_provider",
			`{"name":"x","owner_id":1,"provider":"nope","username":"a@example.com","password":"p"}`,
			"unknown_provider"},
		{"xoauth2_via_create_rejected",
			`{"name":"x","owner_id":1,"provider":"microsoft","username":"a@example.com","password":"p"}`,
			"oauth_required"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := call(t, s, "POST", "/api/email-accounts", c.body, adminPrincipal(1))
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), c.wantCode) {
				t.Fatalf("want code %q, got body=%s", c.wantCode, rec.Body.String())
			}
		})
	}
}

// ---------- list + get ----------

func TestEmailAccounts_List_OmitsSealedSecret(t *testing.T) {
	s, _ := newEmailAccountsServer(t)
	seedUser(t, s.DB, 1)
	// Seed via the store to skip the handler path.
	sealed, _ := emailaccounts.SealPassword(s.EmailwatchAEAD, "p")
	if _, err := emailaccounts.Create(context.Background(), s.DB, emailaccounts.Account{
		Name: "a", OwnerID: 1, Provider: emailaccounts.ProviderCustom,
		Host: "h", Port: 993, UseTLS: true,
		AuthMethod: emailaccounts.AuthPassword, Username: "u", SealedSecret: sealed,
		Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}

	rec := call(t, s, "GET", "/api/email-accounts", "", adminPrincipal(1))
	if rec.Code != 200 {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "sealed_secret") {
		t.Fatalf("list leaked sealed_secret: %s", rec.Body.String())
	}
	var listed struct {
		Capabilities struct {
			MicrosoftOAuth struct {
				Ready  bool   `json:"ready"`
				Reason string `json:"reason"`
			} `json:"microsoft_oauth"`
		} `json:"capabilities"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &listed); err != nil {
		t.Fatal(err)
	}
	if listed.Capabilities.MicrosoftOAuth.Ready || listed.Capabilities.MicrosoftOAuth.Reason == "" {
		t.Fatalf("missing OAuth readiness: %#v", listed.Capabilities.MicrosoftOAuth)
	}
}

func TestEmailAccounts_Get_404(t *testing.T) {
	s, _ := newEmailAccountsServer(t)
	rec := call(t, s, "GET", "/api/email-accounts/999", "", adminPrincipal(1))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}

// ---------- patch ----------

func TestEmailAccounts_Patch_PasswordSealsAndAudits(t *testing.T) {
	s, reloads := newEmailAccountsServer(t)
	seedUser(t, s.DB, 1)
	sealed, _ := emailaccounts.SealPassword(s.EmailwatchAEAD, "old")
	acc, err := emailaccounts.Create(context.Background(), s.DB, emailaccounts.Account{
		Name: "a", OwnerID: 1, Provider: emailaccounts.ProviderCustom,
		Host: "h", Port: 993, UseTLS: true,
		AuthMethod: emailaccounts.AuthPassword, Username: "u", SealedSecret: sealed,
		Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	prev := reloads.Load()

	rec := call(t, s, "PATCH",
		"/api/email-accounts/"+strconv.FormatInt(acc.ID, 10),
		`{"password":"new-hunter"}`, adminPrincipal(1))
	if rec.Code != 200 {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	after, _ := emailaccounts.Get(context.Background(), s.DB, acc.ID)
	if string(after.SealedSecret) == string(sealed) {
		t.Fatal("sealed_secret should have been replaced")
	}
	if reloads.Load() != prev+1 {
		t.Fatalf("reload didn't fire: prev=%d now=%d", prev, reloads.Load())
	}

	// Audit After has password_updated=true; the plaintext must not
	// appear anywhere in the audit row.
	var afterJSON string
	if err := s.DB.Read.QueryRow(
		`SELECT after_json FROM audit_events WHERE action='email_account.update' ORDER BY id DESC LIMIT 1`,
	).Scan(&afterJSON); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(afterJSON, `"password_updated":true`) {
		t.Fatalf("audit after missing password_updated flag: %s", afterJSON)
	}
	if strings.Contains(afterJSON, "new-hunter") {
		t.Fatalf("audit row leaked plaintext password: %s", afterJSON)
	}
}

// ---------- delete ----------

func TestEmailAccounts_Delete(t *testing.T) {
	s, reloads := newEmailAccountsServer(t)
	seedUser(t, s.DB, 1)
	sealed, _ := emailaccounts.SealPassword(s.EmailwatchAEAD, "p")
	acc, err := emailaccounts.Create(context.Background(), s.DB, emailaccounts.Account{
		Name: "a", OwnerID: 1, Provider: emailaccounts.ProviderCustom,
		Host: "h", Port: 993, UseTLS: true,
		AuthMethod: emailaccounts.AuthPassword, Username: "u", SealedSecret: sealed,
		Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	prev := reloads.Load()

	rec := call(t, s, "DELETE",
		"/api/email-accounts/"+strconv.FormatInt(acc.ID, 10),
		"", adminPrincipal(1))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if reloads.Load() != prev+1 {
		t.Fatalf("reload didn't fire on delete")
	}
	var count int
	if err := s.DB.Read.QueryRow(
		`SELECT COUNT(*) FROM audit_events WHERE action='email_account.delete'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("audit rows=%d, want 1", count)
	}
	// Row is gone.
	if rec := call(t, s, "GET",
		"/api/email-accounts/"+strconv.FormatInt(acc.ID, 10),
		"", adminPrincipal(1)); rec.Code != http.StatusNotFound {
		t.Fatalf("post-delete GET status=%d", rec.Code)
	}
}

// ---------- test dial ----------

// The test-dial handler needs an actual IMAP server to prove the happy
// path; here we cover only the "MSAL client missing" branch for XOAUTH2
// accounts. The password branch would hit the network.
func TestEmailAccounts_TestDial_XOAUTH2_NoMSAL(t *testing.T) {
	s, _ := newEmailAccountsServer(t)
	seedUser(t, s.DB, 1)
	sealed, _ := emailaccounts.SealMicrosoftOAuthCredential(s.EmailwatchAEAD,
		emailaccounts.MicrosoftOAuthCredential{
			ClientID: "11111111-1111-1111-1111-111111111111", CacheJSON: []byte(`{"unused":true}`),
		})
	acc, err := emailaccounts.Create(context.Background(), s.DB, emailaccounts.Account{
		Name: "a", OwnerID: 1, Provider: emailaccounts.ProviderMicrosoft,
		Host: "outlook.office365.com", Port: 993, UseTLS: true,
		AuthMethod: emailaccounts.AuthXOAuth2, Username: "u@example.com",
		SealedSecret: sealed, OAuthAccountID: "home-abc", Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	rec := call(t, s, "POST",
		"/api/email-accounts/"+strconv.FormatInt(acc.ID, 10)+"/test",
		"", adminPrincipal(1))
	if rec.Code != 200 {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "unavailable") {
		t.Fatalf("expected msal-missing message; got %s", rec.Body.String())
	}
}

// ---------- oauth ----------

func TestEmailAccounts_OAuth_Start_NoMSAL(t *testing.T) {
	s, _ := newEmailAccountsServer(t)
	rec := call(t, s, "POST", "/api/email-accounts/oauth/start",
		`{"provider":"microsoft"}`, adminPrincipal(1))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestEmailAccounts_OAuth_Start_BadProvider(t *testing.T) {
	s, _ := newEmailAccountsServer(t)
	rec := call(t, s, "POST", "/api/email-accounts/oauth/start",
		`{"provider":"gmail"}`, adminPrincipal(1))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestEmailAccounts_OAuth_Complete_UnknownHandle(t *testing.T) {
	s, _ := newEmailAccountsServer(t)
	// The complete handler bails on missing MSAL before the flow lookup.
	// A default-options client is enough here: the flow-map miss returns
	// 404 before any MSAL method fires.
	m, err := oauth.NewManager("11111111-1111-1111-1111-111111111111", nil)
	if err != nil {
		t.Fatal(err)
	}
	s.EmailwatchMSAL = m
	rec := call(t, s, "POST", "/api/email-accounts/oauth/complete",
		`{"flow_handle":"does-not-exist"}`, adminPrincipal(1))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestOAuthFlowStoreBoundsAndPrunes(t *testing.T) {
	now := time.Now()
	var store oauthFlowStore
	for i := 0; i < maxOAuthFlows; i++ {
		handle := strconv.Itoa(i)
		if !store.put(handle, oauthFlowEntry{expiresAt: now.Add(time.Minute)}, now) {
			t.Fatalf("flow %d rejected before limit", i)
		}
	}
	if store.put("overflow", oauthFlowEntry{expiresAt: now.Add(time.Minute)}, now) {
		t.Fatal("flow store exceeded its limit")
	}
	if !store.put("after-expiry", oauthFlowEntry{expiresAt: now.Add(time.Minute)}, now.Add(2*time.Minute)) {
		t.Fatal("expired flows were not pruned")
	}
}

func TestOAuthFlowStoreAllowsOneCompletion(t *testing.T) {
	now := time.Now()
	var store oauthFlowStore
	if !store.put("flow", oauthFlowEntry{expiresAt: now.Add(time.Minute)}, now) {
		t.Fatal("put failed")
	}
	if _, err := store.begin("flow", now); err != nil {
		t.Fatal(err)
	}
	if _, err := store.begin("flow", now); !errors.Is(err, errOAuthFlowActive) {
		t.Fatalf("second completion error = %v", err)
	}
	store.release("flow")
	if _, err := store.begin("flow", now); err != nil {
		t.Fatalf("retry after release: %v", err)
	}
}

// ---------- revoke ----------

func TestEmailAccounts_OAuth_Revoke_ClearsAndDisables(t *testing.T) {
	s, reloads := newEmailAccountsServer(t)
	seedUser(t, s.DB, 1)
	sealed, _ := emailaccounts.SealMicrosoftOAuthCredential(s.EmailwatchAEAD,
		emailaccounts.MicrosoftOAuthCredential{
			ClientID: "11111111-1111-1111-1111-111111111111", CacheJSON: []byte("cache"),
		})
	acc, err := emailaccounts.Create(context.Background(), s.DB, emailaccounts.Account{
		Name: "a", OwnerID: 1, Provider: emailaccounts.ProviderMicrosoft,
		Host: "outlook.office365.com", Port: 993, UseTLS: true,
		AuthMethod: emailaccounts.AuthXOAuth2, Username: "u", SealedSecret: sealed,
		OAuthAccountID: "home-abc", Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	prev := reloads.Load()

	rec := call(t, s, "POST",
		"/api/email-accounts/"+strconv.FormatInt(acc.ID, 10)+"/oauth/revoke",
		"", adminPrincipal(1))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	after, _ := emailaccounts.Get(context.Background(), s.DB, acc.ID)
	if after.OAuthAccountID != "" {
		t.Fatalf("oauth_account_id should be cleared, got %q", after.OAuthAccountID)
	}
	if after.AuthMethod != emailaccounts.AuthPassword {
		t.Fatalf("auth_method should be password, got %q", after.AuthMethod)
	}
	if after.Enabled {
		t.Fatal("row should be disabled after revoke")
	}
	if reloads.Load() != prev+1 {
		t.Fatal("reload didn't fire on revoke")
	}
}
