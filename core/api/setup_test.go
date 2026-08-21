package api

// Setup wizard validation tests. Focused on boundary checks that keep
// bad inputs out of SQL / shell / settings — not the happy-path
// integration flow (that's exercised by hack/local-ingest-test.sh +
// deploy/mail-mbsync/smoke-test.sh).

import (
	"context"
	"encoding/json"
	"errors"
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

func TestSetupIntent_PersistsRecommendation(t *testing.T) {
	d := openTestDB(t)
	s := &Server{DB: d, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	admin := &pluginapi.Principal{Kind: "user", UserID: 1, Role: "admin"}
	req := httptest.NewRequest(http.MethodPost, "/api/admin/setup/intent",
		strings.NewReader(`{"intent":"freelance"}`))
	req = req.WithContext(auth.WithPrincipal(req.Context(), admin))
	rec := httptest.NewRecorder()
	s.SaveSetupIntent(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"recommended_preset":"freelance"`) {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if err := settings.Set(req.Context(), d, settings.KeyPreset, "solo"); err != nil {
		t.Fatal(err)
	}

	stateReq := httptest.NewRequest(http.MethodGet, "/api/admin/setup/state", nil)
	stateReq = stateReq.WithContext(auth.WithPrincipal(stateReq.Context(), admin))
	stateRec := httptest.NewRecorder()
	s.SetupState(stateRec, stateReq)
	if stateRec.Code != http.StatusOK || !strings.Contains(stateRec.Body.String(), `"intent":"freelance"`) ||
		!strings.Contains(stateRec.Body.String(), `"recommended_preset":"freelance"`) ||
		!strings.Contains(stateRec.Body.String(), `"current_preset":"solo"`) ||
		strings.Contains(stateRec.Body.String(), `"steps"`) {
		t.Fatalf("state status=%d body=%s", stateRec.Code, stateRec.Body.String())
	}
}

func TestSetupIntent_RejectsUnknownValue(t *testing.T) {
	d := openTestDB(t)
	s := &Server{DB: d, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	req := httptest.NewRequest(http.MethodPost, "/api/admin/setup/intent",
		strings.NewReader(`{"intent":"paper-hoard"}`))
	req = req.WithContext(auth.WithPrincipal(req.Context(), &pluginapi.Principal{
		Kind: "user", UserID: 1, Role: "admin",
	}))
	rec := httptest.NewRecorder()
	s.SaveSetupIntent(rec, req)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "bad_intent") {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestSetupStepRouteRemoved(t *testing.T) {
	mux := http.NewServeMux()
	(&Server{}).registerSetup(mux)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/admin/setup/step/archive", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("removed setup step route returned %d", rec.Code)
	}
}

func TestMicrosoftOAuthSettingsRoutesRemoved(t *testing.T) {
	mux := http.NewServeMux()
	(&Server{}).registerSetup(mux)
	for _, method := range []string{http.MethodGet, http.MethodPost} {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(method, "/api/admin/settings/microsoft-oauth", nil))
		if rec.Code != http.StatusNotFound {
			t.Fatalf("%s removed Microsoft OAuth settings route returned %d", method, rec.Code)
		}
	}
}

func TestSaveLLMSettings_SealsKeyAndActivatesLive(t *testing.T) {
	d := openTestDB(t)
	key, err := suchicrypto.LoadOrCreateKey(filepath.Join(t.TempDir(), "secret.key"))
	if err != nil {
		t.Fatal(err)
	}
	active := false
	s := &Server{
		DB:      d,
		Log:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		LLMAEAD: key,
		LLMReloader: func(context.Context) error {
			active = true
			return nil
		},
		LLMStatusReader: func(ctx context.Context) (LLMSettingsStatus, error) {
			cfg, err := settings.ResolveLLMConfig(ctx, d, settings.LLMConfig{}, key)
			if err != nil {
				return LLMSettingsStatus{}, err
			}
			return LLMSettingsStatus{
				Enabled:             !cfg.Disabled && cfg.EndpointURL != "",
				Active:              active,
				EndpointURL:         cfg.EndpointURL,
				Model:               cfg.Model,
				EgressAck:           cfg.EgressAck,
				HasAPIKey:           cfg.APIKey != "",
				ConfidenceThreshold: cfg.ConfidenceThreshold,
			}, nil
		},
	}
	body := `{"endpoint_url":"http://127.0.0.1:11434/v1","model":"qwen2.5:7b","api_key":"top-secret","confidence_threshold":0.8,"archive_enabled":false,"archive_auto_threshold":0.85,"archive_review_threshold":0.6}`
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
		Active bool `json:"active"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if !response.Active || !active {
		t.Fatal("first-time activation did not apply to the running classifier")
	}
	var responseFields map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &responseFields); err != nil {
		t.Fatal(err)
	}
	if _, ok := responseFields["restart_required"]; ok {
		t.Fatal("live settings response still exposes restart_required")
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
	if resolved.ConfidenceThreshold != 0.8 {
		t.Fatalf("confidence threshold = %v", resolved.ConfidenceThreshold)
	}
	archive := settings.ResolveArchiveClassifierConfig(req.Context(), d)
	if archive.Enabled || archive.AutoThreshold != 0.85 || archive.ReviewThreshold != 0.6 {
		t.Fatalf("archive config = %#v", archive)
	}
	if _, ok := stored["ciphertext"]; !ok {
		t.Fatalf("stored key is not a sealed envelope: %#v", stored)
	}

	statusReq := httptest.NewRequest(http.MethodGet, "/api/admin/settings/llm", nil)
	statusReq = statusReq.WithContext(auth.WithPrincipal(statusReq.Context(), &pluginapi.Principal{
		Kind: "user", UserID: 1, Role: "admin",
	}))
	statusRec := httptest.NewRecorder()
	s.GetLLMSettings(statusRec, statusReq)
	if statusRec.Code != http.StatusOK {
		t.Fatalf("status endpoint=%d body=%s", statusRec.Code, statusRec.Body.String())
	}
	if strings.Contains(statusRec.Body.String(), "top-secret") {
		t.Fatal("status response exposed API key")
	}
	var status LLMSettingsStatus
	if err := json.Unmarshal(statusRec.Body.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	if !status.HasAPIKey || !status.Enabled || !status.Active {
		t.Fatalf("unexpected masked status: %#v", status)
	}
	if status.ArchiveEnabled || status.ArchiveAuto != 0.85 || status.ArchiveReview != 0.6 {
		t.Fatalf("archive status = %#v", status)
	}

	clearBody := `{"endpoint_url":"http://127.0.0.1:11434/v1","model":"qwen2.5:7b","clear_api_key":true}`
	clearReq := httptest.NewRequest(http.MethodPost, "/api/admin/settings/llm", strings.NewReader(clearBody))
	clearReq = clearReq.WithContext(auth.WithPrincipal(clearReq.Context(), &pluginapi.Principal{
		Kind: "user", UserID: 1, Role: "admin",
	}))
	clearRec := httptest.NewRecorder()
	s.SaveLLMSettings(clearRec, clearReq)
	if clearRec.Code != http.StatusOK {
		t.Fatalf("clear status=%d body=%s", clearRec.Code, clearRec.Body.String())
	}
	cleared, err := settings.ResolveLLMConfig(clearReq.Context(), d,
		settings.LLMConfig{APIKey: "environment-key"}, key)
	if err != nil {
		t.Fatal(err)
	}
	if cleared.APIKey != "" {
		t.Fatalf("cleared API key resolved to %q", cleared.APIKey)
	}
	clearedStatus, err := s.loadLLMSettingsStatus(clearReq.Context())
	if err != nil {
		t.Fatal(err)
	}
	if clearedStatus.HasAPIKey {
		t.Fatal("masked status still reports an API key after explicit clear")
	}
}

func TestSaveLLMSettings_Disables(t *testing.T) {
	d := openTestDB(t)
	s := &Server{DB: d, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	body := `{"enabled":false,"endpoint_url":"http://127.0.0.1:11434/v1","model":"qwen2.5:7b"}`
	req := httptest.NewRequest(http.MethodPost, "/api/admin/settings/llm", strings.NewReader(body))
	req = req.WithContext(auth.WithPrincipal(req.Context(), &pluginapi.Principal{
		Kind: "user", UserID: 1, Role: "admin",
	}))
	rec := httptest.NewRecorder()
	s.SaveLLMSettings(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var response map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if _, ok := response["restart_required"]; ok {
		t.Fatal("disable response still exposes restart_required")
	}
	resolved, err := settings.ResolveLLMConfig(req.Context(), d, settings.LLMConfig{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !resolved.Disabled {
		t.Fatal("disabled state was not persisted")
	}
}

func TestSaveLLMSettings_RejectsEndpointQuery(t *testing.T) {
	d := openTestDB(t)
	s := &Server{DB: d, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	body := `{"endpoint_url":"http://127.0.0.1:11434/v1?key=secret","model":"qwen2.5:7b"}`
	req := httptest.NewRequest(http.MethodPost, "/api/admin/settings/llm", strings.NewReader(body))
	req = req.WithContext(auth.WithPrincipal(req.Context(), &pluginapi.Principal{
		Kind: "user", UserID: 1, Role: "admin",
	}))
	rec := httptest.NewRecorder()
	s.SaveLLMSettings(rec, req)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "bad_url") {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestLLMSettingsTest_UsesCandidateWithoutSaving(t *testing.T) {
	d := openTestDB(t)
	called := false
	s := &Server{
		DB:  d,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		LLMTester: func(_ context.Context, cfg LLMTestConfig) (LLMTestResult, error) {
			called = true
			if cfg.EndpointURL != "http://127.0.0.1:11434/v1" || cfg.Model != "qwen2.5:7b" || cfg.APIKey != "candidate" {
				t.Fatalf("unexpected candidate: %#v", cfg)
			}
			return LLMTestResult{Title: "Connection test", Confidence: 0.91, ElapsedMS: 12}, nil
		},
	}
	body := `{"endpoint_url":"http://127.0.0.1:11434/v1","model":"qwen2.5:7b","api_key":"candidate"}`
	req := httptest.NewRequest(http.MethodPost, "/api/admin/settings/llm/test", strings.NewReader(body))
	req = req.WithContext(auth.WithPrincipal(req.Context(), &pluginapi.Principal{
		Kind: "user", UserID: 1, Role: "admin",
	}))
	rec := httptest.NewRecorder()
	s.TestLLMSettings(rec, req)
	if rec.Code != http.StatusOK || !called {
		t.Fatalf("status=%d called=%v body=%s", rec.Code, called, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"title":"Connection test"`) ||
		!strings.Contains(rec.Body.String(), `"elapsed_ms":12`) {
		t.Fatalf("sanitized test result missing: %s", rec.Body.String())
	}
	var endpoint string
	if err := settings.Get(req.Context(), d, settings.KeyLLMEndpointURL, &endpoint); !errors.Is(err, settings.ErrNotFound) {
		t.Fatalf("test endpoint persisted candidate config: endpoint=%q err=%v", endpoint, err)
	}
}

func TestSaveLLMSettings_RejectsConfidenceOutsideWebBounds(t *testing.T) {
	d := openTestDB(t)
	s := &Server{DB: d, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	body := `{"endpoint_url":"http://127.0.0.1:11434/v1","model":"qwen2.5:7b","confidence_threshold":0.2}`
	req := httptest.NewRequest(http.MethodPost, "/api/admin/settings/llm", strings.NewReader(body))
	req = req.WithContext(auth.WithPrincipal(req.Context(), &pluginapi.Principal{
		Kind: "user", UserID: 1, Role: "admin",
	}))
	rec := httptest.NewRecorder()
	s.SaveLLMSettings(rec, req)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "bad_confidence_threshold") {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestSaveLLMSettingsRejectsInvalidArchiveThresholds(t *testing.T) {
	d := openTestDB(t)
	s := &Server{DB: d, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	body := `{"enabled":false,"archive_auto_threshold":0.7,"archive_review_threshold":0.7}`
	req := httptest.NewRequest(http.MethodPost, "/api/admin/settings/llm", strings.NewReader(body))
	req = req.WithContext(auth.WithPrincipal(req.Context(), &pluginapi.Principal{
		Kind: "user", UserID: 1, Role: "admin",
	}))
	rec := httptest.NewRecorder()
	s.SaveLLMSettings(rec, req)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "bad_archive_thresholds") {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestSetupRuntimeSettingsApplyLiveAndReadBack(t *testing.T) {
	d := openTestDB(t)
	seedUser(t, d, 1)
	preferenceReloads, fsReloads := 0, 0
	s := &Server{
		DB: d, Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		RuntimePreferencesReloader: func(context.Context) error {
			preferenceReloads++
			return nil
		},
		FSWatchReloader: func(context.Context) error {
			fsReloads++
			return nil
		},
	}
	admin := &pluginapi.Principal{Kind: "user", UserID: 1, Role: "admin"}

	prefReq := httptest.NewRequest(http.MethodPost, "/api/admin/settings/preferences",
		strings.NewReader(`{"backup_interval_hours":12,"ocr_languages":["deu","eng"]}`))
	prefReq = prefReq.WithContext(auth.WithPrincipal(prefReq.Context(), admin))
	prefRec := httptest.NewRecorder()
	s.SavePreferences(prefRec, prefReq)
	if prefRec.Code != http.StatusNoContent || preferenceReloads != 1 {
		t.Fatalf("preferences status=%d reloads=%d body=%s", prefRec.Code, preferenceReloads, prefRec.Body.String())
	}
	prefGet := httptest.NewRequest(http.MethodGet, "/api/admin/settings/preferences", nil)
	prefGet = prefGet.WithContext(auth.WithPrincipal(prefGet.Context(), admin))
	prefGetRec := httptest.NewRecorder()
	s.GetPreferences(prefGetRec, prefGet)
	if prefGetRec.Code != http.StatusOK || !strings.Contains(prefGetRec.Body.String(), `"deu"`) ||
		!strings.Contains(prefGetRec.Body.String(), `"backup_interval_hours":12`) {
		t.Fatalf("preferences readback status=%d body=%s", prefGetRec.Code, prefGetRec.Body.String())
	}

	fsReq := httptest.NewRequest(http.MethodPost, "/api/admin/settings/ingest",
		strings.NewReader(`{"fs_watch_dir":"/tmp/suchi-inbox","fs_watch_owner_email":"u1@t.local"}`))
	fsReq = fsReq.WithContext(auth.WithPrincipal(fsReq.Context(), admin))
	fsRec := httptest.NewRecorder()
	s.SaveIngestSettings(fsRec, fsReq)
	if fsRec.Code != http.StatusNoContent || fsReloads != 1 {
		t.Fatalf("fs-watch status=%d reloads=%d body=%s", fsRec.Code, fsReloads, fsRec.Body.String())
	}
	fsGet := httptest.NewRequest(http.MethodGet, "/api/admin/settings/ingest", nil)
	fsGet = fsGet.WithContext(auth.WithPrincipal(fsGet.Context(), admin))
	fsGetRec := httptest.NewRecorder()
	s.GetIngestSettings(fsGetRec, fsGet)
	if fsGetRec.Code != http.StatusOK || !strings.Contains(fsGetRec.Body.String(), "/tmp/suchi-inbox") {
		t.Fatalf("fs-watch readback status=%d body=%s", fsGetRec.Code, fsGetRec.Body.String())
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
