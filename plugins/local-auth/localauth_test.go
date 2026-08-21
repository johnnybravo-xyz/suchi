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
	p, err := New(ctx, d, log, false, false)
	if err != nil {
		t.Fatal(err)
	}
	return p
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

func TestLoginHandler_AcceptsEmailAndIssuesGranularScopes(t *testing.T) {
	p := openTestPlugin(t)
	if err := p.EnsureDevAdmin(context.Background(), DevAdminEmail, DevAdminPassword); err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(LoginRequest{Email: DevAdminEmail, Password: DevAdminPassword})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/login", bytes.NewReader(body))
	req.Header.Set("Accept", "application/json")
	rec := httptest.NewRecorder()
	p.LoginHandler(rec, req)
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
	cookies := rec.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("cookies: got %d, want 1", len(cookies))
	}
	var rawCount, digestCount int
	if err := p.db.Read.QueryRow(`SELECT count(*) FROM sessions WHERE id = ?`, cookies[0].Value).Scan(&rawCount); err != nil {
		t.Fatal(err)
	}
	if err := p.db.Read.QueryRow(`SELECT count(*) FROM sessions WHERE id = ?`, digest(cookies[0].Value)).Scan(&digestCount); err != nil {
		t.Fatal(err)
	}
	if rawCount != 0 || digestCount != 1 {
		t.Fatalf("session storage: raw=%d digest=%d, want raw=0 digest=1", rawCount, digestCount)
	}

	cookieReq := httptest.NewRequest(http.MethodGet, "/", nil)
	cookieReq.AddCookie(cookies[0])
	cookiePrincipal, err := p.Authenticate(cookieReq)
	if err != nil || cookiePrincipal == nil {
		t.Fatalf("cookie authentication failed: principal=%v err=%v", cookiePrincipal, err)
	}
	tokenReq := httptest.NewRequest(http.MethodGet, "/api/documents/", nil)
	tokenReq.Header.Set("Authorization", "Token "+response.Token)
	tokenPrincipal, err := p.Authenticate(tokenReq)
	if err != nil || tokenPrincipal == nil {
		t.Fatalf("token authentication failed: principal=%v err=%v", tokenPrincipal, err)
	}

	if _, err := p.db.ExecWrite(context.Background(), `UPDATE users SET disabled = 1 WHERE id = ?`, cookiePrincipal.UserID); err != nil {
		t.Fatal(err)
	}
	if principal, err := p.Authenticate(cookieReq); err != nil || principal != nil {
		t.Fatalf("disabled user's cookie accepted: principal=%v err=%v", principal, err)
	}
	if principal, err := p.Authenticate(tokenReq); err == nil || principal != nil {
		t.Fatalf("disabled user's token accepted: principal=%v err=%v", principal, err)
	}
	if _, err := p.db.ExecWrite(context.Background(), `UPDATE users SET disabled = 0 WHERE id = ?`, cookiePrincipal.UserID); err != nil {
		t.Fatal(err)
	}

	logoutReq := httptest.NewRequest(http.MethodPost, "/api/logout", nil)
	logoutReq.AddCookie(cookies[0])
	logoutReq = logoutReq.WithContext(auth.WithPrincipal(logoutReq.Context(), tokenPrincipal))
	logoutRec := httptest.NewRecorder()
	p.LogoutHandler(logoutRec, logoutReq)
	if logoutRec.Code != http.StatusNoContent {
		t.Fatalf("logout status: got %d, want 204", logoutRec.Code)
	}
	if err := p.db.Read.QueryRow(`SELECT count(*) FROM sessions WHERE id = ?`, digest(cookies[0].Value)).Scan(&digestCount); err != nil {
		t.Fatal(err)
	}
	var revokedCount int
	if err := p.db.Read.QueryRow(`SELECT count(*) FROM api_tokens WHERE id = ? AND revoked_at IS NOT NULL`, tokenPrincipal.TokenID).Scan(&revokedCount); err != nil {
		t.Fatal(err)
	}
	if digestCount != 0 || revokedCount != 1 {
		t.Fatalf("logout cleanup: sessions=%d revoked_tokens=%d", digestCount, revokedCount)
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
