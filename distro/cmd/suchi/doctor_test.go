package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/johnnybravo-xyz/suchi/core/config"
	"github.com/johnnybravo-xyz/suchi/core/db"
	migrations "github.com/johnnybravo-xyz/suchi/core/db/migrations"
	"github.com/johnnybravo-xyz/suchi/core/settings"
)

func TestEnumerateEgressIncludesDatabaseRowsAndRedactsSecrets(t *testing.T) {
	ctx := context.Background()
	d, err := db.Open(ctx, filepath.Join(t.TempDir(), "suchi.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	migs, err := db.LoadMigrations(migrations.FS, ".")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Migrate(ctx, d, migs, slog.New(slog.NewTextHandler(io.Discard, nil))); err != nil {
		t.Fatal(err)
	}
	if _, err := d.ExecWrite(ctx, `
		INSERT INTO users(id, email, display_name, role, created_at, updated_at)
		VALUES (1, 'admin@example.test', 'Admin', 'admin', 0, 0);
		INSERT INTO email_accounts(
			name, owner_id, provider, host, port, use_tls, folder,
			poll_interval_min, auth_method, username, sealed_secret,
			intake_policy, enabled, created_at, updated_at
		) VALUES (
			'Bills', 1, 'custom', 'mail.example.test', 993, 1, 'INBOX',
			10, 'password', 'private-user', X'01',
			'{"rules":[{"selection":"files","content":"files_only"}]}', 1, 0, 0
		);
	`); err != nil {
		t.Fatal(err)
	}

	cfg := &config.Config{
		OIDCIssuerURL:                    "https://id.example.test/tenant?hint=secret",
		IngestIMAPOAuthClientIDMicrosoft: "11111111-1111-1111-1111-111111111111",
		IngestIMAPOAuthScopesMicrosoft:   "offline_access",
	}
	got, err := enumerateEgress(ctx, d, cfg, "https://api.openai.com/v1?key=secret")
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(got, "\n")
	for _, want := range []string{
		"imap mail.example.test:993 (Bills)",
		"llm-classifier https://api.openai.com",
		"oauth.microsoft https://login.microsoftonline.com/common",
		"oidc.discovery https://id.example.test/tenant",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("egress inventory missing %q:\n%s", want, joined)
		}
	}
	for _, secret := range []string{"private-user", "user:pass", "/private/token", "key=secret"} {
		if strings.Contains(joined, secret) {
			t.Errorf("egress inventory exposed %q:\n%s", secret, joined)
		}
	}
}

func TestResolveDoctorLLMEndpointRetainsEnvironmentFallbackForEmptySetting(t *testing.T) {
	ctx := context.Background()
	d, err := db.Open(ctx, filepath.Join(t.TempDir(), "suchi.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	migs, err := db.LoadMigrations(migrations.FS, ".")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Migrate(ctx, d, migs, slog.New(slog.NewTextHandler(io.Discard, nil))); err != nil {
		t.Fatal(err)
	}
	if err := settings.Set(ctx, d, settings.KeyLLMEndpointURL, ""); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{LLMEndpointURL: "http://127.0.0.1:11434/v1"}
	if got := resolveDoctorLLMEndpoint(ctx, d, cfg); got != cfg.LLMEndpointURL {
		t.Fatalf("endpoint = %q, want environment fallback %q", got, cfg.LLMEndpointURL)
	}
}

func TestResolveDoctorDataDirFallsBackForUnavailableDefault(t *testing.T) {
	primaryParent := t.TempDir()
	primary := filepath.Join(primaryParent, "not-a-directory", "suchi")
	if err := os.WriteFile(filepath.Dir(primary), []byte("block mkdir"), 0o600); err != nil {
		t.Fatal(err)
	}
	fallbackBase := t.TempDir()

	got, usedFallback, err := resolveDoctorDataDir(primary, true, func() (string, error) {
		return fallbackBase, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(fallbackBase, "suchi")
	if got != want || !usedFallback {
		t.Fatalf("resolveDoctorDataDir() = (%q, %t), want (%q, true)", got, usedFallback, want)
	}
	if info, err := os.Stat(want); err != nil || !info.IsDir() {
		t.Fatalf("fallback directory was not created: info=%v err=%v", info, err)
	}
}

func TestResolveDoctorDataDirDoesNotReplaceExplicitPath(t *testing.T) {
	primaryParent := t.TempDir()
	primary := filepath.Join(primaryParent, "not-a-directory", "suchi")
	if err := os.WriteFile(filepath.Dir(primary), []byte("block mkdir"), 0o600); err != nil {
		t.Fatal(err)
	}
	called := false

	got, usedFallback, err := resolveDoctorDataDir(primary, false, func() (string, error) {
		called = true
		return "", errors.New("must not be called")
	})
	if err == nil {
		t.Fatal("resolveDoctorDataDir() succeeded for an unavailable explicit path")
	}
	if got != "" || usedFallback || called {
		t.Fatalf("resolveDoctorDataDir() = (%q, %t, %v), fallback called=%t", got, usedFallback, err, called)
	}
}
