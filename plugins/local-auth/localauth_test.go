package localauth

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/johnnybravo-xyz/suchi/core/auth"
	"github.com/johnnybravo-xyz/suchi/core/db"
	migrations "github.com/johnnybravo-xyz/suchi/core/db/migrations"
)

func openTestPlugin(t *testing.T) *Plugin {
	return openTestPluginWithOptions(t, Options{})
}

func openTestPluginWithOptions(t *testing.T, opts Options) *Plugin {
	t.Helper()
	ctx := context.Background()
	d, err := db.Open(ctx, filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	migs, err := db.LoadMigrations(migrations.FS, ".")
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	if err := db.Migrate(ctx, d, migs, log); err != nil {
		t.Fatal(err)
	}
	p, err := NewWithOptions(ctx, d, log, false, false, opts)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestNewWithOptions_DisablesLocalSetup(t *testing.T) {
	p := openTestPluginWithOptions(t, Options{DisableSetup: true})
	if got := p.SetupToken(); got != "" {
		t.Fatalf("OIDC-only plugin minted local setup token %q", got)
	}
}

func TestDemoBrowserSessionIsBoundedAndConstrained(t *testing.T) {
	p := openTestPlugin(t)
	p.demoMode = true
	p.cookieSecure = true
	ctx := t.Context()
	_, err := p.db.ExecWrite(ctx, `INSERT INTO users(id,email,display_name,role,created_at,updated_at)
		VALUES(1,'visitor-test@demo.local','Visitor','member',0,0)`)
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("POST", "/api/demo/session/upgrade", nil)
	w := httptest.NewRecorder()
	if err := p.IssueDemoSession(w, r, 1, time.Minute); err != nil {
		t.Fatal(err)
	}
	cookie := w.Result().Cookies()[0]
	if !cookie.HttpOnly || !cookie.Secure || cookie.SameSite != http.SameSiteLaxMode || time.Until(cookie.Expires) > time.Minute {
		t.Fatalf("invalid demo cookie attributes: name=%s expires=%s", cookie.Name, cookie.Expires)
	}
	r.AddCookie(cookie)
	principal, err := p.Authenticate(r)
	if err != nil || principal == nil || principal.Kind != "demo-scratch" || !auth.HasScope(principal, auth.ScopeDocumentsWrite) {
		t.Fatalf("scratch principal=%+v error=%v", principal, err)
	}
	var lifetime int64
	if err := p.db.Read.QueryRowContext(ctx, `SELECT expires_at-created_at FROM sessions`).Scan(&lifetime); err != nil || lifetime != 60 {
		t.Fatalf("session lifetime=%d error=%v", lifetime, err)
	}
	if _, err := p.db.ExecWrite(ctx, `UPDATE sessions SET expires_at=0`); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Authenticate(r); err == nil {
		t.Fatal("expired demo session authenticated")
	}
	p.LogoutHandler(httptest.NewRecorder(), r)
	var sessions int
	if err := p.db.Read.QueryRowContext(ctx, `SELECT COUNT(*) FROM sessions`).Scan(&sessions); err != nil || sessions != 0 {
		t.Fatalf("logout left sessions=%d error=%v", sessions, err)
	}
}

func TestEnsureDevAdmin_Refusals(t *testing.T) {
	ctx := context.Background()

	cases := []struct {
		name     string
		email    string
		password string
		wantErr  string
	}{
		{"empty_email", "", DevAdminPassword, "email and password required"},
		{"empty_password", DevAdminEmail, "", "email and password required"},
		{"short_password", DevAdminEmail, "1234567", "at least 8 chars"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := openTestPlugin(t)
			err := p.EnsureDevAdmin(ctx, tc.email, tc.password)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("want %q in err, got %v", tc.wantErr, err)
			}
		})
	}
}

func TestEnsureDevAdmin_RefusesRoleCollision(t *testing.T) {
	ctx := context.Background()
	p := openTestPlugin(t)
	// Seed a real member user with the same email dev-mode wants.
	if _, err := p.db.Write.ExecContext(ctx, `
		INSERT INTO users(email, display_name, role, created_at, updated_at)
		VALUES (?, 'member seed', 'member', 0, 0)
	`, "victim@suchi.local"); err != nil {
		t.Fatal(err)
	}
	err := p.EnsureDevAdmin(ctx, "victim@suchi.local", DevAdminPassword)
	if err == nil || !strings.Contains(err.Error(), "role") {
		t.Fatalf("want role-collision refusal, got %v", err)
	}
	// The member must still be a member — no silent promotion.
	var role string
	if err := p.db.Read.QueryRowContext(ctx,
		`SELECT role FROM users WHERE email = ?`, "victim@suchi.local").Scan(&role); err != nil {
		t.Fatal(err)
	}
	if role != "member" {
		t.Fatalf("member silently promoted to %q — refusal path is broken", role)
	}
}

// TestEnsureDevAdmin_RefusesOtherAdmin locks in the accidental-prod
// lateral-takeover guard: an operator with a legit admin who boots with
// SUCHI_DEV=1 + loopback PUBLIC_URL must not gain a second admin row
// carrying the public default credentials.
func TestEnsureDevAdmin_RefusesOtherAdmin(t *testing.T) {
	ctx := context.Background()
	p := openTestPlugin(t)
	if _, err := p.db.Write.ExecContext(ctx, `
		INSERT INTO users(email, display_name, role, created_at, updated_at)
		VALUES (?, 'owner', 'admin', 0, 0)
	`, "owner@example.com"); err != nil {
		t.Fatal(err)
	}
	err := p.EnsureDevAdmin(ctx, DevAdminEmail, DevAdminPassword)
	if err == nil || !strings.Contains(err.Error(), "other admin") {
		t.Fatalf("want other-admin refusal, got %v", err)
	}
	// Verify dev@suchi.local was NOT inserted.
	var count int
	if err := p.db.Read.QueryRowContext(ctx,
		`SELECT count(*) FROM users WHERE email = ?`, DevAdminEmail).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("dev admin row created despite refusal — takeover path is open")
	}
}

// TestEnsureDevAdmin_AllowsMemberSideBySide confirms multi-account
// testing survives the new refusal — only OTHER admins block dev-mode;
// member accounts (Alice, Bob) do not.
func TestEnsureDevAdmin_AllowsMemberSideBySide(t *testing.T) {
	ctx := context.Background()
	p := openTestPlugin(t)
	if _, err := p.db.Write.ExecContext(ctx, `
		INSERT INTO users(email, display_name, role, created_at, updated_at)
		VALUES (?, 'alice', 'member', 0, 0)
	`, "alice@example.com"); err != nil {
		t.Fatal(err)
	}
	if err := p.EnsureDevAdmin(ctx, DevAdminEmail, DevAdminPassword); err != nil {
		t.Fatalf("EnsureDevAdmin refused despite only a member peer: %v", err)
	}
	var role string
	if err := p.db.Read.QueryRowContext(ctx,
		`SELECT role FROM users WHERE email = ?`, DevAdminEmail).Scan(&role); err != nil {
		t.Fatal(err)
	}
	if role != "admin" {
		t.Fatalf("want admin, got %q", role)
	}
}

func TestEnsureDevAdmin_HappyPathInsertsAndBurnsToken(t *testing.T) {
	ctx := context.Background()
	p := openTestPlugin(t)
	// Fresh plugin has a setup token minted (usersEmpty=true).
	if p.SetupToken() == "" {
		t.Fatal("expected setup token to be minted on fresh DB")
	}
	if err := p.EnsureDevAdmin(ctx, DevAdminEmail, DevAdminPassword); err != nil {
		t.Fatal(err)
	}
	if p.SetupToken() != "" {
		t.Fatal("setup token should be burned after EnsureDevAdmin succeeds")
	}
	var role string
	if err := p.db.Read.QueryRowContext(ctx,
		`SELECT role FROM users WHERE email = ?`, DevAdminEmail).Scan(&role); err != nil {
		t.Fatal(err)
	}
	if role != "admin" {
		t.Fatalf("want admin role, got %q", role)
	}
}

func TestEnsureDevAdmin_DoesNotResurrectDisabledAdmin(t *testing.T) {
	ctx := context.Background()
	p := openTestPlugin(t)
	// Insert an admin with disabled=1 — simulates a security quarantine.
	if _, err := p.db.Write.ExecContext(ctx, `
		INSERT INTO users(email, display_name, role, disabled, created_at, updated_at)
		VALUES (?, 'quarantined', 'admin', 1, 0, 0)
	`, DevAdminEmail); err != nil {
		t.Fatal(err)
	}
	if err := p.EnsureDevAdmin(ctx, DevAdminEmail, DevAdminPassword); err != nil {
		t.Fatal(err)
	}
	var disabled int
	if err := p.db.Read.QueryRowContext(ctx,
		`SELECT disabled FROM users WHERE email = ?`, DevAdminEmail).Scan(&disabled); err != nil {
		t.Fatal(err)
	}
	if disabled != 1 {
		t.Fatalf("disabled flag was silently reset — quarantine broken")
	}
}

func TestEnsureDevAdmin_IdempotentPasswordReset(t *testing.T) {
	ctx := context.Background()
	p := openTestPlugin(t)
	if err := p.EnsureDevAdmin(ctx, DevAdminEmail, DevAdminPassword); err != nil {
		t.Fatal(err)
	}
	var firstHash string
	if err := p.db.Read.QueryRowContext(ctx,
		`SELECT password_hash FROM users WHERE email = ?`, DevAdminEmail).Scan(&firstHash); err != nil {
		t.Fatal(err)
	}
	if err := p.EnsureDevAdmin(ctx, DevAdminEmail, "rotatedrotated"); err != nil {
		t.Fatal(err)
	}
	var secondHash string
	if err := p.db.Read.QueryRowContext(ctx,
		`SELECT password_hash FROM users WHERE email = ?`, DevAdminEmail).Scan(&secondHash); err != nil {
		t.Fatal(err)
	}
	if firstHash == secondHash {
		t.Fatal("password_hash should rotate on re-invocation with a new password")
	}
}

func TestRefuseEnabledDevAdminAfterDevDataReuse(t *testing.T) {
	ctx := context.Background()
	p := openTestPlugin(t)
	if err := p.EnsureDevAdmin(ctx, DevAdminEmail, DevAdminPassword); err != nil {
		t.Fatal(err)
	}

	if err := p.RefuseEnabledDevAdmin(ctx); err == nil || !strings.Contains(err.Error(), "enabled public development admin") {
		t.Fatalf("normal boot guard error = %v, want enabled development admin refusal", err)
	}
	if _, err := p.db.ExecWrite(ctx,
		`UPDATE users SET disabled = 1 WHERE email = ?`, DevAdminEmail); err != nil {
		t.Fatal(err)
	}
	if err := p.RefuseEnabledDevAdmin(ctx); err != nil {
		t.Fatalf("disabled development admin blocked normal boot: %v", err)
	}
}

func TestTokenHandlerIssuesGranularTokenWithoutSession(t *testing.T) {
	p := openTestPlugin(t)
	if err := p.EnsureDevAdmin(context.Background(), DevAdminEmail, DevAdminPassword); err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(LoginRequest{Email: DevAdminEmail, Password: DevAdminPassword})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/token/", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	p.TokenHandler(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var scopes string
	if err := p.db.Read.QueryRow(`SELECT scopes FROM api_tokens ORDER BY id DESC LIMIT 1`).Scan(&scopes); err != nil {
		t.Fatal(err)
	}
	if scopes != "documents:read,documents:write" {
		t.Fatalf("scopes: got %q, want granular document scopes", scopes)
	}
	var response struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if cookies := rec.Result().Cookies(); len(cookies) != 0 {
		t.Fatalf("token exchange set %d cookies, want none", len(cookies))
	}
	var sessionCount int
	if err := p.db.Read.QueryRow(`SELECT count(*) FROM sessions`).Scan(&sessionCount); err != nil {
		t.Fatal(err)
	}
	if sessionCount != 0 {
		t.Fatalf("token exchange created %d session rows, want none", sessionCount)
	}
	tokenReq := httptest.NewRequest(http.MethodGet, "/api/documents/", nil)
	tokenReq.Header.Set("Authorization", "Token "+response.Token)
	tokenPrincipal, err := p.Authenticate(tokenReq)
	if err != nil || tokenPrincipal == nil {
		t.Fatalf("token authentication failed: principal=%v err=%v", tokenPrincipal, err)
	}

	if _, err := p.db.ExecWrite(context.Background(), `UPDATE users SET disabled = 1 WHERE id = ?`, tokenPrincipal.UserID); err != nil {
		t.Fatal(err)
	}
	if principal, err := p.Authenticate(tokenReq); err == nil || principal != nil {
		t.Fatalf("disabled user's token accepted: principal=%v err=%v", principal, err)
	}
	if _, err := p.db.ExecWrite(context.Background(), `UPDATE users SET disabled = 0 WHERE id = ?`, tokenPrincipal.UserID); err != nil {
		t.Fatal(err)
	}

	logoutReq := httptest.NewRequest(http.MethodPost, "/api/logout", nil)
	logoutReq = logoutReq.WithContext(auth.WithPrincipal(logoutReq.Context(), tokenPrincipal))
	logoutRec := httptest.NewRecorder()
	p.LogoutHandler(logoutRec, logoutReq)
	if logoutRec.Code != http.StatusNoContent {
		t.Fatalf("logout status: got %d, want 204", logoutRec.Code)
	}
	var revokedCount int
	if err := p.db.Read.QueryRow(`SELECT count(*) FROM api_tokens WHERE id = ? AND revoked_at IS NOT NULL`, tokenPrincipal.TokenID).Scan(&revokedCount); err != nil {
		t.Fatal(err)
	}
	if revokedCount != 1 {
		t.Fatalf("logout revoked_tokens=%d, want 1", revokedCount)
	}
}

func TestLoginHandlerCreatesOnlyBrowserSession(t *testing.T) {
	p := openTestPlugin(t)
	if err := p.EnsureDevAdmin(context.Background(), DevAdminEmail, DevAdminPassword); err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(LoginRequest{Email: DevAdminEmail, Password: DevAdminPassword})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/login", bytes.NewReader(body))
	req.Header.Set("Accept", "application/json") // response no longer branches on Accept
	rec := httptest.NewRecorder()
	p.LoginHandler(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status: got %d, want 204: %s", rec.Code, rec.Body.String())
	}
	cookies := rec.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("cookies: got %d, want 1", len(cookies))
	}
	var rawCount, digestCount, tokenCount int
	if err := p.db.Read.QueryRow(`SELECT count(*) FROM sessions WHERE id = ?`, cookies[0].Value).Scan(&rawCount); err != nil {
		t.Fatal(err)
	}
	if err := p.db.Read.QueryRow(`SELECT count(*) FROM sessions WHERE id = ?`, digest(cookies[0].Value)).Scan(&digestCount); err != nil {
		t.Fatal(err)
	}
	if err := p.db.Read.QueryRow(`SELECT count(*) FROM api_tokens`).Scan(&tokenCount); err != nil {
		t.Fatal(err)
	}
	if rawCount != 0 || digestCount != 1 || tokenCount != 0 {
		t.Fatalf("login storage: raw_sessions=%d digested_sessions=%d tokens=%d", rawCount, digestCount, tokenCount)
	}

	cookieReq := httptest.NewRequest(http.MethodGet, "/", nil)
	cookieReq.AddCookie(cookies[0])
	cookiePrincipal, err := p.Authenticate(cookieReq)
	if err != nil || cookiePrincipal == nil {
		t.Fatalf("cookie authentication failed: principal=%v err=%v", cookiePrincipal, err)
	}
	if _, err := p.db.ExecWrite(context.Background(), `UPDATE users SET disabled = 1 WHERE id = ?`, cookiePrincipal.UserID); err != nil {
		t.Fatal(err)
	}
	if principal, err := p.Authenticate(cookieReq); err != nil || principal != nil {
		t.Fatalf("disabled user's cookie accepted: principal=%v err=%v", principal, err)
	}
}

func TestIssueSessionPrunesExpiredRows(t *testing.T) {
	p := openTestPlugin(t)
	if err := p.EnsureDevAdmin(context.Background(), DevAdminEmail, DevAdminPassword); err != nil {
		t.Fatal(err)
	}
	now := time.Now().Unix()
	if _, err := p.db.ExecWrite(context.Background(), `
		INSERT INTO sessions(id, user_id, created_at, expires_at, last_seen_at)
		VALUES ('expired', 1, 1, ?, 1), ('live', 1, 1, ?, 1)
	`, now-1, now+60); err != nil {
		t.Fatal(err)
	}
	if _, err := p.IssueSession(context.Background(), 1,
		httptest.NewRequest(http.MethodPost, "/api/login", nil)); err != nil {
		t.Fatal(err)
	}
	var expired, live, total int
	if err := p.db.Read.QueryRow(`SELECT COUNT(*) FROM sessions WHERE id = 'expired'`).Scan(&expired); err != nil {
		t.Fatal(err)
	}
	if err := p.db.Read.QueryRow(`SELECT COUNT(*) FROM sessions WHERE id = 'live'`).Scan(&live); err != nil {
		t.Fatal(err)
	}
	if err := p.db.Read.QueryRow(`SELECT COUNT(*) FROM sessions`).Scan(&total); err != nil {
		t.Fatal(err)
	}
	if expired != 0 || live != 1 || total != 2 {
		t.Fatalf("session cleanup: expired=%d live=%d total=%d, want 0/1/2", expired, live, total)
	}
}

func TestLoginHandler_RejectsUsernameField(t *testing.T) {
	p := openTestPlugin(t)
	body := []byte(`{"username":"admin@example.com","password":"password"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/login", bytes.NewReader(body))
	req.Header.Set("Accept", "application/json")
	rec := httptest.NewRecorder()
	p.LoginHandler(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "bad body") {
		t.Fatalf("body: got %q", rec.Body.String())
	}
}

func TestLoginFormHandlerAcceptsEmailField(t *testing.T) {
	p := openTestPlugin(t)
	p.cookieSecure = true
	if err := p.EnsureDevAdmin(context.Background(), DevAdminEmail, DevAdminPassword); err != nil {
		t.Fatal(err)
	}
	form := url.Values{"email": {DevAdminEmail}, "password": {DevAdminPassword}}
	req := httptest.NewRequest(http.MethodPost, "/login?next=%2F%2Fevil.example", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	p.LoginFormHandler(rec, req)
	if rec.Code != http.StatusFound {
		t.Fatalf("status: got %d, want 302: %s", rec.Code, rec.Body.String())
	}
	if location := rec.Header().Get("Location"); location != "/" {
		t.Fatalf("unsafe redirect accepted: %q", location)
	}
	if len(rec.Result().Cookies()) == 0 {
		t.Fatal("login did not issue a session cookie")
	}
	if !rec.Result().Cookies()[0].Secure {
		t.Fatal("HTTPS deployment did not issue a Secure session cookie")
	}
}

func TestSetupRejectsWeakAndExpiredCredentials(t *testing.T) {
	p := openTestPlugin(t)
	token := p.SetupToken()
	if _, err := p.applySetup(context.Background(), SetupRequest{
		Token: token, Email: "admin@example.com", Password: "short",
	}); err != errSetupWeakPassword {
		t.Fatalf("weak password error = %v", err)
	}
	p.setupTokenIssuedAt = time.Now().Add(-SetupTokenTTL)
	if _, err := p.applySetup(context.Background(), SetupRequest{
		Token: token, Email: "admin@example.com", Password: "long-enough",
	}); err != errSetupExpired {
		t.Fatalf("expired token error = %v", err)
	}
	if replacement := p.SetupToken(); replacement == "" || replacement == token {
		t.Fatal("expired setup token was not replaced")
	}
}
