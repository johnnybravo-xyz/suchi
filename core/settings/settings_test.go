package settings_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/johnnybravo-xyz/suchi/core/db"
	migrations "github.com/johnnybravo-xyz/suchi/core/db/migrations"
	"github.com/johnnybravo-xyz/suchi/core/settings"
)

type testSecretBox struct{}

func (testSecretBox) Seal(plaintext []byte) ([]byte, error) {
	return append([]byte("sealed:"), plaintext...), nil
}

func (testSecretBox) Open(ciphertext []byte) ([]byte, error) {
	if !bytes.HasPrefix(ciphertext, []byte("sealed:")) {
		return nil, errors.New("bad ciphertext")
	}
	return bytes.TrimPrefix(ciphertext, []byte("sealed:")), nil
}

func setupDB(t *testing.T) *db.DB {
	t.Helper()
	clearRuntimeConfigEnv(t)
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "test.db")
	d, err := db.Open(ctx, path)
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
	return d
}

func clearRuntimeConfigEnv(t *testing.T) {
	t.Helper()
	for _, key := range []string{
		"LLM_ENDPOINT_URL", "LLM_MODEL", "LLM_API_KEY", "LLM_API_KEY_FILE",
		"LLM_EGRESS_ACK", "LLM_CONFIDENCE_THRESHOLD",
		"INGEST_FS_DIR", "INGEST_FS_OWNER_EMAIL",
		"BACKUP_INTERVAL", "OCR_LANGUAGES",
	} {
		value, present := os.LookupEnv(key)
		_ = os.Unsetenv(key)
		t.Cleanup(func() {
			if present {
				_ = os.Setenv(key, value)
			} else {
				_ = os.Unsetenv(key)
			}
		})
	}
}

func TestGet_MissingReturnsErrNotFound(t *testing.T) {
	d := setupDB(t)
	var out string
	err := settings.Get(context.Background(), d, "nonexistent.key", &out)
	if !errors.Is(err, settings.ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func TestSet_RoundTripString(t *testing.T) {
	d := setupDB(t)
	ctx := context.Background()
	if err := settings.Set(ctx, d, "test.key", "hello"); err != nil {
		t.Fatal(err)
	}
	var got string
	if err := settings.Get(ctx, d, "test.key", &got); err != nil {
		t.Fatal(err)
	}
	if got != "hello" {
		t.Errorf("got %q, want %q", got, "hello")
	}
}

func TestSet_Overwrite(t *testing.T) {
	d := setupDB(t)
	ctx := context.Background()
	_ = settings.Set(ctx, d, "k", "v1")
	if err := settings.Set(ctx, d, "k", "v2"); err != nil {
		t.Fatal(err)
	}
	var got string
	_ = settings.Get(ctx, d, "k", &got)
	if got != "v2" {
		t.Errorf("overwrite failed: got %q", got)
	}
}

func TestSetMany_RollsBackOnError(t *testing.T) {
	d := setupDB(t)
	ctx := context.Background()
	if _, err := d.Write.ExecContext(ctx, `
		CREATE TRIGGER reject_bad_setting
		BEFORE INSERT ON settings WHEN NEW.key = 'bad'
		BEGIN SELECT RAISE(ABORT, 'rejected'); END
	`); err != nil {
		t.Fatal(err)
	}

	err := settings.SetMany(ctx, d, map[string]any{"good": "saved", "bad": "rejected"})
	if err == nil {
		t.Fatal("expected batch failure")
	}
	var got string
	if err := settings.Get(ctx, d, "good", &got); !errors.Is(err, settings.ErrNotFound) {
		t.Fatalf("batch partially committed: value=%q err=%v", got, err)
	}
}

func TestSet_RoundTripStruct(t *testing.T) {
	d := setupDB(t)
	ctx := context.Background()
	type T struct{ A, B string }
	if err := settings.Set(ctx, d, "s", T{A: "x", B: "y"}); err != nil {
		t.Fatal(err)
	}
	var got T
	_ = settings.Get(ctx, d, "s", &got)
	if got.A != "x" || got.B != "y" {
		t.Errorf("struct mismatch: %+v", got)
	}
}

func TestDelete(t *testing.T) {
	d := setupDB(t)
	ctx := context.Background()
	_ = settings.Set(ctx, d, "kill.me", "yes")
	if err := settings.Delete(ctx, d, "kill.me"); err != nil {
		t.Fatal(err)
	}
	var got string
	err := settings.Get(ctx, d, "kill.me", &got)
	if !errors.Is(err, settings.ErrNotFound) {
		t.Errorf("delete didn't clear key: err=%v", err)
	}
}

func TestSetupState_Empty(t *testing.T) {
	d := setupDB(t)
	s, err := settings.LoadSetupState(context.Background(), d)
	if err != nil {
		t.Fatal(err)
	}
	if s.CompletedAt != nil {
		t.Error("fresh setup should not have completed_at")
	}
	if s.StartedAt != nil {
		t.Error("setup without an admin should not have started_at")
	}
	if s.FilingTreeChosen {
		t.Error("fresh setup should not have a filing-tree choice")
	}
}

func TestSetupState_StartsWithFirstAdmin(t *testing.T) {
	d := setupDB(t)
	ctx := context.Background()
	if _, err := d.Write.ExecContext(ctx, `
		INSERT INTO users(email, display_name, role, created_at, updated_at)
		VALUES ('member@example.test', 'Member', 'member', 50, 50),
		       ('later@example.test', 'Later admin', 'admin', 200, 200),
		       ('first@example.test', 'First admin', 'admin', 100, 100)
	`); err != nil {
		t.Fatal(err)
	}
	s, err := settings.LoadSetupState(ctx, d)
	if err != nil {
		t.Fatal(err)
	}
	if s.StartedAt == nil || *s.StartedAt != 100 {
		t.Fatalf("started_at = %v, want 100", s.StartedAt)
	}
}

func TestSetupState_LoadsIntentAndCurrentPreset(t *testing.T) {
	d := setupDB(t)
	ctx := context.Background()
	if err := settings.SetMany(ctx, d, map[string]any{
		settings.KeySetupIntent: "household",
		settings.KeyPreset:      "household",
	}); err != nil {
		t.Fatal(err)
	}
	s, err := settings.LoadSetupState(ctx, d)
	if err != nil {
		t.Fatal(err)
	}
	if s.Intent != "household" || s.CurrentPreset != "household" {
		t.Fatalf("setup state = %#v", s)
	}
	if !s.FilingTreeChosen {
		t.Error("an applied preset should count as a filing-tree choice")
	}
}

func TestSetupState_ImportedTaxonomyCountsAsChoice(t *testing.T) {
	d := setupDB(t)
	ctx := context.Background()
	if err := settings.Set(ctx, d, "taxonomy_preset_id", "custom-archive"); err != nil {
		t.Fatal(err)
	}
	s, err := settings.LoadSetupState(ctx, d)
	if err != nil {
		t.Fatal(err)
	}
	if !s.FilingTreeChosen {
		t.Error("an imported taxonomy should count as a filing-tree choice")
	}
}

func TestMarkComplete_UpdatesNeeded(t *testing.T) {
	d := setupDB(t)
	ctx := context.Background()
	if need, _ := settings.SetupNeeded(ctx, d); !need {
		t.Error("SetupNeeded should be true before MarkSetupComplete")
	}
	if err := settings.MarkSetupComplete(ctx, d); err != nil {
		t.Fatal(err)
	}
	if need, _ := settings.SetupNeeded(ctx, d); need {
		t.Error("SetupNeeded should be false after MarkSetupComplete")
	}
}

func TestResolveLLMConfig_Precedence(t *testing.T) {
	d := setupDB(t)
	ctx := context.Background()
	envFB := settings.LLMConfig{
		EndpointURL:         "http://env.example/v1",
		Model:               "env-model",
		APIKey:              "env-key",
		EgressAck:           false,
		ConfidenceThreshold: 0.7,
		DateAutoApply:       true,
	}
	// No settings written yet: use the boot fallback.
	got, err := settings.ResolveLLMConfig(ctx, d, envFB, testSecretBox{})
	if err != nil {
		t.Fatal(err)
	}
	if got != envFB {
		t.Errorf("empty settings should pass env through, got %+v", got)
	}
	// Database settings fill fields that were not explicitly configured.
	_ = settings.Set(ctx, d, settings.KeyLLMEndpointURL, "http://override.local/v1")
	_ = settings.Set(ctx, d, settings.KeyLLMEgressAck, true)
	got, err = settings.ResolveLLMConfig(ctx, d, envFB, testSecretBox{})
	if err != nil {
		t.Fatal(err)
	}
	if got.EndpointURL != "http://override.local/v1" {
		t.Errorf("settings should override endpoint, got %q", got.EndpointURL)
	}
	if got.Model != "env-model" {
		t.Errorf("unset settings should stay env, got model=%q", got.Model)
	}
	if !got.EgressAck {
		t.Errorf("egress_ack should follow settings, got false")
	}

	// An explicit environment/file value wins over the stored value.
	t.Setenv("LLM_ENDPOINT_URL", "http://env.example/v1")
	t.Setenv("LLM_EGRESS_ACK", "false")
	got, err = settings.ResolveLLMConfig(ctx, d, envFB, testSecretBox{})
	if err != nil {
		t.Fatal(err)
	}
	if got.EndpointURL != envFB.EndpointURL || got.EgressAck {
		t.Errorf("boot configuration should win, got %+v", got)
	}
}

func TestResolveLLMConfig_StoredDisabledWhenUnpinned(t *testing.T) {
	d := setupDB(t)
	ctx := context.Background()
	envFB := settings.LLMConfig{
		EndpointURL: "https://api.example.com/v1",
		Model:       "env-model",
		APIKey:      "env-key",
		EgressAck:   true,
	}
	if err := settings.SaveLLMConfig(ctx, d, settings.LLMConfig{
		EndpointURL: envFB.EndpointURL,
		Model:       envFB.Model,
		EgressAck:   true,
		Disabled:    true,
	}, testSecretBox{}, nil); err != nil {
		t.Fatal(err)
	}
	got, err := settings.ResolveLLMConfig(ctx, d, envFB, testSecretBox{})
	if err != nil {
		t.Fatal(err)
	}
	if !got.Disabled {
		t.Fatal("stored disabled state was not applied to an unpinned endpoint")
	}
	if got.EndpointURL != envFB.EndpointURL || got.Model != envFB.Model {
		t.Fatalf("disabled config should retain its connection fields: %#v", got)
	}
}

func TestLLMAPIKey_SealedAndResolved(t *testing.T) {
	d := setupDB(t)
	ctx := context.Background()
	box := testSecretBox{}
	if err := settings.SetLLMAPIKey(ctx, d, box, "secret-key"); err != nil {
		t.Fatal(err)
	}

	var stored map[string]any
	if err := settings.Get(ctx, d, settings.KeyLLMAPIKeySealed, &stored); err != nil {
		t.Fatal(err)
	}
	if stored["ciphertext"] == "secret-key" {
		t.Fatal("API key stored in plaintext")
	}

	got, err := settings.ResolveLLMConfig(ctx, d, settings.LLMConfig{}, box)
	if err != nil {
		t.Fatal(err)
	}
	if got.APIKey != "secret-key" {
		t.Fatalf("APIKey = %q", got.APIKey)
	}
	if !got.DateAutoApply {
		t.Fatal("date auto-apply should default on")
	}
}

func TestSaveLLMConfig_UpdatesOneSnapshot(t *testing.T) {
	d := setupDB(t)
	ctx := context.Background()
	want := settings.LLMConfig{
		EndpointURL:         "http://localhost:11434/v1",
		Model:               "qwen2.5:7b",
		EgressAck:           true,
		ConfidenceThreshold: 0.75,
	}
	apiKey := "secret-key"
	if err := settings.SaveLLMConfig(ctx, d, want, testSecretBox{}, &apiKey); err != nil {
		t.Fatal(err)
	}

	got, err := settings.ResolveLLMConfig(ctx, d, settings.LLMConfig{}, testSecretBox{})
	if err != nil {
		t.Fatal(err)
	}
	want.APIKey = "secret-key"
	if got != want {
		t.Fatalf("resolved config = %#v, want %#v", got, want)
	}
}

func TestArchiveClassifierConfigDefaultsAndPersists(t *testing.T) {
	d := setupDB(t)
	ctx := context.Background()
	got := settings.ResolveArchiveClassifierConfig(ctx, d)
	if !got.Enabled || got.AutoThreshold != 0.9 || got.ReviewThreshold != 0.5 {
		t.Fatalf("defaults = %#v", got)
	}

	want := settings.ArchiveClassifierConfig{
		Enabled: false, AutoThreshold: 0.85, ReviewThreshold: 0.65,
	}
	if err := settings.SaveArchiveClassifierConfig(ctx, d, want); err != nil {
		t.Fatal(err)
	}
	if got := settings.ResolveArchiveClassifierConfig(ctx, d); got != want {
		t.Fatalf("resolved config = %#v, want %#v", got, want)
	}
}

func TestSaveLLMConfig_ExplicitEmptyKeyOverridesUnpinnedFallback(t *testing.T) {
	d := setupDB(t)
	ctx := context.Background()
	empty := ""
	if err := settings.SaveLLMConfig(ctx, d, settings.LLMConfig{
		EndpointURL: "http://127.0.0.1:11434/v1", Model: "local",
	}, testSecretBox{}, &empty); err != nil {
		t.Fatal(err)
	}
	got, err := settings.ResolveLLMConfig(ctx, d, settings.LLMConfig{APIKey: "environment-key"}, testSecretBox{})
	if err != nil {
		t.Fatal(err)
	}
	if got.APIKey != "" {
		t.Fatalf("API key = %q, want explicit empty stored value", got.APIKey)
	}
}

func TestResolveFSWatchConfig_Precedence(t *testing.T) {
	d := setupDB(t)
	ctx := context.Background()
	envFB := settings.FSWatchConfig{Dir: "/env/dir", OwnerEmail: "env@e.com"}
	_ = settings.Set(ctx, d, settings.KeyFSWatchDir, "/settings/dir")
	got := settings.ResolveFSWatchConfig(ctx, d, envFB)
	if got.Dir != "/settings/dir" {
		t.Errorf("settings dir should win, got %q", got.Dir)
	}
	if got.OwnerEmail != "env@e.com" {
		t.Errorf("unset owner should stay env, got %q", got.OwnerEmail)
	}
	t.Setenv("INGEST_FS_DIR", envFB.Dir)
	got = settings.ResolveFSWatchConfig(ctx, d, envFB)
	if got.Dir != envFB.Dir {
		t.Errorf("configured dir should win, got %q", got.Dir)
	}
}

func TestResolveRuntimePreferences_Precedence(t *testing.T) {
	d := setupDB(t)
	ctx := context.Background()
	if err := settings.SetMany(ctx, d, map[string]any{
		settings.KeyBackupIntervalHours: 0,
		settings.KeyOCRLanguages:        []string{"deu", "eng"},
	}); err != nil {
		t.Fatal(err)
	}
	got := settings.ResolveRuntimePreferences(ctx, d, settings.RuntimePreferences{
		BackupInterval: 24 * time.Hour,
		OCRLanguages:   []string{"fra"},
	})
	if got.BackupInterval != 0 {
		t.Fatalf("backup interval = %s, want disabled", got.BackupInterval)
	}
	if len(got.OCRLanguages) != 2 || got.OCRLanguages[0] != "deu" || got.OCRLanguages[1] != "eng" {
		t.Fatalf("OCR languages = %#v", got.OCRLanguages)
	}
	t.Setenv("BACKUP_INTERVAL", "12h")
	t.Setenv("OCR_LANGUAGES", "fra")
	got = settings.ResolveRuntimePreferences(ctx, d, settings.RuntimePreferences{
		BackupInterval: 12 * time.Hour,
		OCRLanguages:   []string{"fra"},
	})
	if got.BackupInterval != 12*time.Hour || len(got.OCRLanguages) != 1 || got.OCRLanguages[0] != "fra" {
		t.Fatalf("boot preferences should win: %#v", got)
	}
}
