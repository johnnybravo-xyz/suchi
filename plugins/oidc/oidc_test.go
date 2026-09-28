// SPDX-License-Identifier: AGPL-3.0-or-later

package oidcauth

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/johnnybravo-xyz/suchi/core/auth"
	"github.com/johnnybravo-xyz/suchi/core/db"
	migrations "github.com/johnnybravo-xyz/suchi/core/db/migrations"
	"github.com/johnnybravo-xyz/suchi/core/settings"
	pluginapi "github.com/johnnybravo-xyz/suchi/plugin-api"
	localauth "github.com/johnnybravo-xyz/suchi/plugins/local-auth"
)

func newTestOIDC(t *testing.T) (*Plugin, *localauth.Plugin, func(map[string]any) string) {
	t.Helper()
	database, err := db.Open(t.Context(), filepath.Join(t.TempDir(), "suchi.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	migs, err := db.LoadMigrations(migrations.FS, ".")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Migrate(t.Context(), database, migs, log); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Write.Exec(`INSERT INTO users(id,email,display_name,role,created_at,updated_at)
		VALUES(1,'owner@example.test','Owner','admin',1,1)`); err != nil {
		t.Fatal(err)
	}
	local, err := localauth.NewWithOptions(t.Context(), database, log, false, false, localauth.Options{DisableSetup: true})
	if err != nil {
		t.Fatal(err)
	}
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	var tokenMu sync.Mutex
	usedCodes := make(map[string]struct{})
	issuer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/.well-known/openid-configuration":
			origin := "http://" + r.Host
			_ = json.NewEncoder(w).Encode(map[string]any{
				"issuer": origin, "authorization_endpoint": origin + "/authorize",
				"token_endpoint": origin + "/token", "jwks_uri": origin + "/keys",
				"id_token_signing_alg_values_supported": []string{"RS256"},
			})
		case "/keys":
			_ = json.NewEncoder(w).Encode(map[string]any{"keys": []any{map[string]any{
				"kty": "RSA", "kid": "test", "alg": "RS256", "use": "sig",
				"n": base64.RawURLEncoding.EncodeToString(key.N.Bytes()), "e": "AQAB",
			}}})
		case "/token":
			if r.FormValue("code_verifier") == "" {
				http.Error(w, "PKCE verifier required", http.StatusBadRequest)
				return
			}
			code := r.FormValue("code")
			tokenMu.Lock()
			_, reused := usedCodes[code]
			if !reused {
				usedCodes[code] = struct{}{}
			}
			tokenMu.Unlock()
			if reused {
				http.Error(w, "authorization code already used", http.StatusBadRequest)
				return
			}
			// The test's authorization code carries its signed token fixture.
			_ = json.NewEncoder(w).Encode(map[string]any{
				"access_token": "test-access", "token_type": "Bearer", "id_token": code,
			})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(issuer.Close)
	if _, err := database.Write.Exec(`
		UPDATE users SET oidc_issuer=?, oidc_subject='issuer-subject' WHERE id=1
	`, issuer.URL); err != nil {
		t.Fatal(err)
	}
	p, err := New(t.Context(), Config{
		IssuerURL: issuer.URL, ClientID: "test-client", ClientSecret: "test-secret",
		PublicURL: "http://suchi.example.test", AdminEmail: " OWNER@Example.Test ",
		IssueSession: local.IssueSession,
		PrepareSession: func(r *http.Request) (PreparedSession, error) {
			return local.PrepareSession(r)
		},
		EmailSyncAllowed:   func(*pluginapi.Principal) bool { return true },
		EmailChangeAllowed: func(_, _ string) bool { return true },
	}, database, log)
	if err != nil {
		t.Fatal(err)
	}
	sign := func(overrides map[string]any) string {
		t.Helper()
		claims := map[string]any{
			"iss": issuer.URL, "sub": "issuer-subject", "aud": "test-client",
			"exp": time.Now().Add(time.Hour).Unix(), "iat": time.Now().Unix(),
			"email": " Owner@Example.Test ", "email_verified": true, "name": "Issuer name",
		}
		for name, value := range overrides {
			if value == nil {
				delete(claims, name)
			} else {
				claims[name] = value
			}
		}
		body, err := json.Marshal(claims)
		if err != nil {
			t.Fatal(err)
		}
		unsigned := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","kid":"test"}`)) + "." + base64.RawURLEncoding.EncodeToString(body)
		digest := sha256.Sum256([]byte(unsigned))
		signature, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
		if err != nil {
			t.Fatal(err)
		}
		return unsigned + "." + base64.RawURLEncoding.EncodeToString(signature)
	}
	return p, local, sign
}

type oidcTestFlow struct {
	authorizationURL *url.URL
	cookie           *http.Cookie
}

func readOIDCTestFlow(t *testing.T, start *httptest.ResponseRecorder) oidcTestFlow {
	t.Helper()
	if start.Code != http.StatusFound || start.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("start status=%d headers=%v body=%s", start.Code, start.Header(), start.Body.String())
	}
	authorizationURL, err := url.Parse(start.Header().Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	query := authorizationURL.Query()
	if query.Get("state") == "" || query.Get("nonce") == "" ||
		query.Get("code_challenge") == "" || query.Get("code_challenge_method") != "S256" {
		t.Fatalf("authorization URL missing transaction binding: %s", authorizationURL)
	}
	var transactionCookie *http.Cookie
	for _, cookie := range start.Result().Cookies() {
		if cookie.Name == transactionCookieName {
			transactionCookie = cookie
		}
	}
	if transactionCookie == nil || !transactionCookie.HttpOnly ||
		transactionCookie.Domain != "" || transactionCookie.Path != "/oidc" ||
		transactionCookie.SameSite != http.SameSiteLaxMode ||
		transactionCookie.MaxAge != int(stateTTL/time.Second) {
		t.Fatalf("transaction cookie=%+v", transactionCookie)
	}
	return oidcTestFlow{authorizationURL: authorizationURL, cookie: transactionCookie}
}

func completeOIDCTestFlow(
	t *testing.T,
	p *Plugin,
	flow oidcTestFlow,
	rawIDToken string,
) *httptest.ResponseRecorder {
	t.Helper()
	query := flow.authorizationURL.Query()
	callback := httptest.NewRequest(http.MethodGet,
		"/oidc/callback?state="+url.QueryEscape(query.Get("state"))+
			"&code="+url.QueryEscape(rawIDToken), nil)
	callback.AddCookie(flow.cookie)
	response := httptest.NewRecorder()
	p.CallbackHandler(response, callback)
	return response
}

func oidcCallback(
	t *testing.T,
	p *Plugin,
	sign func(map[string]any) string,
	overrides map[string]any,
) *httptest.ResponseRecorder {
	t.Helper()
	start := httptest.NewRecorder()
	p.LoginHandler(start, httptest.NewRequest(http.MethodGet, "/oidc/login", nil))
	flow := readOIDCTestFlow(t, start)
	claims := make(map[string]any, len(overrides)+1)
	for name, value := range overrides {
		claims[name] = value
	}
	claims["nonce"] = flow.authorizationURL.Query().Get("nonce")
	return completeOIDCTestFlow(t, p, flow, sign(claims))
}
func issueOIDCTestSession(
	t *testing.T,
	local *localauth.Plugin,
	userID int64,
) (*http.Cookie, *pluginapi.Principal) {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	request.RemoteAddr = "192.0.2.10:1234"
	cookie, err := local.IssueSession(t.Context(), userID, request)
	if err != nil {
		t.Fatal(err)
	}
	authRequest := httptest.NewRequest(http.MethodGet, "/", nil)
	authRequest.AddCookie(cookie)
	principal, err := local.Authenticate(authRequest)
	if err != nil || principal == nil {
		t.Fatalf("authenticate issued session: principal=%+v err=%v", principal, err)
	}
	return cookie, principal
}

func startOIDCEmailSync(
	t *testing.T,
	p *Plugin,
	principal *pluginapi.Principal,
) oidcTestFlow {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, "/oidc/email-change", nil)
	request.Header.Set("Sec-Fetch-Site", "same-origin")
	request = request.WithContext(auth.WithPrincipal(request.Context(), principal))
	response := httptest.NewRecorder()
	p.EmailChangeHandler(response, request)
	flow := readOIDCTestFlow(t, response)
	if flow.authorizationURL.Query().Get("prompt") != "login" {
		t.Fatalf("email synchronization did not force provider login: %s", flow.authorizationURL)
	}
	return flow
}

func completeOIDCTestClaims(
	t *testing.T,
	p *Plugin,
	flow oidcTestFlow,
	sign func(map[string]any) string,
	overrides map[string]any,
) *httptest.ResponseRecorder {
	t.Helper()
	claims := make(map[string]any, len(overrides)+1)
	for name, value := range overrides {
		claims[name] = value
	}
	claims["nonce"] = flow.authorizationURL.Query().Get("nonce")
	return completeOIDCTestFlow(t, p, flow, sign(claims))
}

func responseCookie(response *httptest.ResponseRecorder, name string) *http.Cookie {
	for _, cookie := range response.Result().Cookies() {
		if cookie.Name == name {
			return cookie
		}
	}
	return nil
}

func TestOIDCAndLocalTokenDispatch(t *testing.T) {
	p, local, sign := newTestOIDC(t)
	var token string
	err := p.db.WriteTx(t.Context(), func(tx *sql.Tx) error {
		var err error
		token, err = local.IssueAPIToken(t.Context(), tx, 1, 1, "integration", auth.ScopeDocumentsRead, "")
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "/api/whoami", nil)
	session, err := local.IssueSession(t.Context(), 1, request)
	if err != nil {
		t.Fatal(err)
	}
	chain := auth.Chain{Authenticators: []pluginapi.Authenticator{p, local}}
	for _, tc := range []struct {
		name, header, kind string
	}{
		{"local_token", "Token " + token, "token"},
		{"local_bearer", "Bearer " + token, "token"},
		{"oidc_bearer", "Bearer " + sign(nil), "user"},
		{"unknown_local_bearer", "Bearer " + strings.Repeat("a", 64), ""},
		{"uppercase_hex", "Bearer " + strings.Repeat("A", 64), ""},
		{"malformed_jwt", "Bearer invalid.jwt.value", ""},
		{"missing_bearer_value", "Bearer", ""},
		{"empty_bearer", "Bearer ", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := request.Clone(context.Background())
			r.Header.Set("Authorization", tc.header)
			r.AddCookie(session)
			principal, err := chain.Authenticate(r)
			if tc.kind == "" {
				if err == nil || principal != nil {
					t.Fatalf("invalid credential fell through: principal=%+v err=%v", principal, err)
				}
				return
			}
			if err != nil || principal == nil || principal.Kind != tc.kind || principal.UserID != 1 {
				t.Fatalf("principal=%+v err=%v", principal, err)
			}
		})
	}
}

func TestOIDCRequiresVerifiedEmailBeforeAccountBinding(t *testing.T) {
	for _, tc := range []struct {
		name     string
		claims   map[string]any
		disabled bool
		allow    bool
	}{
		{"verified", nil, false, true},
		{"false", map[string]any{"email_verified": false}, false, false},
		{"missing", map[string]any{"email_verified": nil}, false, false},
		{"string_true", map[string]any{"email_verified": "true"}, false, false},
		{"missing_email", map[string]any{"email": nil}, false, false},
		{"unverified_new_account", map[string]any{"email": "new@example.test", "email_verified": false}, false, false},
		{"missing_subject", map[string]any{"sub": nil}, false, false},
		{"empty_subject", map[string]any{"sub": "  "}, false, false},
		{"wrong_audience", map[string]any{"aud": "another-client"}, false, false},
		{"expired", map[string]any{"exp": time.Now().Add(-time.Hour).Unix()}, false, false},
		{"disabled", nil, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, local, sign := newTestOIDC(t)
			if tc.disabled {
				if _, err := p.db.Write.Exec(`UPDATE users SET disabled=1 WHERE id=1`); err != nil {
					t.Fatal(err)
				}
			}
			raw := sign(tc.claims)
			r := httptest.NewRequest(http.MethodGet, "/api/whoami", nil)
			r.Header.Set("Authorization", "Bearer "+raw)
			principal, err := p.Authenticate(r)
			if tc.allow {
				if err != nil || principal == nil || principal.UserID != 1 || principal.Role != "admin" || principal.Display != "Owner" {
					t.Fatalf("verified existing account binding: principal=%+v err=%v", principal, err)
				}
			} else if err == nil || principal != nil {
				t.Fatalf("rejected claims admitted by bearer: principal=%+v err=%v", principal, err)
			}

			response := oidcCallback(t, p, sign, tc.claims)
			var session *http.Cookie
			for _, cookie := range response.Result().Cookies() {
				if cookie.Name == localauth.CookieName {
					session = cookie
				}
			}
			if tc.allow {
				if response.Code != http.StatusFound || session == nil {
					t.Fatalf("verified callback: status=%d body=%s", response.Code, response.Body.String())
				}
				request := httptest.NewRequest(http.MethodGet, "/api/whoami", nil)
				request.AddCookie(session)
				principal, err := local.Authenticate(request)
				if err != nil || principal == nil || principal.UserID != 1 || principal.Role != "admin" {
					t.Fatalf("callback session: principal=%+v err=%v", principal, err)
				}
			} else if response.Code < 400 || session != nil {
				t.Fatalf("rejected claims admitted by callback: status=%d session=%v", response.Code, session)
			}
			var users int
			if err := p.db.Read.QueryRow(`SELECT count(*) FROM users`).Scan(&users); err != nil || users != 1 {
				t.Fatalf("unexpected account created: users=%d err=%v", users, err)
			}
		})
	}
}
func TestOIDCBearerResolvesOnlyBoundSubjectAndUsesCanonicalProfile(t *testing.T) {
	p, _, sign := newTestOIDC(t)
	if _, err := p.db.Write.Exec(`
		UPDATE users
		SET email='canonical@example.test', display_name='Canonical', role='member'
		WHERE id=1
	`); err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "/api/whoami", nil)
	request.Header.Set("Authorization", "Bearer "+sign(map[string]any{
		"email": "claim@example.test", "name": "Claim Name",
	}))
	principal, err := p.Authenticate(request)
	if err != nil {
		t.Fatal(err)
	}
	if principal.Email != "canonical@example.test" || principal.Display != "Canonical" ||
		principal.Role != "member" || principal.AuthNBy != Name {
		t.Fatalf("principal used mutable claims instead of canonical row: %+v", principal)
	}
	response := oidcCallback(t, p, sign, map[string]any{
		"email": "different-claim@example.test", "name": "Different Claim",
	})
	if response.Code != http.StatusFound {
		t.Fatalf("existing-binding callback status=%d body=%s", response.Code, response.Body.String())
	}
	var canonicalEmail, canonicalDisplay string
	if err := p.db.Read.QueryRow(`SELECT email,display_name FROM users WHERE id=1`).
		Scan(&canonicalEmail, &canonicalDisplay); err != nil {
		t.Fatal(err)
	}
	if canonicalEmail != "canonical@example.test" || canonicalDisplay != "Canonical" {
		t.Fatalf("callback silently synchronized profile: email=%q display=%q",
			canonicalEmail, canonicalDisplay)
	}

	request.Header.Set("Authorization", "Bearer "+sign(map[string]any{
		"sub": "unknown-subject", "email": "unused@example.test",
	}))
	principal, err = p.Authenticate(request)
	if !errors.Is(err, errIdentityNotBound) || principal != nil {
		t.Fatalf("unknown bearer provisioned or resolved: principal=%+v err=%v", principal, err)
	}
	var users, bindingAudits int
	if err := p.db.Read.QueryRow(`SELECT count(*) FROM users`).Scan(&users); err != nil {
		t.Fatal(err)
	}
	if err := p.db.Read.QueryRow(`SELECT count(*) FROM audit_events WHERE action='user.oidc_bound'`).Scan(&bindingAudits); err != nil {
		t.Fatal(err)
	}
	if users != 1 || bindingAudits != 0 {
		t.Fatalf("resolution-only bearer wrote state: users=%d audits=%d", users, bindingAudits)
	}
}

func TestOIDCCallbackProvisionsUnusedSubjectAndRetainedBinding(t *testing.T) {
	p, _, sign := newTestOIDC(t)
	response := oidcCallback(t, p, sign, map[string]any{
		"sub": "new-subject", "email": " New@Example.Test ", "name": "New User",
	})
	if response.Code != http.StatusFound {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	var userID int64
	var email, display, role, issuer, subject string
	var password sql.NullString
	if err := p.db.Read.QueryRow(`
		SELECT id,email,display_name,role,oidc_issuer,oidc_subject,password_hash
		FROM users WHERE oidc_subject='new-subject'
	`).Scan(&userID, &email, &display, &role, &issuer, &subject, &password); err != nil {
		t.Fatal(err)
	}
	if email != "new@example.test" || display != "New User" || role != "member" ||
		issuer != p.cfg.IssuerURL || subject != "new-subject" || password.Valid {
		t.Fatalf("provisioned identity=(%d,%q,%q,%q,%q,%q,%v)",
			userID, email, display, role, issuer, subject, password)
	}
	var retained int
	var afterJSON string
	if err := p.db.Read.QueryRow(`
		SELECT retained,after_json FROM audit_events
		WHERE action='user.oidc_bound' AND object_id=?
	`, userID).Scan(&retained, &afterJSON); err != nil {
		t.Fatal(err)
	}
	if retained != 1 || !strings.Contains(afterJSON, `"email":"new@example.test"`) ||
		!strings.Contains(afterJSON, `"new_user":true`) ||
		strings.Contains(afterJSON, "new-subject") {
		t.Fatalf("binding audit retained=%d after=%s", retained, afterJSON)
	}
}

func TestOIDCCallbackNeverAdoptsOccupiedEmail(t *testing.T) {
	p, _, sign := newTestOIDC(t)
	if _, err := p.db.Write.Exec(`UPDATE users SET oidc_issuer=NULL,oidc_subject=NULL WHERE id=1`); err != nil {
		t.Fatal(err)
	}
	response := oidcCallback(t, p, sign, nil)
	if response.Code != http.StatusConflict {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	var issuer, subject sql.NullString
	if err := p.db.Read.QueryRow(`SELECT oidc_issuer,oidc_subject FROM users WHERE id=1`).Scan(&issuer, &subject); err != nil {
		t.Fatal(err)
	}
	var users, audits int
	if err := p.db.Read.QueryRow(`SELECT count(*) FROM users`).Scan(&users); err != nil {
		t.Fatal(err)
	}
	if err := p.db.Read.QueryRow(`SELECT count(*) FROM audit_events WHERE action='user.oidc_bound'`).Scan(&audits); err != nil {
		t.Fatal(err)
	}
	if issuer.Valid || subject.Valid || users != 1 || audits != 0 {
		t.Fatalf("occupied email was rebound: issuer=%v subject=%v users=%d audits=%d",
			issuer, subject, users, audits)
	}
}

func TestOIDCAdminBootstrapRequiresNoExistingAdministrator(t *testing.T) {
	t.Run("first administrator", func(t *testing.T) {
		p, _, sign := newTestOIDC(t)
		if _, err := p.db.Write.Exec(`DELETE FROM users`); err != nil {
			t.Fatal(err)
		}
		response := oidcCallback(t, p, sign, nil)
		if response.Code != http.StatusFound {
			t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
		}
		var role string
		if err := p.db.Read.QueryRow(`SELECT role FROM users WHERE email='owner@example.test'`).Scan(&role); err != nil || role != "admin" {
			t.Fatalf("role=%q err=%v", role, err)
		}
	})

	t.Run("existing administrator prevents reassignment", func(t *testing.T) {
		p, _, sign := newTestOIDC(t)
		if _, err := p.db.Write.Exec(`UPDATE users SET email='existing-admin@example.test' WHERE id=1`); err != nil {
			t.Fatal(err)
		}
		response := oidcCallback(t, p, sign, map[string]any{"sub": "later-subject"})
		if response.Code != http.StatusFound {
			t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
		}
		var role string
		if err := p.db.Read.QueryRow(`SELECT role FROM users WHERE oidc_subject='later-subject'`).Scan(&role); err != nil || role != "member" {
			t.Fatalf("role=%q err=%v", role, err)
		}
		var admins int
		if err := p.db.Read.QueryRow(`SELECT count(*) FROM users WHERE role='admin'`).Scan(&admins); err != nil || admins != 1 {
			t.Fatalf("administrators=%d err=%v", admins, err)
		}
	})
}

func TestOIDCProvisioningRollsBackWhenRetainedAuditFails(t *testing.T) {
	p, _, sign := newTestOIDC(t)
	if _, err := p.db.Write.Exec(`
		CREATE TRIGGER reject_oidc_binding_audit
		BEFORE INSERT ON audit_events
		WHEN NEW.action='user.oidc_bound'
		BEGIN SELECT RAISE(ABORT, 'binding audit unavailable'); END
	`); err != nil {
		t.Fatal(err)
	}
	response := oidcCallback(t, p, sign, map[string]any{
		"sub": "rollback-subject", "email": "rollback@example.test",
	})
	if response.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	var users int
	if err := p.db.Read.QueryRow(`SELECT count(*) FROM users WHERE email='rollback@example.test'`).Scan(&users); err != nil || users != 0 {
		t.Fatalf("rolled-back users=%d err=%v", users, err)
	}
}

func TestOIDCEmailChangeStartRequiresStrictOriginAndLiveSession(t *testing.T) {
	p, local, _ := newTestOIDC(t)
	_, principal := issueOIDCTestSession(t, local, 1)
	for _, tc := range []struct {
		name, fetchSite, origin, authorization string
		mutate                                 func(*pluginapi.Principal)
		revoke                                 bool
		want                                   int
	}{
		{name: "fetch metadata", fetchSite: "same-origin", want: http.StatusFound},
		{name: "matching origin fallback", origin: "http://suchi.example.test", want: http.StatusFound},
		{name: "missing provenance", want: http.StatusForbidden},
		{name: "cross site", fetchSite: "cross-site", origin: "http://suchi.example.test", want: http.StatusForbidden},
		{name: "expired proof", fetchSite: "same-origin", mutate: func(p *pluginapi.Principal) { p.AuthExpiresAt = 1 }, want: http.StatusUnauthorized},
		{name: "token principal", fetchSite: "same-origin", mutate: func(p *pluginapi.Principal) { p.Kind = "token" }, want: http.StatusUnauthorized},
		{name: "authorization header", fetchSite: "same-origin", authorization: "Bearer external", want: http.StatusUnauthorized},
		{name: "revoked session", fetchSite: "same-origin", revoke: true, want: http.StatusUnauthorized},
	} {
		t.Run(tc.name, func(t *testing.T) {
			current := *principal
			if tc.mutate != nil {
				tc.mutate(&current)
			}
			if tc.revoke {
				if _, err := p.db.Write.Exec(`DELETE FROM sessions WHERE id=?`, current.SessionID); err != nil {
					t.Fatal(err)
				}
			}
			request := httptest.NewRequest(http.MethodPost, "/oidc/email-change", nil)
			request.Header.Set("Sec-Fetch-Site", tc.fetchSite)
			request.Header.Set("Origin", tc.origin)
			request.Header.Set("Authorization", tc.authorization)
			request = request.WithContext(auth.WithPrincipal(request.Context(), &current))
			response := httptest.NewRecorder()
			p.EmailChangeHandler(response, request)
			if response.Code != tc.want || response.Header().Get("Cache-Control") != "no-store" {
				t.Fatalf("status=%d headers=%v body=%s", response.Code, response.Header(), response.Body.String())
			}
			if tc.want == http.StatusFound {
				flow := readOIDCTestFlow(t, response)
				if flow.authorizationURL.Query().Get("prompt") != "login" {
					t.Fatalf("prompt=%q", flow.authorizationURL.Query().Get("prompt"))
				}
			}
		})
	}

	_, principal = issueOIDCTestSession(t, local, 1)
	p.cfg.EmailSyncAllowed = func(*pluginapi.Principal) bool { return false }
	request := httptest.NewRequest(http.MethodPost, "/oidc/email-change", nil)
	request.Header.Set("Sec-Fetch-Site", "same-origin")
	request = request.WithContext(auth.WithPrincipal(request.Context(), principal))
	response := httptest.NewRecorder()
	p.EmailChangeHandler(response, request)
	if response.Code != http.StatusConflict {
		t.Fatalf("disabled mode status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestOIDCEmailSyncChangesEmailAtomicallyAndRotatesBrowserSessions(t *testing.T) {
	p, local, sign := newTestOIDC(t)
	firstCookie, principal := issueOIDCTestSession(t, local, 1)
	secondCookie, _ := issueOIDCTestSession(t, local, 1)
	var apiToken string
	if err := p.db.WriteTx(t.Context(), func(tx *sql.Tx) error {
		var err error
		apiToken, err = local.IssueAPIToken(t.Context(), tx, 1, 1, "mobile", auth.ScopeDocumentsRead, "")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := settings.Set(t.Context(), p.db, settings.KeyFSWatchOwnerEmail, "owner@example.test"); err != nil {
		t.Fatal(err)
	}
	reloads := 0
	p.cfg.FSWatchReloader = func(context.Context) error {
		reloads++
		return errors.New("reload warning")
	}

	flow := startOIDCEmailSync(t, p, principal)
	response := completeOIDCTestClaims(t, p, flow, sign, map[string]any{
		"email": " Changed@Example.Test ",
	})
	if response.Code != http.StatusFound || response.Header().Get("Location") != accountNoticeChanged ||
		response.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("status=%d location=%q headers=%v body=%s",
			response.Code, response.Header().Get("Location"), response.Header(), response.Body.String())
	}
	replacement := responseCookie(response, localauth.CookieName)
	if replacement == nil || reloads != 1 {
		t.Fatalf("replacement=%v reloads=%d", replacement, reloads)
	}
	authRequest := httptest.NewRequest(http.MethodGet, "/", nil)
	authRequest.AddCookie(replacement)
	replacementPrincipal, err := local.Authenticate(authRequest)
	if err != nil || replacementPrincipal == nil || replacementPrincipal.UserID != 1 {
		t.Fatalf("replacement principal=%+v err=%v", replacementPrincipal, err)
	}

	var email, owner string
	if err := p.db.Read.QueryRow(`SELECT email FROM users WHERE id=1`).Scan(&email); err != nil {
		t.Fatal(err)
	}
	if err := settings.Get(t.Context(), p.db, settings.KeyFSWatchOwnerEmail, &owner); err != nil {
		t.Fatal(err)
	}
	var sessions, tokens int
	if err := p.db.Read.QueryRow(`SELECT count(*) FROM sessions WHERE user_id=1`).Scan(&sessions); err != nil {
		t.Fatal(err)
	}
	if err := p.db.Read.QueryRow(`SELECT count(*) FROM api_tokens WHERE user_id=1`).Scan(&tokens); err != nil {
		t.Fatal(err)
	}
	if email != "changed@example.test" || owner != email || sessions != 1 || tokens != 1 || apiToken == "" {
		t.Fatalf("email=%q owner=%q sessions=%d tokens=%d api_token_empty=%v",
			email, owner, sessions, tokens, apiToken == "")
	}
	for _, oldCookie := range []*http.Cookie{firstCookie, secondCookie} {
		request := httptest.NewRequest(http.MethodGet, "/", nil)
		request.AddCookie(oldCookie)
		oldPrincipal, _ := local.Authenticate(request)
		if oldPrincipal != nil {
			t.Fatalf("old browser session survived rotation: %+v", oldPrincipal)
		}
	}

	var retained int
	var afterJSON string
	if err := p.db.Read.QueryRow(`
		SELECT retained,after_json FROM audit_events WHERE action='user.email_changed'
	`).Scan(&retained, &afterJSON); err != nil {
		t.Fatal(err)
	}
	if retained != 1 || !strings.Contains(afterJSON, `"email":"changed@example.test"`) ||
		!strings.Contains(afterJSON, `"revoked_sessions":2`) ||
		!strings.Contains(afterJSON, `"source":"oidc"`) {
		t.Fatalf("retained=%d after=%s", retained, afterJSON)
	}
}

func TestOIDCEmailSyncLeavesUnchangedIdentityAndSessionAlone(t *testing.T) {
	p, local, sign := newTestOIDC(t)
	_, principal := issueOIDCTestSession(t, local, 1)
	reloads := 0
	p.cfg.FSWatchReloader = func(context.Context) error {
		reloads++
		return nil
	}
	flow := startOIDCEmailSync(t, p, principal)
	response := completeOIDCTestClaims(t, p, flow, sign, nil)
	if response.Code != http.StatusFound || response.Header().Get("Location") != accountNoticeChecked {
		t.Fatalf("status=%d location=%q body=%s", response.Code, response.Header().Get("Location"), response.Body.String())
	}
	if responseCookie(response, localauth.CookieName) != nil || reloads != 0 {
		t.Fatalf("unchanged identity rotated session or reloaded watcher: headers=%v reloads=%d", response.Header(), reloads)
	}
	var sessions, identityEvents int
	if err := p.db.Read.QueryRow(`SELECT count(*) FROM sessions WHERE id=?`, principal.SessionID).Scan(&sessions); err != nil {
		t.Fatal(err)
	}
	if err := p.db.Read.QueryRow(`
		SELECT count(*) FROM audit_events
		WHERE action IN ('user.email_changed','user.oidc_bound')
	`).Scan(&identityEvents); err != nil {
		t.Fatal(err)
	}
	if sessions != 1 || identityEvents != 0 {
		t.Fatalf("sessions=%d identity_events=%d", sessions, identityEvents)
	}
}

func TestOIDCEmailSyncBindsEligibleUnboundCurrentUser(t *testing.T) {
	p, local, sign := newTestOIDC(t)
	if _, err := p.db.Write.Exec(`UPDATE users SET oidc_issuer=NULL,oidc_subject=NULL WHERE id=1`); err != nil {
		t.Fatal(err)
	}
	_, principal := issueOIDCTestSession(t, local, 1)
	flow := startOIDCEmailSync(t, p, principal)
	response := completeOIDCTestClaims(t, p, flow, sign, nil)
	if response.Code != http.StatusFound || response.Header().Get("Location") != accountNoticeBound ||
		responseCookie(response, localauth.CookieName) == nil {
		t.Fatalf("status=%d location=%q headers=%v body=%s",
			response.Code, response.Header().Get("Location"), response.Header(), response.Body.String())
	}
	var issuer, subject string
	if err := p.db.Read.QueryRow(`SELECT oidc_issuer,oidc_subject FROM users WHERE id=1`).Scan(&issuer, &subject); err != nil {
		t.Fatal(err)
	}
	var retained int
	var afterJSON string
	if err := p.db.Read.QueryRow(`
		SELECT retained,after_json FROM audit_events WHERE action='user.oidc_bound'
	`).Scan(&retained, &afterJSON); err != nil {
		t.Fatal(err)
	}
	if issuer != p.cfg.IssuerURL || subject != "issuer-subject" || retained != 1 ||
		!strings.Contains(afterJSON, `"new_user":false`) ||
		strings.Contains(afterJSON, "issuer-subject") {
		t.Fatalf("issuer=%q subject=%q retained=%d after=%s", issuer, subject, retained, afterJSON)
	}
}

func TestOIDCEmailSyncRejectsConflictsPinnedOwnerAndStaleSession(t *testing.T) {
	t.Run("different bound subject", func(t *testing.T) {
		p, local, sign := newTestOIDC(t)
		_, principal := issueOIDCTestSession(t, local, 1)
		flow := startOIDCEmailSync(t, p, principal)
		response := completeOIDCTestClaims(t, p, flow, sign, map[string]any{
			"sub": "attacker-subject", "email": "attacker@example.test",
		})
		if response.Code != http.StatusConflict {
			t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
		}
	})

	t.Run("occupied target email", func(t *testing.T) {
		p, local, sign := newTestOIDC(t)
		if _, err := p.db.Write.Exec(`
			INSERT INTO users(id,email,display_name,role,created_at,updated_at)
			VALUES(2,'occupied@example.test','Occupied','member',0,0)
		`); err != nil {
			t.Fatal(err)
		}
		_, principal := issueOIDCTestSession(t, local, 1)
		flow := startOIDCEmailSync(t, p, principal)
		response := completeOIDCTestClaims(t, p, flow, sign, map[string]any{
			"email": "occupied@example.test",
		})
		if response.Code != http.StatusConflict {
			t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
		}
	})

	t.Run("unbound email mismatch", func(t *testing.T) {
		p, local, sign := newTestOIDC(t)
		if _, err := p.db.Write.Exec(`UPDATE users SET oidc_issuer=NULL,oidc_subject=NULL WHERE id=1`); err != nil {
			t.Fatal(err)
		}
		_, principal := issueOIDCTestSession(t, local, 1)
		flow := startOIDCEmailSync(t, p, principal)
		response := completeOIDCTestClaims(t, p, flow, sign, map[string]any{
			"email": "different@example.test",
		})
		if response.Code != http.StatusConflict {
			t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
		}
		var issuer sql.NullString
		if err := p.db.Read.QueryRow(`SELECT oidc_issuer FROM users WHERE id=1`).Scan(&issuer); err != nil || issuer.Valid {
			t.Fatalf("issuer=%v err=%v", issuer, err)
		}
	})

	t.Run("pinned watched-folder owner", func(t *testing.T) {
		p, local, sign := newTestOIDC(t)
		p.cfg.EmailChangeAllowed = func(_, _ string) bool { return false }
		_, principal := issueOIDCTestSession(t, local, 1)
		flow := startOIDCEmailSync(t, p, principal)
		response := completeOIDCTestClaims(t, p, flow, sign, map[string]any{
			"email": "changed@example.test",
		})
		if response.Code != http.StatusConflict {
			t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
		}
	})

	t.Run("session revoked during provider round trip", func(t *testing.T) {
		p, local, sign := newTestOIDC(t)
		_, principal := issueOIDCTestSession(t, local, 1)
		flow := startOIDCEmailSync(t, p, principal)
		if _, err := p.db.Write.Exec(`DELETE FROM sessions WHERE id=?`, principal.SessionID); err != nil {
			t.Fatal(err)
		}
		response := completeOIDCTestClaims(t, p, flow, sign, map[string]any{
			"email": "changed@example.test",
		})
		if response.Code != http.StatusUnauthorized {
			t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
		}
	})

	t.Run("account disabled during provider round trip", func(t *testing.T) {
		p, local, sign := newTestOIDC(t)
		_, principal := issueOIDCTestSession(t, local, 1)
		flow := startOIDCEmailSync(t, p, principal)
		if _, err := p.db.Write.Exec(`UPDATE users SET disabled=1 WHERE id=1`); err != nil {
			t.Fatal(err)
		}
		response := completeOIDCTestClaims(t, p, flow, sign, map[string]any{
			"email": "changed@example.test",
		})
		if response.Code != http.StatusUnauthorized {
			t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
		}
	})

	t.Run("mode disabled during provider round trip", func(t *testing.T) {
		p, local, sign := newTestOIDC(t)
		_, principal := issueOIDCTestSession(t, local, 1)
		flow := startOIDCEmailSync(t, p, principal)
		p.cfg.EmailSyncAllowed = func(*pluginapi.Principal) bool { return false }
		response := completeOIDCTestClaims(t, p, flow, sign, map[string]any{
			"email": "changed@example.test",
		})
		if response.Code != http.StatusConflict {
			t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
		}
	})
}

func TestOIDCEmailSyncRollsBackWhenRequiredAuditFails(t *testing.T) {
	p, local, sign := newTestOIDC(t)
	_, principal := issueOIDCTestSession(t, local, 1)
	if err := settings.Set(t.Context(), p.db, settings.KeyFSWatchOwnerEmail, "owner@example.test"); err != nil {
		t.Fatal(err)
	}
	if _, err := p.db.Write.Exec(`
		CREATE TRIGGER reject_oidc_email_audit
		BEFORE INSERT ON audit_events
		WHEN NEW.action='user.email_changed'
		BEGIN SELECT RAISE(ABORT, 'email audit unavailable'); END
	`); err != nil {
		t.Fatal(err)
	}
	flow := startOIDCEmailSync(t, p, principal)
	response := completeOIDCTestClaims(t, p, flow, sign, map[string]any{
		"email": "changed@example.test",
	})
	if response.Code != http.StatusInternalServerError ||
		responseCookie(response, localauth.CookieName) != nil {
		t.Fatalf("status=%d headers=%v body=%s", response.Code, response.Header(), response.Body.String())
	}
	var email, owner string
	if err := p.db.Read.QueryRow(`SELECT email FROM users WHERE id=1`).Scan(&email); err != nil {
		t.Fatal(err)
	}
	if err := settings.Get(t.Context(), p.db, settings.KeyFSWatchOwnerEmail, &owner); err != nil {
		t.Fatal(err)
	}
	var sessions int
	if err := p.db.Read.QueryRow(`SELECT count(*) FROM sessions WHERE id=?`, principal.SessionID).Scan(&sessions); err != nil {
		t.Fatal(err)
	}
	if email != "owner@example.test" || owner != email || sessions != 1 {
		t.Fatalf("rollback email=%q owner=%q sessions=%d", email, owner, sessions)
	}
}

func TestOIDCTransactionCookieUsesConfiguredSecurePolicy(t *testing.T) {
	p, _, _ := newTestOIDC(t)
	p.cfg.CookieSecure = true
	start := httptest.NewRecorder()
	p.LoginHandler(start, httptest.NewRequest(http.MethodGet, "/oidc/login", nil))
	if flow := readOIDCTestFlow(t, start); !flow.cookie.Secure {
		t.Fatal("transaction cookie is not Secure")
	}
}

func TestOIDCAuthorizationTransactionRejectsTamperingExpiryStateAndNonce(t *testing.T) {
	assertRejected := func(t *testing.T, response *httptest.ResponseRecorder) {
		t.Helper()
		if response.Code != http.StatusBadRequest || response.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("status=%d headers=%v body=%s", response.Code, response.Header(), response.Body.String())
		}
		expired := responseCookie(response, transactionCookieName)
		if expired == nil || expired.MaxAge >= 0 || expired.Path != "/oidc" {
			t.Fatalf("transaction cookie was not expired: %+v", expired)
		}
	}

	t.Run("tampered cookie", func(t *testing.T) {
		p, _, sign := newTestOIDC(t)
		start := httptest.NewRecorder()
		p.LoginHandler(start, httptest.NewRequest(http.MethodGet, "/oidc/login", nil))
		flow := readOIDCTestFlow(t, start)
		tampered := *flow.cookie
		replacement := byte('A')
		if tampered.Value[0] == replacement {
			replacement = 'B'
		}
		tampered.Value = string(replacement) + tampered.Value[1:]
		flow.cookie = &tampered
		raw := sign(map[string]any{"nonce": flow.authorizationURL.Query().Get("nonce")})
		assertRejected(t, completeOIDCTestFlow(t, p, flow, raw))
	})

	t.Run("state mismatch", func(t *testing.T) {
		p, _, sign := newTestOIDC(t)
		start := httptest.NewRecorder()
		p.LoginHandler(start, httptest.NewRequest(http.MethodGet, "/oidc/login", nil))
		flow := readOIDCTestFlow(t, start)
		changed := *flow.authorizationURL
		query := changed.Query()
		query.Set("state", "wrong-state")
		changed.RawQuery = query.Encode()
		flow.authorizationURL = &changed
		raw := sign(map[string]any{"nonce": query.Get("nonce")})
		assertRejected(t, completeOIDCTestFlow(t, p, flow, raw))
	})

	t.Run("expired transaction", func(t *testing.T) {
		p, _, sign := newTestOIDC(t)
		now := time.Now()
		transaction := authorizationTransaction{
			Version: transactionVersion, Purpose: purposeLogin,
			IssuedAt:  now.Add(-stateTTL - time.Second).Unix(),
			ExpiresAt: now.Add(-time.Second).Unix(),
			State:     "expired-state", Nonce: "expired-nonce", Verifier: "expired-verifier",
		}
		value, err := p.signTransaction(transaction)
		if err != nil {
			t.Fatal(err)
		}
		flow := oidcTestFlow{
			authorizationURL: &url.URL{Path: "/authorize", RawQuery: url.Values{
				"state": []string{transaction.State}, "nonce": []string{transaction.Nonce},
			}.Encode()},
			cookie: &http.Cookie{Name: transactionCookieName, Value: value},
		}
		assertRejected(t, completeOIDCTestFlow(t, p, flow, sign(map[string]any{"nonce": transaction.Nonce})))
	})

	t.Run("process restart key", func(t *testing.T) {
		p, _, sign := newTestOIDC(t)
		start := httptest.NewRecorder()
		p.LoginHandler(start, httptest.NewRequest(http.MethodGet, "/oidc/login", nil))
		flow := readOIDCTestFlow(t, start)
		restarted := *p
		restarted.transactionKey[0] ^= 0xff
		raw := sign(map[string]any{"nonce": flow.authorizationURL.Query().Get("nonce")})
		assertRejected(t, completeOIDCTestFlow(t, &restarted, flow, raw))
	})

	t.Run("nonce mismatch", func(t *testing.T) {
		p, _, sign := newTestOIDC(t)
		start := httptest.NewRecorder()
		p.LoginHandler(start, httptest.NewRequest(http.MethodGet, "/oidc/login", nil))
		flow := readOIDCTestFlow(t, start)
		assertRejected(t, completeOIDCTestFlow(t, p, flow, sign(map[string]any{"nonce": "wrong-nonce"})))
	})

	t.Run("provider error", func(t *testing.T) {
		p, _, _ := newTestOIDC(t)
		start := httptest.NewRecorder()
		p.LoginHandler(start, httptest.NewRequest(http.MethodGet, "/oidc/login", nil))
		flow := readOIDCTestFlow(t, start)
		callback := httptest.NewRequest(http.MethodGet,
			"/oidc/callback?state="+url.QueryEscape(flow.authorizationURL.Query().Get("state"))+
				"&error=access_denied", nil)
		callback.AddCookie(flow.cookie)
		response := httptest.NewRecorder()
		p.CallbackHandler(response, callback)
		assertRejected(t, response)
	})
}

func TestOIDCDuplicateCallbackMutatesIdentityAtMostOnce(t *testing.T) {
	p, _, sign := newTestOIDC(t)
	start := httptest.NewRecorder()
	p.LoginHandler(start, httptest.NewRequest(http.MethodGet, "/oidc/login", nil))
	flow := readOIDCTestFlow(t, start)
	raw := sign(map[string]any{
		"nonce": flow.authorizationURL.Query().Get("nonce"),
		"sub":   "one-use-subject", "email": "one-use@example.test",
	})
	first := completeOIDCTestFlow(t, p, flow, raw)
	if first.Code != http.StatusFound {
		t.Fatalf("first status=%d body=%s", first.Code, first.Body.String())
	}
	second := completeOIDCTestFlow(t, p, flow, raw)
	if second.Code != http.StatusBadGateway {
		t.Fatalf("second status=%d body=%s", second.Code, second.Body.String())
	}
	var users, audits int
	if err := p.db.Read.QueryRow(`SELECT count(*) FROM users WHERE email='one-use@example.test'`).Scan(&users); err != nil {
		t.Fatal(err)
	}
	if err := p.db.Read.QueryRow(`SELECT count(*) FROM audit_events WHERE action='user.oidc_bound'`).Scan(&audits); err != nil {
		t.Fatal(err)
	}
	if users != 1 || audits != 1 {
		t.Fatalf("users=%d audits=%d", users, audits)
	}
}
