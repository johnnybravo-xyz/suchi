package api

// Setup wizard validation tests. Focused on boundary checks that keep
// bad inputs out of SQL / shell / settings — not the happy-path
// integration flow (that's exercised by hack/local-ingest-test.sh +
// deploy/mail-mbsync/smoke-test.sh).

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/johnnybravo-xyz/suchi/core/auth"
	suchicrypto "github.com/johnnybravo-xyz/suchi/core/crypto"
	"github.com/johnnybravo-xyz/suchi/core/netutil"
	"github.com/johnnybravo-xyz/suchi/core/settings"
	pluginapi "github.com/johnnybravo-xyz/suchi/plugin-api"
)

func TestIsNonLocal(t *testing.T) {
	cases := []struct {
		host string
		want bool
	}{
		{"", false},
		{"localhost", false},
		{"127.0.0.1", false},
		{"::1", false},
		{"192.168.1.10", false},
		{"10.0.0.5", false},
		{"172.16.99.1", false},
		{"172.31.255.255", false},
		{"172.32.0.1", true}, // out of RFC-1918 range
		{"172.15.0.1", true},
		{"host.local", false},
		{"lab.internal", true},
		{"my.lan", true},
		{"api.openai.com", true},
		{"claude.anthropic.com", true},
		{"[::1]", false},
		{"localhost:11434", false},
		{"api.openai.com:443", true},
	}
	for _, tc := range cases {
		if got := !netutil.IsLocalHost(tc.host); got != tc.want {
			t.Errorf("non-local(%q) = %v, want %v", tc.host, got, tc.want)
		}
	}
}

func TestEmailPattern(t *testing.T) {
	good := []string{
		"a@b.co", "user@example.com", "u+tag@e.io",
		"first.last@sub.domain.tld", "u-y@z.co",
	}
	bad := []string{
		"", "no-at-sign", "@nope.com", "user@", "user@host",
		"has space@e.com", "\"quoted\"@e.com", "back\\slash@e.com",
		"<inline>@e.com", "user;drop@e.com",
	}
	for _, e := range good {
		if !emailPattern.MatchString(e) {
			t.Errorf("expected valid: %q", e)
		}
	}
	for _, e := range bad {
		if emailPattern.MatchString(e) {
			t.Errorf("expected invalid: %q", e)
		}
	}
}

func TestModelSafe(t *testing.T) {
	good := []string{
		"gpt-4o-mini", "llama3.1:8b", "claude-3-5-sonnet-20241022",
		"microsoft/DialoGPT", "mistral/mistral-large",
	}
	badKnown := []string{
		"", "has space", "has;semi", "has$dollar",
		"has|pipe", "has`tick", "has'quote",
	}
	for _, m := range good {
		if !modelSafe.MatchString(m) {
			t.Errorf("expected valid: %q", m)
		}
	}
	for _, m := range badKnown {
		if modelSafe.MatchString(m) {
			t.Errorf("expected invalid: %q", m)
		}
	}
}

func TestSaveLLMSettings_SealsKeyAndReportsRestart(t *testing.T) {
	d := openTestDB(t)
	key, err := suchicrypto.LoadOrCreateKey(filepath.Join(t.TempDir(), "secret.key"))
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{
		DB:      d,
		Log:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		LLMAEAD: key,
	}
	body := `{"endpoint_url":"http://127.0.0.1:11434/v1","model":"qwen2.5:7b","api_key":"top-secret"}`
	req := httptest.NewRequest(http.MethodPost, "/api/admin/settings/llm", strings.NewReader(body))
	req = req.WithContext(auth.WithPrincipal(req.Context(), &pluginapi.Principal{
		Kind: "user", UserID: 1, Role: "admin",
	}))
	rec := httptest.NewRecorder()

	s.SaveLLMSettings(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var response struct {
		RestartRequired bool `json:"restart_required"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if !response.RestartRequired {
		t.Fatal("first-time activation must report restart_required")
	}

	var stored map[string]any
	if err := settings.Get(req.Context(), d, settings.KeyLLMAPIKeySealed, &stored); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(rec.Body.String(), "top-secret") {
		t.Fatal("response exposed API key")
	}
	resolved, err := settings.ResolveLLMConfig(req.Context(), d, settings.LLMConfig{}, key)
	if err != nil {
		t.Fatal(err)
	}
	if resolved.APIKey != "top-secret" || resolved.Model != "qwen2.5:7b" {
		t.Fatalf("resolved config = %#v", resolved)
	}
	if _, ok := stored["ciphertext"]; !ok {
		t.Fatalf("stored key is not a sealed envelope: %#v", stored)
	}
}

func TestLangCode(t *testing.T) {
	good := []string{"eng", "hin", "en_US", "pt_BR", "de"}
	bad := []string{"", "e", "english", "en-US", "En", "eng123", "eng_us"}
	for _, l := range good {
		if !langCode.MatchString(l) {
			t.Errorf("expected valid: %q", l)
		}
	}
	for _, l := range bad {
		if langCode.MatchString(l) {
			t.Errorf("expected invalid: %q", l)
		}
	}
}

func TestFSPath(t *testing.T) {
	good := []string{"/data/staging", "/mnt/homelab/ingest", "/tmp/x", "/"}
	bad := []string{
		"", "relative/path", "/back\\slash", "/semi;colon",
		"/pipe|", "/tick`", "/quote'", "/dollar$var",
	}
	for _, p := range good {
		if !fsPath.MatchString(p) {
			t.Errorf("expected valid: %q", p)
		}
	}
	for _, p := range bad {
		if fsPath.MatchString(p) {
			t.Errorf("expected invalid: %q", p)
		}
	}
}

func TestStepNames_IsExactAllowlist(t *testing.T) {
	// Any change to the allowlist should be intentional — this test
	// forces a review by listing the exact set.
	want := map[string]bool{
		"welcome":     true,
		"users":       true,
		"mail":        true,
		"llm":         true,
		"jd":          true,
		"rules":       true,
		"sources":     true,
		"preferences": true,
		"done":        true,
	}
	if len(StepNames) != len(want) {
		t.Fatalf("StepNames size drifted: got %d, want %d", len(StepNames), len(want))
	}
	for k := range want {
		if !StepNames[k] {
			t.Errorf("StepNames missing %q", k)
		}
	}
	for k := range StepNames {
		if !want[k] {
			t.Errorf("StepNames has unexpected %q", k)
		}
	}
}

func TestIsUniqueViolation(t *testing.T) {
	cases := []struct {
		msg  string
		want bool
	}{
		{"UNIQUE constraint failed: users.email", true},
		{"constraint failed: (2067)", true},
		{"NOT NULL constraint failed: docs.title", false},
		{"connection refused", false},
		{"", false},
	}
	for _, tc := range cases {
		got := isUniqueViolation(&stringErr{s: tc.msg})
		if got != tc.want {
			t.Errorf("isUniqueViolation(%q) = %v, want %v", tc.msg, got, tc.want)
		}
	}
	if isUniqueViolation(nil) {
		t.Error("isUniqueViolation(nil) should be false")
	}
}

type stringErr struct{ s string }

func (e *stringErr) Error() string { return e.s }
