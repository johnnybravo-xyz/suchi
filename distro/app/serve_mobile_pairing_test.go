// SPDX-License-Identifier: AGPL-3.0-or-later

package app

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image/png"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"github.com/johnnybravo-xyz/suchi/core/api"
	"github.com/johnnybravo-xyz/suchi/core/auth"
	"github.com/johnnybravo-xyz/suchi/core/config"
	"github.com/johnnybravo-xyz/suchi/core/db"
	"github.com/johnnybravo-xyz/suchi/core/db/migrations"
	"github.com/johnnybravo-xyz/suchi/core/httpx"
	pluginapi "github.com/johnnybravo-xyz/suchi/plugin-api"
	localauth "github.com/johnnybravo-xyz/suchi/plugins/local-auth"
	"github.com/makiuchi-d/gozxing"
	"github.com/makiuchi-d/gozxing/qrcode"
)

func TestMobilePairingAssembledGuards(t *testing.T) {
	d := newDemoTestDB(t)
	for _, tc := range []struct {
		name, method, path, kind, site string
		want                           int
	}{
		{"anonymous cannot create", "POST", "/api/mobile/pairing", "", "", 401},
		{"anonymous cannot cancel", "DELETE", "/api/mobile/pairing", "", "", 401},
		{"token cannot create", "POST", "/api/mobile/pairing", "token", "", 403},
		{"token cannot create slash", "POST", "/api/mobile/pairing/", "token", "", 403},
		{"token cannot cancel", "DELETE", "/api/mobile/pairing", "token", "", 403},
		{"scratch cannot create", "POST", "/api/mobile/pairing", "demo-scratch", "", 403},
		{"demo cannot create", "POST", "/api/mobile/pairing", "demo-anon", "", 403},
		{"session reaches issuer check", "POST", "/api/mobile/pairing", "user", "same-origin", 501},
		{"cross-origin create refused", "POST", "/api/mobile/pairing", "user", "cross-site", 403},
		{"sibling-origin cancel refused", "DELETE", "/api/mobile/pairing", "user", "same-site", 403},
		{"exchange is public", "POST", "/api/mobile/pairing/exchange", "", "", 400},
		{"exchange slash is public", "POST", "/api/mobile/pairing/exchange/", "", "", 400},
		{"token does not replace pairing code", "POST", "/api/mobile/pairing/exchange", "token", "", 400},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var logs bytes.Buffer
			log := slog.New(slog.NewJSONHandler(&logs, nil))
			s := &api.Server{DB: d, Log: log}
			mux := http.NewServeMux()
			s.Register(mux)
			h := buildHTTPHandler(mux, &config.Config{BodyLimit: 1024}, &auth.Chain{}, nil, httpx.NewMetrics(), log)
			r := httptest.NewRequest(tc.method, tc.path+"?private=DO_NOT_LOG_PAIRING", strings.NewReader(`{"code":"DO_NOT_LOG_PAIRING"}`))
			r.Header.Set("Sec-Fetch-Site", tc.site)
			if tc.kind != "" {
				r = r.WithContext(auth.WithPrincipal(r.Context(), &pluginapi.Principal{Kind: tc.kind, UserID: 1, Role: "admin"}))
			}
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != tc.want {
				t.Fatalf("status=%d want=%d body=%s", w.Code, tc.want, w.Body.String())
			}
			if strings.Contains(logs.String(), "DO_NOT_LOG_PAIRING") {
				t.Fatal("request logs exposed pairing input")
			}
		})
	}
}

func TestMobilePairingExchangeSharesLoginRateLimitAcrossSlashAliases(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/mobile/pairing/exchange", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	h := buildHTTPHandler(mux, &config.Config{BodyLimit: 1024}, &auth.Chain{}, nil, httpx.NewMetrics(), testLogger())
	for i := range 11 {
		path := "/api/mobile/pairing/exchange"
		if i%2 == 0 {
			path += "/"
		}
		w := httptest.NewRecorder()
		r := httptest.NewRequest("POST", path, nil)
		h.ServeHTTP(w, r)
		want := http.StatusNoContent
		if i == 10 {
			want = http.StatusTooManyRequests
		}
		if w.Code != want {
			t.Fatalf("request %d status=%d want=%d", i, w.Code, want)
		}
	}
}

func TestMobilePairingRegistersConnectedApps(t *testing.T) {
	d, err := db.Open(t.Context(), filepath.Join(t.TempDir(), "pairing.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	migs, err := db.LoadMigrations(migrations.FS, ".")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Migrate(t.Context(), d, migs, testLogger()); err != nil {
		t.Fatal(err)
	}
	if _, err := d.ExecWrite(t.Context(), `INSERT INTO users(id,email,display_name,role,created_at,updated_at) VALUES
		(1,'owner@example.test','Owner','member',0,0),(2,'other@example.test','Other','member',0,0)`); err != nil {
		t.Fatal(err)
	}
	local, err := localauth.New(t.Context(), d, testLogger(), false, false)
	if err != nil {
		t.Fatal(err)
	}
	s := &api.Server{DB: d, Log: testLogger(), PublicURL: "https://archive.example", TokenIssuer: local.IssueAPIToken}
	mux := http.NewServeMux()
	s.Register(mux)
	handler := buildHTTPHandler(mux, &config.Config{BodyLimit: 1024}, &auth.Chain{}, nil, httpx.NewMetrics(), testLogger())
	request := func(method, path, body string, userID int64) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		if userID != 0 {
			r = r.WithContext(auth.WithPrincipal(r.Context(), &pluginapi.Principal{Kind: "user", UserID: userID, Role: "member"}))
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}
	list := func(userID int64) []api.APITokenView {
		t.Helper()
		w := request("GET", "/api/tokens/", "", userID)
		if w.Code != 200 || w.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("list status=%d headers=%v", w.Code, w.Header())
		}
		var out struct {
			Results []api.APITokenView `json:"results"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(w.Body.String(), "token_hash") || strings.Contains(w.Body.String(), `"token":`) {
			t.Fatal("list contains a credential")
		}
		return out.Results
	}
	// A manually named mobile credential must remain an ordinary API token.
	w := request("POST", "/api/tokens/", `{"name":"My phone","scopes":"documents:read"}`, 1)
	if w.Code != 201 {
		t.Fatalf("manual token status=%d", w.Code)
	}
	manual := list(1)
	if len(manual) != 1 || manual[0].Source != "" {
		t.Fatal("manual token was registered as a paired app")
	}

	var credentials []string
	var connected []api.APITokenView
	for _, method := range []string{"QR", "link"} {
		t.Run(method, func(t *testing.T) {
			w := request("POST", "/api/mobile/pairing", `{"name":"My phone"}`, 1)
			if w.Code != 201 {
				t.Fatalf("pair status=%d", w.Code)
			}
			var pairing struct {
				URL string `json:"pairing_url"`
				QR  string `json:"qr_data_url"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &pairing); err != nil {
				t.Fatal(err)
			}
			if got := list(1); len(got) != len(credentials)+1 {
				t.Fatal("unused pairing created an entry")
			}
			link := pairing.URL
			if method == "QR" {
				data, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(pairing.QR, "data:image/png;base64,"))
				if err != nil {
					t.Fatal(err)
				}
				img, err := png.Decode(bytes.NewReader(data))
				if err != nil {
					t.Fatal(err)
				}
				bitmap, err := gozxing.NewBinaryBitmapFromImage(img)
				if err != nil {
					t.Fatal(err)
				}
				decoded, err := qrcode.NewQRCodeReader().Decode(bitmap, nil)
				if err != nil {
					t.Fatal(err)
				}
				link = decoded.GetText()
			}
			u, err := url.Parse(link)
			if err != nil {
				t.Fatal(err)
			}
			deviceName := map[string]string{"QR": "Ritesh’s iPhone", "link": "Family Pixel"}[method]
			body, err := json.Marshal(map[string]string{"code": u.Query().Get("code"), "device_name": deviceName})
			if err != nil {
				t.Fatal(err)
			}
			w = request("POST", "/api/mobile/pairing/exchange", string(body), 0)
			if w.Code != 200 {
				t.Fatalf("exchange status=%d", w.Code)
			}
			var issued map[string]string
			if err := json.Unmarshal(w.Body.Bytes(), &issued); err != nil {
				t.Fatal(err)
			}
			credentials = append(credentials, issued["token"])
			if replay := request("POST", "/api/mobile/pairing/exchange", string(body), 0); replay.Code != 400 {
				t.Fatalf("replay status=%d", replay.Code)
			}
			rows := list(1)
			if len(rows) != len(credentials)+1 {
				t.Fatalf("entries=%d", len(rows))
			}
			newest := rows[0]
			if newest.Source != auth.TokenSourceMobilePairing || newest.UserID != 1 || newest.Name != deviceName ||
				newest.Scopes != "documents:read,documents:write" || newest.CreatedAt == 0 || newest.LastUsedAt != 0 {
				t.Fatalf("connected app metadata=%+v", newest)
			}
			connected = append(connected, newest)
		})
	}
	if len(connected) != 2 {
		t.Fatal("both pairing methods must register")
	}
	if got := list(2); len(got) != 0 {
		t.Fatal("another member can see connected apps")
	}
	if w := request("DELETE", fmt.Sprintf("/api/tokens/%d", connected[0].ID), "", 2); w.Code != 404 {
		t.Fatalf("foreign revoke status=%d", w.Code)
	}
	authRequest := httptest.NewRequest("GET", "/api/whoami", nil)
	authRequest.Header.Set("Authorization", "Token "+credentials[0])
	principal, err := local.Authenticate(authRequest)
	if err != nil || principal == nil || principal.UserID != 1 {
		t.Fatalf("paired token auth: %v", err)
	}
	rows := list(1)
	if rows[1].ID != connected[0].ID || rows[1].LastUsedAt == 0 {
		t.Fatal("last use is not reflected in connected apps")
	}
	if w := request("DELETE", fmt.Sprintf("/api/tokens/%d", connected[0].ID), "", 1); w.Code != 204 {
		t.Fatalf("revoke status=%d", w.Code)
	}
	if _, err := local.Authenticate(authRequest); err == nil {
		t.Fatal("revoked app can authenticate")
	}
	if len(list(1)) != 2 {
		t.Fatal("revoked app is still listed")
	}
	authRequest.Header.Set("Authorization", "Token "+credentials[1])
	principal, err = local.Authenticate(authRequest)
	if err != nil || principal == nil {
		t.Fatalf("second app auth: %v", err)
	}
	logout := httptest.NewRecorder()
	local.LogoutHandler(logout, authRequest.WithContext(auth.WithPrincipal(authRequest.Context(), principal)))
	if logout.Code != 204 {
		t.Fatalf("logout status=%d", logout.Code)
	}
	if rows := list(1); len(rows) != 1 || rows[0].ID != manual[0].ID {
		t.Fatal("signed-out app is still listed")
	}
}
