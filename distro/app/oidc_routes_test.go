// SPDX-License-Identifier: AGPL-3.0-or-later

package app_test

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"
)

func TestConfiguredOIDCCallbackRouteCompletesLogin(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		origin := "http://" + r.Host
		switch r.URL.Path {
		case "/.well-known/openid-configuration":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"issuer": origin, "authorization_endpoint": origin + "/authorize",
				"token_endpoint": origin + "/token", "jwks_uri": origin + "/keys",
				"id_token_signing_alg_values_supported": []string{"RS256"},
			})
		case "/keys":
			_ = json.NewEncoder(w).Encode(map[string]any{"keys": []any{map[string]any{
				"kty": "RSA", "kid": "route-test", "alg": "RS256", "use": "sig",
				"n": base64.RawURLEncoding.EncodeToString(key.N.Bytes()), "e": "AQAB",
			}}})
		case "/token":
			if r.FormValue("code_verifier") == "" {
				http.Error(w, "PKCE verifier required", http.StatusBadRequest)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"access_token": "route-test", "token_type": "Bearer",
				"id_token": r.FormValue("code"),
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer provider.Close()

	opts := options(t)
	opts.Config.DevMode = false
	opts.Config.OIDCIssuerURL = provider.URL
	opts.Config.OIDCClientID = "route-client"
	opts.Config.OIDCClientSecret = "route-secret"
	opts.Config.AdminEmail = "owner@example.test"
	running := startAppWithoutLogin(t, opts)
	client := &http.Client{
		Jar:     running.client.Jar,
		Timeout: 3 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	start, err := client.Get(running.url + "/oidc/login")
	if err != nil {
		t.Fatal(err)
	}
	_ = start.Body.Close()
	if start.StatusCode != http.StatusFound {
		t.Fatalf("OIDC login route status=%d", start.StatusCode)
	}
	authorizationURL, err := url.Parse(start.Header.Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	query := authorizationURL.Query()
	if query.Get("state") == "" || query.Get("nonce") == "" || query.Get("code_challenge") == "" {
		t.Fatalf("OIDC authorization redirect is not transaction-bound: %s", authorizationURL)
	}
	claims := map[string]any{
		"iss": provider.URL, "sub": "route-subject", "aud": "route-client",
		"exp": time.Now().Add(time.Hour).Unix(), "iat": time.Now().Unix(),
		"nonce": query.Get("nonce"), "email": "Owner@Example.Test",
		"email_verified": true, "name": "Route owner",
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	unsigned := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","kid":"route-test"}`)) +
		"." + base64.RawURLEncoding.EncodeToString(payload)
	digest := sha256.Sum256([]byte(unsigned))
	signature, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	token := unsigned + "." + base64.RawURLEncoding.EncodeToString(signature)

	callback, err := client.Get(running.url + "/oidc/callback?state=" +
		url.QueryEscape(query.Get("state")) + "&code=" + url.QueryEscape(token))
	if err != nil {
		t.Fatal(err)
	}
	_ = callback.Body.Close()
	if callback.StatusCode != http.StatusFound || callback.Header.Get("Location") != "/" {
		t.Fatalf("OIDC callback route status=%d location=%q", callback.StatusCode, callback.Header.Get("Location"))
	}

	var self struct {
		Email string `json:"email"`
		Role  string `json:"role"`
	}
	if err := json.Unmarshal(
		running.request(t, http.MethodGet, "/api/whoami", "", http.StatusOK),
		&self,
	); err != nil {
		t.Fatal(err)
	}
	if self.Email != "owner@example.test" || self.Role != "admin" {
		t.Fatalf("whoami after configured OIDC callback = %+v", self)
	}
}
