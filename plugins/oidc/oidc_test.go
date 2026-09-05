package oidcauth

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/johnnybravo-xyz/suchi/core/auth"
	"github.com/johnnybravo-xyz/suchi/core/db"
	migrations "github.com/johnnybravo-xyz/suchi/core/db/migrations"
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
			// The test's authorization code carries its signed token fixture.
			_ = json.NewEncoder(w).Encode(map[string]any{
				"access_token": "test-access", "token_type": "Bearer", "id_token": r.FormValue("code"),
			})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(issuer.Close)
	p, err := New(t.Context(), Config{
		IssuerURL: issuer.URL, ClientID: "test-client", ClientSecret: "test-secret",
		PublicURL: "http://suchi.example.test", AdminEmail: "owner@example.test", IssueSession: local.IssueSession,
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

func TestOIDCAndLocalTokenDispatch(t *testing.T) {
	p, local, sign := newTestOIDC(t)
	token, err := local.IssueAPIToken(t.Context(), 1, "integration", auth.ScopeDocumentsRead)
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
			r.AddCookie(&http.Cookie{Name: localauth.CookieName, Value: session})
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

			callback := httptest.NewRequest(http.MethodGet, "/oidc/callback?state=test-state&code="+url.QueryEscape(raw), nil)
			callback.AddCookie(&http.Cookie{Name: stateCookie, Value: "test-state"})
			response := httptest.NewRecorder()
			p.CallbackHandler(response, callback)
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
