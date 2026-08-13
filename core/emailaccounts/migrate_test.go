package emailaccounts_test

import (
	"context"
	"errors"
	"testing"

	"github.com/johnnybravo-xyz/suchi/core/emailaccounts"
	"github.com/johnnybravo-xyz/suchi/core/settings"
)

func TestMigrateFromLegacySettings(t *testing.T) {
	ctx := context.Background()
	d := setupDB(t)
	seedUser(t, ctx, d, "owner@example.com")
	k := newAEAD(t)

	// Pre-seed the legacy ingest.imap_* keys.
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(settings.Set(ctx, d, settings.KeyIMAPHost, "imap.example.com"))
	must(settings.Set(ctx, d, settings.KeyIMAPPort, 993))
	must(settings.Set(ctx, d, settings.KeyIMAPUsername, "owner@example.com"))
	must(settings.Set(ctx, d, settings.KeyIMAPPasswordSealed, "plaintext-pw"))
	must(settings.Set(ctx, d, settings.KeyIMAPFolder, "INBOX"))
	must(settings.Set(ctx, d, settings.KeyIMAPPollIntervalMin, 15))
	must(settings.Set(ctx, d, settings.KeyIMAPOwnerEmail, "owner@example.com"))

	if err := emailaccounts.MigrateFromLegacySettings(ctx, d, k); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	// One row lands.
	rows, err := emailaccounts.List(ctx, d)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("want 1 account, got %d", len(rows))
	}
	a := rows[0]
	if a.Name != "Legacy mailbox" || a.Host != "imap.example.com" || a.Port != 993 ||
		a.PollIntervalMin != 15 || a.Provider != emailaccounts.ProviderCustom ||
		a.AuthMethod != emailaccounts.AuthPassword {
		t.Fatalf("wrong shape: %+v", a)
	}
	// Password unseals to the plaintext we fed in.
	pw, err := emailaccounts.OpenPassword(k, a.SealedSecret)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if pw != "plaintext-pw" {
		t.Fatalf("password mismatch: %q", pw)
	}

	// Legacy keys are gone.
	for _, key := range []string{
		settings.KeyIMAPHost, settings.KeyIMAPPort, settings.KeyIMAPUsername,
		settings.KeyIMAPPasswordSealed, settings.KeyIMAPFolder,
		settings.KeyIMAPPollIntervalMin, settings.KeyIMAPOwnerEmail,
	} {
		var v any
		err := settings.Get(ctx, d, key, &v)
		if !errors.Is(err, settings.ErrNotFound) {
			t.Errorf("legacy key %s survived: err=%v val=%v", key, err, v)
		}
	}

	// Idempotent: second run is a no-op.
	if err := emailaccounts.MigrateFromLegacySettings(ctx, d, k); err != nil {
		t.Fatalf("second migrate: %v", err)
	}
	rows2, _ := emailaccounts.List(ctx, d)
	if len(rows2) != 1 {
		t.Fatalf("second migrate created a duplicate: got %d rows", len(rows2))
	}
}

func TestMigrateNoLegacyHostIsNoop(t *testing.T) {
	ctx := context.Background()
	d := setupDB(t)
	k := newAEAD(t)
	if err := emailaccounts.MigrateFromLegacySettings(ctx, d, k); err != nil {
		t.Fatalf("noop: %v", err)
	}
	rows, _ := emailaccounts.List(ctx, d)
	if len(rows) != 0 {
		t.Fatalf("want zero rows, got %d", len(rows))
	}
}

func TestMigrateOwnerEmailMismatchIsNoop(t *testing.T) {
	ctx := context.Background()
	d := setupDB(t)
	k := newAEAD(t)
	_ = settings.Set(ctx, d, settings.KeyIMAPHost, "imap.example.com")
	_ = settings.Set(ctx, d, settings.KeyIMAPUsername, "u")
	_ = settings.Set(ctx, d, settings.KeyIMAPPasswordSealed, "p")
	_ = settings.Set(ctx, d, settings.KeyIMAPOwnerEmail, "ghost@example.com")

	if err := emailaccounts.MigrateFromLegacySettings(ctx, d, k); err != nil {
		t.Fatalf("expected nil error on missing owner, got %v", err)
	}
	rows, _ := emailaccounts.List(ctx, d)
	if len(rows) != 0 {
		t.Fatalf("want zero rows on missing owner, got %d", len(rows))
	}
	// Legacy keys are preserved so an operator can fix the owner_email
	// and re-run the migration.
	var host string
	if err := settings.Get(ctx, d, settings.KeyIMAPHost, &host); err != nil {
		t.Fatalf("legacy host should remain on skip: %v", err)
	}
}
