package settings_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

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

func TestSetupState_EmptyIsPendingEverywhere(t *testing.T) {
	d := setupDB(t)
	s, err := settings.LoadSetupState(context.Background(), d)
	if err != nil {
		t.Fatal(err)
	}
	if s.CompletedAt != nil {
		t.Error("fresh setup should not have completed_at")
	}
	if len(s.Steps) != 0 {
		t.Errorf("fresh setup should have no steps recorded; got %v", s.Steps)
	}
}

func TestRecordStep_DoneThenSkipped(t *testing.T) {
	d := setupDB(t)
	ctx := context.Background()
	if err := settings.RecordStep(ctx, d, "welcome", settings.StepDone); err != nil {
		t.Fatal(err)
	}
	// Toggling to skipped should move it, not duplicate.
	if err := settings.RecordStep(ctx, d, "welcome", settings.StepSkipped); err != nil {
		t.Fatal(err)
	}
	s, _ := settings.LoadSetupState(ctx, d)
	if s.Steps["welcome"] != settings.StepSkipped {
		t.Errorf("expected welcome=skipped, got %v", s.Steps["welcome"])
	}
	// And no stale "done" copy hangs around.
	count := 0
	for k, v := range s.Steps {
		if k == "welcome" {
			count++
		}
		_ = v
	}
	if count != 1 {
		t.Errorf("expected exactly one welcome entry, got %d", count)
	}
}

func TestRecordStep_BadStatusRejected(t *testing.T) {
	d := setupDB(t)
	err := settings.RecordStep(context.Background(), d, "welcome", settings.StepStatus("weird"))
	if err == nil {
		t.Fatal("expected error for bad status")
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

func TestResolveLLMConfig_SettingsOverrideEnv(t *testing.T) {
	d := setupDB(t)
	ctx := context.Background()
	envFB := settings.LLMConfig{
		EndpointURL: "http://env.example/v1",
		Model:       "env-model",
		APIKey:      "env-key",
		EgressAck:   false,
	}
	// No settings written yet — should be identical to env.
	got, err := settings.ResolveLLMConfig(ctx, d, envFB, testSecretBox{})
	if err != nil {
		t.Fatal(err)
	}
	if got != envFB {
		t.Errorf("empty settings should pass env through, got %+v", got)
	}
	// Write one setting → that field wins, others stay env.
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
}

func TestSaveLLMConfig_UpdatesOneSnapshot(t *testing.T) {
	d := setupDB(t)
	ctx := context.Background()
	want := settings.LLMConfig{
		EndpointURL: "http://localhost:11434/v1",
		Model:       "qwen2.5:7b",
		EgressAck:   true,
	}
	if err := settings.SaveLLMConfig(ctx, d, want, testSecretBox{}, "secret-key"); err != nil {
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

func TestResolveLLMConfig_MigratesLegacyPlaintextKey(t *testing.T) {
	d := setupDB(t)
	ctx := context.Background()
	if err := settings.Set(ctx, d, settings.KeyLLMAPIKeySealed, "old-key"); err != nil {
		t.Fatal(err)
	}

	got, err := settings.ResolveLLMConfig(ctx, d, settings.LLMConfig{}, testSecretBox{})
	if err != nil {
		t.Fatal(err)
	}
	if got.APIKey != "old-key" {
		t.Fatalf("APIKey = %q", got.APIKey)
	}
	var stored map[string]any
	if err := settings.Get(ctx, d, settings.KeyLLMAPIKeySealed, &stored); err != nil {
		t.Fatal(err)
	}
	if stored["version"] != float64(1) {
		t.Fatalf("legacy key was not migrated: %#v", stored)
	}
}

func TestResolveFSWatchConfig_SettingsOverrideEnv(t *testing.T) {
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
}
