// SPDX-License-Identifier: AGPL-3.0-or-later

package api

// PATCH /api/users/me covers display-name and locale preferences. Tests pin:
//   - anonymous → 401
//   - happy-path updates emit audit events
//   - locale values must resolve to an embedded catalog
//   - unknown fields, including email, are refused
//   - empty body refused
//   - overly long name refused

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	pluginapi "github.com/johnnybravo-xyz/suchi/plugin-api"

	"github.com/johnnybravo-xyz/suchi/core/auth"
	"github.com/johnnybravo-xyz/suchi/core/i18n"
)

func doPatchSelf(t *testing.T, s *Server, body string, p *pluginapi.Principal) (int, UserSelf) {
	t.Helper()
	rec := httptest.NewRecorder()
	ctx := context.Background()
	if p != nil {
		ctx = auth.WithPrincipal(ctx, p)
	}
	r := httptest.NewRequest("PATCH", "/api/users/me", strings.NewReader(body)).WithContext(ctx)
	r.Header.Set("Content-Type", "application/json")
	s.PatchSelf(rec, r)
	var out UserSelf
	if rec.Code == 200 {
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
	}
	return rec.Code, out
}

func TestPatchSelf_Anonymous(t *testing.T) {
	d := openTestDB(t)
	s := &Server{DB: d, Log: slog.New(slog.NewTextHandler(os.Stderr, nil))}
	code, _ := doPatchSelf(t, s, `{"display_name":"x"}`, nil)
	if code != 401 {
		t.Fatalf("anonymous status=%d, want 401", code)
	}
}

func TestPatchSelf_UpdatesDisplayName(t *testing.T) {
	d := openTestDB(t)
	seedUser(t, d, 1)
	s := &Server{DB: d, Log: slog.New(slog.NewTextHandler(os.Stderr, nil))}
	code, self := doPatchSelf(t, s, `{"display_name":"  Ritesh S.  "}`, memberPrincipal(1))
	if code != 200 {
		t.Fatalf("status=%d", code)
	}
	if self.DisplayName != "Ritesh S." {
		t.Errorf("display_name = %q, want %q (whitespace should trim)", self.DisplayName, "Ritesh S.")
	}
	// Audit row must land.
	var count int
	if err := d.Read.QueryRow(
		`SELECT COUNT(*) FROM audit_events WHERE action = 'user.display_name_changed'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Errorf("audit rows = %d, want 1", count)
	}
}

func TestPatchSelf_PersistsSupportedLocale(t *testing.T) {
	d := openTestDB(t)
	seedUser(t, d, 1)
	catalogs, err := i18n.NewRegistry(nil)
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{DB: d, Log: slog.New(slog.NewTextHandler(os.Stderr, nil)), Catalogs: catalogs}
	code, self := doPatchSelf(t, s, `{"locale":"en-US"}`, memberPrincipal(1))
	if code != 200 || self.Locale != "en" {
		t.Fatalf("locale patch status=%d self=%+v", code, self)
	}
	var locale string
	if err := d.Read.QueryRow(`SELECT locale FROM users WHERE id=1`).Scan(&locale); err != nil {
		t.Fatal(err)
	}
	if locale != "en" {
		t.Fatalf("stored locale = %q, want en", locale)
	}
	if code, _ := doPatchSelf(t, s, `{"locale":"de"}`, memberPrincipal(1)); code != 400 {
		t.Fatalf("unsupported locale status=%d, want 400", code)
	}
	if code, self := doPatchSelf(t, s, `{"locale":""}`, memberPrincipal(1)); code != 200 || self.Locale != "" {
		t.Fatalf("automatic locale patch status=%d self=%+v", code, self)
	}
	var auditCount int
	if err := d.Read.QueryRow(`SELECT COUNT(*) FROM audit_events WHERE action='user.locale_changed'`).Scan(&auditCount); err != nil {
		t.Fatal(err)
	}
	if auditCount != 2 {
		t.Fatalf("locale audit rows=%d, want 2", auditCount)
	}
}

func TestPatchSelf_EmailIsUnknownField(t *testing.T) {
	d := openTestDB(t)
	seedUser(t, d, 1)
	s := &Server{DB: d, Log: slog.New(slog.NewTextHandler(os.Stderr, nil))}
	rec := httptest.NewRecorder()
	ctx := auth.WithPrincipal(context.Background(), memberPrincipal(1))
	r := httptest.NewRequest("PATCH", "/api/users/me",
		strings.NewReader(`{"email":"new@example.com"}`)).WithContext(ctx)
	s.PatchSelf(rec, r)
	if rec.Code != 400 {
		t.Fatalf("email field status=%d, want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"code":"bad_json"`) ||
		!strings.Contains(rec.Body.String(), "unknown field") {
		t.Errorf("expected strict unknown-field error, got body=%s", rec.Body.String())
	}
}

func TestPatchSelf_EmptyAndTooLong(t *testing.T) {
	d := openTestDB(t)
	seedUser(t, d, 1)
	s := &Server{DB: d, Log: slog.New(slog.NewTextHandler(os.Stderr, nil))}

	code, _ := doPatchSelf(t, s, `{}`, memberPrincipal(1))
	if code != 400 {
		t.Errorf("empty body status=%d, want 400", code)
	}

	code, _ = doPatchSelf(t, s, `{"display_name":"   "}`, memberPrincipal(1))
	if code != 400 {
		t.Errorf("whitespace-only name status=%d, want 400", code)
	}

	long := `{"display_name":"` + strings.Repeat("x", 121) + `"}`
	code, _ = doPatchSelf(t, s, long, memberPrincipal(1))
	if code != 400 {
		t.Errorf("121-char name status=%d, want 400", code)
	}
}

func doWhoami(t *testing.T, s *Server, p *pluginapi.Principal) (int, UserSelf) {
	t.Helper()
	rec := httptest.NewRecorder()
	ctx := context.Background()
	if p != nil {
		ctx = auth.WithPrincipal(ctx, p)
	}
	r := httptest.NewRequest("GET", "/api/whoami", nil).WithContext(ctx)
	s.Whoami(rec, r)
	var out UserSelf
	if rec.Code == 200 {
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
	}
	return rec.Code, out
}

func TestWhoami_ReturnsOnlyTokenScopes(t *testing.T) {
	d := openTestDB(t)
	seedUser(t, d, 1)
	s := &Server{DB: d, Log: slog.New(slog.NewTextHandler(os.Stderr, nil))}

	token := memberPrincipal(1)
	token.Kind = "token"
	token.Scopes = []string{"documents:read", "documents:write"}
	code, self := doWhoami(t, s, token)
	if code != 200 {
		t.Fatalf("token status=%d, want 200", code)
	}
	if len(self.Scopes) != 2 || self.Scopes[0] != "documents:read" || self.Scopes[1] != "documents:write" {
		t.Fatalf("token scopes=%v, want principal scopes", self.Scopes)
	}
	if self.SystemID == nil || *self.SystemID != 1 || self.SystemName == nil || *self.SystemName != "Archive" || self.SystemCode == nil || *self.SystemCode != "" {
		t.Fatalf("token system = (%v, %v, %v), want (1, Archive, empty code)", self.SystemID, self.SystemName, self.SystemCode)
	}

	session := memberPrincipal(1)
	session.Scopes = []string{"must:not:leak"}
	code, self = doWhoami(t, s, session)
	if code != 200 {
		t.Fatalf("session status=%d, want 200", code)
	}
	if self.Scopes == nil || len(self.Scopes) != 0 {
		t.Fatalf("session scopes=%v, want []", self.Scopes)
	}
	if self.SystemID != nil || self.SystemName != nil || self.SystemCode != nil {
		t.Fatalf("session unexpectedly exposed a token system: (%v, %v, %v)", self.SystemID, self.SystemName, self.SystemCode)
	}
}

func TestWhoamiReturnsEffectiveDeploymentFeatures(t *testing.T) {
	d := openTestDB(t)
	seedUser(t, d, 1)
	want := DeploymentFeatures{
		LLMConnections: false,
		EmailIngestion: false,
		ShareLinks:     false,
		MobilePairing:  false,
	}
	s := &Server{
		DB:       d,
		Log:      slog.New(slog.NewTextHandler(os.Stderr, nil)),
		Features: want,
	}

	code, self := doWhoami(t, s, memberPrincipal(1))
	if code != http.StatusOK {
		t.Fatalf("status=%d, want 200", code)
	}
	if self.Features != want {
		t.Fatalf("features=%+v, want %+v", self.Features, want)
	}

	self.DisplayName = "updated"
	body, err := json.Marshal(map[string]string{"display_name": self.DisplayName})
	if err != nil {
		t.Fatal(err)
	}
	code, patched := doPatchSelf(t, s, string(body), memberPrincipal(1))
	if code != http.StatusOK || patched.Features != want {
		t.Fatalf("profile refresh status=%d features=%+v, want %+v", code, patched.Features, want)
	}
}

func TestWhoamiRereadsCanonicalIdentityBeforeModeEvaluation(t *testing.T) {
	d := openTestDB(t)
	seedUser(t, d, 1)
	if _, err := d.Write.Exec(`
		UPDATE users SET email='canonical@example.test', role='member', display_name='Canonical'
		WHERE id=1
	`); err != nil {
		t.Fatal(err)
	}
	var evaluated pluginapi.Principal
	s := &Server{
		DB: d, Log: slog.New(slog.NewTextHandler(os.Stderr, nil)),
		EmailChangeModeFor: func(p *pluginapi.Principal) string {
			evaluated = *p
			return EmailChangeModePassword
		},
	}
	stale := memberPrincipal(1)
	stale.Email = "stale@example.test"
	stale.Role = "admin"
	code, self := doWhoami(t, s, stale)
	if code != 200 {
		t.Fatalf("status=%d", code)
	}
	if self.Email != "canonical@example.test" || self.Role != "member" ||
		self.DisplayName != "Canonical" || self.EmailChangeMode != EmailChangeModePassword {
		t.Fatalf("canonical self=%+v", self)
	}
	if evaluated.Email != self.Email || evaluated.Role != self.Role {
		t.Fatalf("mode evaluator received stale identity: %+v", evaluated)
	}
}

func TestWhoamiAllowsOnlyStatelessDemoFallback(t *testing.T) {
	d := openTestDB(t)
	s := &Server{
		DB: d, Log: slog.New(slog.NewTextHandler(os.Stderr, nil)),
		EmailChangeModeFor: func(*pluginapi.Principal) string { return EmailChangeModeDisabled },
	}
	demo := &pluginapi.Principal{
		Kind: PrincipalKindDemoAnon, Email: "visitor@demo.local",
		Display: "Visitor", Role: "member", AuthNBy: "demo-anon",
	}
	code, self := doWhoami(t, s, demo)
	if code != 200 || self.Email != demo.Email || self.DisplayName != demo.Display ||
		self.Role != demo.Role || self.EmailChangeMode != EmailChangeModeDisabled {
		t.Fatalf("demo fallback: status=%d self=%+v", code, self)
	}

	missing := memberPrincipal(999)
	code, _ = doWhoami(t, s, missing)
	if code != 500 {
		t.Fatalf("row-backed missing user status=%d, want 500", code)
	}
}
