// SPDX-License-Identifier: AGPL-3.0-or-later

package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"image/png"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/johnnybravo-xyz/suchi/core/auth"
	pluginapi "github.com/johnnybravo-xyz/suchi/plugin-api"
	"github.com/makiuchi-d/gozxing"
	"github.com/makiuchi-d/gozxing/qrcode"
)

func newMobilePairingServer(t *testing.T) (*Server, *bytes.Buffer) {
	t.Helper()
	d := openTestDB(t)
	seedUser(t, d, 1)
	seedUser(t, d, 2)
	var logs bytes.Buffer
	s := &Server{DB: d, Log: slog.New(slog.NewTextHandler(&logs, nil)), PublicURL: "https://Archive.Example:443/"}
	var nextToken atomic.Int64
	s.TokenIssuer = func(ctx context.Context, tx *sql.Tx, userID, systemID int64, name, scopes, source string) (string, error) {
		token := fmt.Sprintf("%064x", nextToken.Add(1))
		sum := sha256.Sum256([]byte(token))
		_, err := tx.ExecContext(ctx, `INSERT INTO api_tokens(system_id, user_id, name, token_hash, scopes, created_at, source) VALUES (?, ?, ?, ?, ?, ?, ?)`,
			systemID, userID, name, hex.EncodeToString(sum[:]), scopes, time.Now().Unix(), source)
		return token, err
	}
	return s, &logs
}

func mobilePairingRequest(s *Server, method, path, body string, p *pluginapi.Principal) *httptest.ResponseRecorder {
	mux := http.NewServeMux()
	s.Register(mux)
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Host = "untrusted-proxy.example"
	r.Header.Set("X-Forwarded-Host", "untrusted-proxy.example")
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	r = r.WithContext(auth.WithPrincipal(ctx, p))
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	return w
}

func createMobilePairing(t *testing.T, s *Server) mobilePairingResponse {
	t.Helper()
	w := mobilePairingRequest(s, "POST", "/api/mobile/pairing", `{}`, memberPrincipal(1))
	if w.Code != 201 {
		t.Fatalf("create status=%d body=%s", w.Code, w.Body.String())
	}
	var out mobilePairingResponse
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestMobilePairingCreatesLocalQRAndStoresOnlyDigest(t *testing.T) {
	s, logs := newMobilePairingServer(t)
	before := time.Now().Unix()
	w := mobilePairingRequest(s, "POST", "/api/mobile/pairing", "", memberPrincipal(1))
	if w.Code != 201 || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("create status=%d headers=%v", w.Code, w.Header())
	}
	var out mobilePairingResponse
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if !validMobilePairingCode(out.Code) || out.Name != "Suchi mobile" || out.ExpiresAt < before+300 || out.ExpiresAt > time.Now().Unix()+300 {
		t.Fatal("unexpected pairing code, name, or expiry")
	}
	link, err := url.Parse(out.PairingURL)
	if err != nil {
		t.Fatal(err)
	}
	if link.Scheme != "suchi" || link.Host != "pair" || link.Query().Get("v") != "1" ||
		link.Query().Get("server") != "https://archive.example" || link.Query().Get("code") != out.Code || len(link.Query()) != 3 {
		t.Fatal("pairing link must bind code to configured canonical origin")
	}
	qr, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(out.QRDataURL, "data:image/png;base64,"))
	if err != nil {
		t.Fatal(err)
	}
	img, err := png.Decode(bytes.NewReader(qr))
	if err != nil {
		t.Fatal(err)
	}
	if img.Bounds().Dx() != 320 || img.Bounds().Dy() != 320 {
		t.Fatalf("QR bounds=%v", img.Bounds())
	}
	bitmap, err := gozxing.NewBinaryBitmapFromImage(img)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := qrcode.NewQRCodeReader().Decode(bitmap, nil)
	if err != nil || decoded.GetText() != out.PairingURL {
		t.Fatalf("QR roundtrip failed: %v", err)
	}
	var hash, name string
	var expires int64
	if err := s.DB.Read.QueryRow(`SELECT code_hash, name, expires_at FROM mobile_pairings WHERE user_id=1`).Scan(&hash, &name, &expires); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(out.Code))
	if hash != hex.EncodeToString(sum[:]) || hash == out.Code || name != out.Name || expires != out.ExpiresAt {
		t.Fatal("stored pairing does not match one-way digest and metadata")
	}
	for _, secret := range []string{out.Code, out.PairingURL, out.QRDataURL} {
		if strings.Contains(logs.String(), secret) {
			t.Fatal("pairing secret was logged")
		}
	}
}

func TestMobilePairingReplacementAndCancellationStayScoped(t *testing.T) {
	s, _ := newMobilePairingServer(t)
	old := createMobilePairing(t, s)
	current := createMobilePairing(t, s)
	for _, code := range []string{old.Code, old.Code} {
		w := mobilePairingRequest(s, "DELETE", "/api/mobile/pairing", `{"code":"`+code+`"}`, memberPrincipal(1))
		if w.Code != 204 || w.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("cancel status=%d", w.Code)
		}
	}
	w := mobilePairingRequest(s, "DELETE", "/api/mobile/pairing", `{"code":"`+current.Code+`"}`, memberPrincipal(2))
	if w.Code != 204 {
		t.Fatalf("other user cancellation status=%d", w.Code)
	}
	assertMobilePairingInvalid(t, s, old.Code)
	w = mobilePairingRequest(s, "POST", "/api/mobile/pairing/exchange", `{"code":"`+current.Code+`"}`, nil)
	if w.Code != 200 || w.Header().Get("Cache-Control") != "no-store" || w.Header().Get("Set-Cookie") != "" {
		t.Fatalf("exchange status=%d headers=%v", w.Code, w.Header())
	}
	var out map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out["token"] != fmt.Sprintf("%064x", 1) || out["name"] != "Suchi mobile" || out["scopes"] != mobilePairingScopes || len(out) != 3 {
		t.Fatal("exchange did not preserve fixed token contract")
	}
	assertMobilePairingInvalid(t, s, current.Code)
	other := createMobilePairing(t, s)
	w = mobilePairingRequest(s, "DELETE", "/api/mobile/pairing", `{"code":"`+other.Code+`"}`, memberPrincipal(1))
	if w.Code != 204 {
		t.Fatalf("cancel status=%d", w.Code)
	}
	assertMobilePairingInvalid(t, s, other.Code)
}

func assertMobilePairingInvalid(t *testing.T, s *Server, code string) {
	t.Helper()
	body, err := json.Marshal(map[string]string{"code": code})
	if err != nil {
		t.Fatal(err)
	}
	w := mobilePairingRequest(s, "POST", "/api/mobile/pairing/exchange", string(body), nil)
	if w.Code != 400 || w.Header().Get("Cache-Control") != "no-store" ||
		w.Body.String() != "{\"error\":\"pairing code is invalid or expired\",\"code\":\"pairing_invalid\"}\n" {
		t.Fatalf("invalid response status=%d body=%s", w.Code, w.Body.String())
	}
}

func TestMobilePairingExpiryDisabledUserAndInvalidCodeAreIndistinguishable(t *testing.T) {
	for _, lifecycle := range []string{"expired", "disabled", "deleted", "demo"} {
		t.Run(lifecycle, func(t *testing.T) {
			s, _ := newMobilePairingServer(t)
			pairing := createMobilePairing(t, s)
			query := map[string]string{
				"expired": "UPDATE mobile_pairings SET expires_at=0", "disabled": "UPDATE users SET disabled=1 WHERE id=1",
				"deleted": "DELETE FROM users WHERE id=1",
			}[lifecycle]
			if query != "" {
				if _, err := s.DB.ExecWrite(t.Context(), query); err != nil {
					t.Fatal(err)
				}
			} else {
				s.SetDemo(DemoConfig{Enabled: true})
			}
			assertMobilePairingInvalid(t, s, pairing.Code)
		})
	}
	s, _ := newMobilePairingServer(t)
	for _, code := range []string{"", "a", strings.Repeat("a", 63), strings.Repeat("a", 65), strings.Repeat("A", 64), strings.Repeat("g", 64), strings.Repeat("0", 64)} {
		assertMobilePairingInvalid(t, s, code)
	}
}

func TestMobilePairingConcurrentExchangeIssuesExactlyOneToken(t *testing.T) {
	s, _ := newMobilePairingServer(t)
	pairing := createMobilePairing(t, s)
	var issued atomic.Int32
	issuer := s.TokenIssuer
	s.TokenIssuer = func(ctx context.Context, tx *sql.Tx, userID, systemID int64, name, scopes, source string) (string, error) {
		issued.Add(1)
		return issuer(ctx, tx, userID, systemID, name, scopes, source)
	}
	var wg sync.WaitGroup
	statuses := make(chan int, 12)
	for range 12 {
		wg.Go(func() {
			w := mobilePairingRequest(s, "POST", "/api/mobile/pairing/exchange", `{"code":"`+pairing.Code+`"}`, nil)
			statuses <- w.Code
		})
	}
	wg.Wait()
	close(statuses)
	success := 0
	for status := range statuses {
		if status == 200 {
			success++
		} else if status != 400 {
			t.Errorf("unexpected exchange status=%d", status)
		}
	}
	if success != 1 || issued.Load() != 1 {
		t.Fatalf("successful exchanges=%d issued=%d", success, issued.Load())
	}
}

func TestMobilePairingIssuanceFailureRollsBackAndDoesNotLogSecrets(t *testing.T) {
	s, logs := newMobilePairingServer(t)
	pairing := createMobilePairing(t, s)
	issuer := s.TokenIssuer
	s.TokenIssuer = func(context.Context, *sql.Tx, int64, int64, string, string, string) (string, error) {
		return "", errors.New("credential failure " + pairing.Code)
	}
	w := mobilePairingRequest(s, "POST", "/api/mobile/pairing/exchange", `{"code":"`+pairing.Code+`"}`, nil)
	if w.Code != 500 || strings.Contains(logs.String(), pairing.Code) || strings.Contains(w.Body.String(), pairing.Code) {
		t.Fatal("issuance failure must stay secret and return 500")
	}
	var count int
	if err := s.DB.Read.QueryRow("SELECT COUNT(*) FROM api_tokens").Scan(&count); err != nil || count != 0 {
		t.Fatalf("failed exchange registered a token: count=%d err=%v", count, err)
	}
	s.TokenIssuer = issuer
	w = mobilePairingRequest(s, "POST", "/api/mobile/pairing/exchange", `{"code":"`+pairing.Code+`"}`, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("failed issuance consumed code: status=%d body=%s", w.Code, w.Body.String())
	}
	assertMobilePairingInvalid(t, s, pairing.Code)
}

func TestMobilePairingSessionAndInputValidation(t *testing.T) {
	s, _ := newMobilePairingServer(t)
	for _, method := range []string{"POST", "DELETE"} {
		for _, kind := range []string{"", "token", "demo-anon", "demo-scratch", "future-kind"} {
			p := memberPrincipal(1)
			p.Kind = kind
			want := 403
			if kind == "" {
				p, want = nil, 401
			}
			w := mobilePairingRequest(s, method, "/api/mobile/pairing", `{}`, p)
			if w.Code != want {
				t.Errorf("%s kind=%q status=%d want=%d", method, kind, w.Code, want)
			}
		}
	}
	for _, body := range []string{`{"name":"` + strings.Repeat("x", 65) + `"}`, `{"scopes":"events:read"}`, `{} {}`, `[]`, `{`} {
		w := mobilePairingRequest(s, "POST", "/api/mobile/pairing", body, memberPrincipal(1))
		if w.Code != 400 {
			t.Errorf("invalid create body status=%d", w.Code)
		}
	}
	w := mobilePairingRequest(s, "POST", "/api/mobile/pairing", `{"name":"  `+strings.Repeat("日", 64)+`  "}`, memberPrincipal(1))
	if w.Code != 201 {
		t.Fatalf("64-character Unicode name status=%d", w.Code)
	}
	if _, err := s.DB.ExecWrite(t.Context(), `UPDATE users SET disabled=1 WHERE id=1`); err != nil {
		t.Fatal(err)
	}
	w = mobilePairingRequest(s, "POST", "/api/mobile/pairing", `{}`, memberPrincipal(1))
	if w.Code != http.StatusNotFound {
		t.Fatalf("disabled principal retained system entry: status=%d", w.Code)
	}
}

func TestMobilePairingSchemaCascadesAndBoundsOneCodePerUser(t *testing.T) {
	s, _ := newMobilePairingServer(t)
	createMobilePairing(t, s)
	createMobilePairing(t, s)
	var n int
	if err := s.DB.Read.QueryRow(`SELECT COUNT(*) FROM mobile_pairings WHERE user_id=1`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("pending pairings=%d err=%v", n, err)
	}
	if _, err := s.DB.ExecWrite(t.Context(), `DELETE FROM users WHERE id=1`); err != nil {
		t.Fatal(err)
	}
	var id int64
	if err := s.DB.Read.QueryRow(`SELECT user_id FROM mobile_pairings`).Scan(&id); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("pairing survived owner deletion: %v", err)
	}
}

func TestMobilePairingOrigin(t *testing.T) {
	for raw, want := range map[string]string{
		"https://Archive.Example:443/": "https://archive.example", "http://localhost:80": "http://localhost",
		"http://[::1]:8000/": "http://[::1]:8000", "https://[2001:db8::1]:443": "https://[2001:db8::1]",
		"http://LOCALHOST:8000": "http://localhost:8000", "http://127.0.0.1:8000": "http://127.0.0.1:8000",
		"http://127.255.255.254": "http://127.255.255.254", "http://10.1.2.3": "http://10.1.2.3",
		"http://172.16.0.1": "http://172.16.0.1", "http://172.31.255.1": "http://172.31.255.1",
		"http://192.168.4.2": "http://192.168.4.2", "http://[fc00::1]": "http://[fc00::1]",
		"http://[fd00::12]": "http://[fd00::12]", "https://printer.local": "https://printer.local",
		"https://archive.example:00443": "https://archive.example", "https://archive.example:08443": "https://archive.example:8443",
	} {
		got, err := mobilePairingOrigin(raw)
		if err != nil || got != want {
			t.Errorf("origin=%q want=%q err=%v", got, want, err)
		}
	}
	for _, raw := range []string{
		"", "/", "https://user:secret@example.com", "https://example.com/path", "https://example.com/?secret=x", "https://example.com/#", "ftp://example.com",
		"http://suchi.example.com", "http://printer.local", "http://localhost.example.com", "http://app.localhost", "http://localhost.",
		"http://8.8.8.8", "http://169.254.1.1", "http://[fe80::1]", "http://172.15.255.255", "http://172.32.0.1", "http://192.169.0.1",
		"http://0.0.0.0", "http://[::]", "http://[::ffff:127.0.0.1]", "http://[fd00::1%25en0]",
		"https://example.com:0", "https://example.com:00", "https://example.com:65536",
	} {
		if _, err := mobilePairingOrigin(raw); err == nil {
			t.Errorf("accepted non-origin %q", raw)
		}
	}
}

func TestMobilePairingRejectsUnusableOriginWithoutReplacingPendingCode(t *testing.T) {
	s, _ := newMobilePairingServer(t)
	current := createMobilePairing(t, s)
	wantHash := sha256.Sum256([]byte(current.Code))
	for _, origin := range []string{"http://archive.example:8000", "http://archive.local:8000", "http://169.254.1.1", "https://archive.example:0"} {
		s.PublicURL = origin
		w := mobilePairingRequest(s, "POST", "/api/mobile/pairing", `{}`, memberPrincipal(1))
		if w.Code != http.StatusServiceUnavailable || w.Header().Get("Cache-Control") != "no-store" ||
			!strings.Contains(w.Body.String(), `"code":"pairing_unavailable"`) ||
			!strings.Contains(w.Body.String(), "HTTPS origin") || !strings.Contains(w.Body.String(), "private literal IP") {
			t.Fatalf("origin=%s status=%d body=%s", origin, w.Code, w.Body.String())
		}
		var storedHash string
		if err := s.DB.Read.QueryRow(`SELECT code_hash FROM mobile_pairings WHERE user_id=1`).Scan(&storedHash); err != nil ||
			storedHash != hex.EncodeToString(wantHash[:]) {
			t.Fatalf("invalid origin replaced the pending code: %v", err)
		}
	}
}

func TestMobilePairingUsesConnectingDeviceName(t *testing.T) {
	for _, tc := range []struct{ label, deviceName, want string }{
		{"phone", "Ritesh’s iPhone", "Ritesh’s iPhone"},
		{"trimmed Unicode", "  家族の Pixel  ", "家族の Pixel"},
		{"maximum Unicode length", strings.Repeat("日", 64), strings.Repeat("日", 64)},
		{"empty fallback", "", "Browser label"},
		{"blank fallback", "   ", "Browser label"},
	} {
		t.Run(tc.label, func(t *testing.T) {
			s, logs := newMobilePairingServer(t)
			w := mobilePairingRequest(s, "POST", "/api/mobile/pairing", `{"name":"Browser label"}`, memberPrincipal(1))
			var pairing mobilePairingResponse
			if w.Code != 201 {
				t.Fatalf("create status=%d", w.Code)
			}
			if err := json.Unmarshal(w.Body.Bytes(), &pairing); err != nil {
				t.Fatal(err)
			}
			body, err := json.Marshal(map[string]string{"code": pairing.Code, "device_name": tc.deviceName})
			if err != nil {
				t.Fatal(err)
			}
			w = mobilePairingRequest(s, "POST", "/api/mobile/pairing/exchange", string(body), nil)
			if w.Code != 200 {
				t.Fatalf("exchange status=%d database=%s logs=%s", w.Code, s.DB.Path, logs.String())
			}
			var response map[string]string
			if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
				t.Fatal(err)
			}
			if response["name"] != tc.want {
				t.Fatalf("returned name=%q want=%q", response["name"], tc.want)
			}
			w = mobilePairingRequest(s, "GET", "/api/tokens/", "", memberPrincipal(1))
			var listed struct {
				Results []APITokenView `json:"results"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &listed); err != nil {
				t.Fatal(err)
			}
			if w.Code != 200 || len(listed.Results) != 1 || listed.Results[0].Name != tc.want ||
				listed.Results[0].Source != auth.TokenSourceMobilePairing || listed.Results[0].UserID != 1 {
				t.Fatalf("connected app=%+v status=%d", listed, w.Code)
			}
		})
	}
}

func TestMobilePairingInvalidDeviceNameDoesNotConsumeCode(t *testing.T) {
	for _, name := range []string{strings.Repeat("x", 65), strings.Repeat("日", 65), "Phone\x00name", "Phone\nname", "Phone\u0085name"} {
		s, _ := newMobilePairingServer(t)
		pairing := createMobilePairing(t, s)
		body, err := json.Marshal(map[string]string{"code": pairing.Code, "device_name": name})
		if err != nil {
			t.Fatal(err)
		}
		w := mobilePairingRequest(s, "POST", "/api/mobile/pairing/exchange", string(body), nil)
		if w.Code != 400 || !strings.Contains(w.Body.String(), "bad_name") {
			t.Fatalf("invalid name status=%d", w.Code)
		}
		var count int
		if err := s.DB.Read.QueryRow("SELECT COUNT(*) FROM api_tokens").Scan(&count); err != nil || count != 0 {
			t.Fatalf("invalid name registered a token: count=%d err=%v", count, err)
		}
		body, err = json.Marshal(map[string]string{"code": pairing.Code, "device_name": "Corrected iPhone"})
		if err != nil {
			t.Fatal(err)
		}
		w = mobilePairingRequest(s, "POST", "/api/mobile/pairing/exchange", string(body), nil)
		if w.Code != 200 {
			t.Fatalf("corrected name could not use code: status=%d", w.Code)
		}
	}
}
