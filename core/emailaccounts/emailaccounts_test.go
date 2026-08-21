package emailaccounts_test

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/johnnybravo-xyz/suchi/core/crypto"
	"github.com/johnnybravo-xyz/suchi/core/db"
	migrations "github.com/johnnybravo-xyz/suchi/core/db/migrations"
	"github.com/johnnybravo-xyz/suchi/core/emailaccounts"
)

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
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	if err := db.Migrate(ctx, d, migs, log); err != nil {
		t.Fatal(err)
	}
	return d
}

func seedUser(t *testing.T, ctx context.Context, d *db.DB, email string) int64 {
	t.Helper()
	var id int64
	if err := d.WriteTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `
			INSERT INTO users(email, display_name, role, created_at, updated_at)
			VALUES (?, 'test', 'admin', 0, 0)`, email)
		if err != nil {
			return err
		}
		id, err = res.LastInsertId()
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return id
}

func newAEAD(t *testing.T) *crypto.AEADKey {
	t.Helper()
	k, err := crypto.LoadOrCreateKey(filepath.Join(t.TempDir(), ".decrypt-key"))
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func TestCRUDRoundTrip(t *testing.T) {
	ctx := context.Background()
	d := setupDB(t)
	uid := seedUser(t, ctx, d, "owner@example.com")
	k := newAEAD(t)

	sealed, err := emailaccounts.SealPassword(k, "hunter2")
	if err != nil {
		t.Fatal(err)
	}
	created, err := emailaccounts.Create(ctx, d, emailaccounts.Account{
		Name:         "primary",
		OwnerID:      uid,
		Provider:     emailaccounts.ProviderFastmail,
		Host:         "imap.fastmail.com",
		Port:         993,
		UseTLS:       true,
		AuthMethod:   emailaccounts.AuthPassword,
		Username:     "owner@example.com",
		SealedSecret: sealed,
		Enabled:      true,
		MarkSeen:     true,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if created.ID == 0 {
		t.Fatal("expected non-zero id on create")
	}

	got, err := emailaccounts.Get(ctx, d, created.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Name != "primary" || got.Port != 993 || !got.UseTLS ||
		got.Provider != emailaccounts.ProviderFastmail ||
		got.AuthMethod != emailaccounts.AuthPassword || !got.MarkSeen {
		t.Fatalf("get returned wrong shape: %+v", got)
	}
	if !bytes.Equal(got.SealedSecret, sealed) {
		t.Fatal("sealed secret round-trip mismatch")
	}
	if got.Folder != "INBOX" || got.PollIntervalMin != 10 {
		t.Fatalf("defaults not applied: folder=%q poll=%d", got.Folder, got.PollIntervalMin)
	}

	// Sparse patch: rename + disable + change poll interval, leave the rest.
	newName := "primary-fastmail"
	disabled := false
	newPoll := 30
	patched, err := emailaccounts.Patch(ctx, d, created.ID, emailaccounts.AccountPatch{
		Name:            &newName,
		Enabled:         &disabled,
		PollIntervalMin: &newPoll,
	})
	if err != nil {
		t.Fatalf("patch: %v", err)
	}
	if patched.Name != newName || patched.Enabled || patched.PollIntervalMin != 30 {
		t.Fatalf("patch didn't apply: %+v", patched)
	}
	if patched.Host != "imap.fastmail.com" {
		t.Fatalf("patch clobbered unrelated field host=%q", patched.Host)
	}

	if err := emailaccounts.UpdateUIDCursor(ctx, d, created.ID, 42, 7); err != nil {
		t.Fatal(err)
	}
	newPoll = 45
	patched, err = emailaccounts.Patch(ctx, d, created.ID, emailaccounts.AccountPatch{PollIntervalMin: &newPoll})
	if err != nil {
		t.Fatal(err)
	}
	if patched.LastUIDSeen != 42 || patched.UIDValiditySeen != 7 {
		t.Fatal("non-source patch reset the UID cursor")
	}
	newFolder := "Receipts"
	patched, err = emailaccounts.Patch(ctx, d, created.ID, emailaccounts.AccountPatch{Folder: &newFolder})
	if err != nil {
		t.Fatal(err)
	}
	if patched.LastUIDSeen != 0 || patched.UIDValiditySeen != 0 {
		t.Fatalf("source patch kept cursor: uid=%d uidvalidity=%d", patched.LastUIDSeen, patched.UIDValiditySeen)
	}

	if err := emailaccounts.Delete(ctx, d, created.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := emailaccounts.Get(ctx, d, created.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("get after delete: want ErrNoRows, got %v", err)
	}
	if err := emailaccounts.Delete(ctx, d, created.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("second delete: want ErrNoRows, got %v", err)
	}
}

func TestListEnabledFilters(t *testing.T) {
	ctx := context.Background()
	d := setupDB(t)
	uid := seedUser(t, ctx, d, "a@example.com")
	k := newAEAD(t)
	sealed, _ := emailaccounts.SealPassword(k, "p")

	mk := func(name string, enabled bool) {
		_, err := emailaccounts.Create(ctx, d, emailaccounts.Account{
			Name: name, OwnerID: uid, Provider: emailaccounts.ProviderCustom,
			Host: "h", Port: 993, UseTLS: true,
			AuthMethod: emailaccounts.AuthPassword, Username: "u", SealedSecret: sealed,
			Enabled: enabled,
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	mk("on-1", true)
	mk("off", false)
	mk("on-2", true)

	all, err := emailaccounts.List(ctx, d)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 3 {
		t.Fatalf("List: want 3, got %d", len(all))
	}

	enabled, err := emailaccounts.ListEnabled(ctx, d)
	if err != nil {
		t.Fatal(err)
	}
	if len(enabled) != 2 {
		t.Fatalf("ListEnabled: want 2, got %d", len(enabled))
	}
	for _, a := range enabled {
		if !a.Enabled {
			t.Fatalf("ListEnabled returned disabled row: %+v", a)
		}
	}
	if _, err := d.ExecWrite(ctx, `UPDATE users SET disabled = 1 WHERE id = ?`, uid); err != nil {
		t.Fatal(err)
	}
	enabled, err = emailaccounts.ListEnabled(ctx, d)
	if err != nil {
		t.Fatal(err)
	}
	if len(enabled) != 0 {
		t.Fatalf("ListEnabled returned %d mailboxes for a disabled owner", len(enabled))
	}
}

func TestMarkSync(t *testing.T) {
	ctx := context.Background()
	d := setupDB(t)
	uid := seedUser(t, ctx, d, "a@example.com")
	k := newAEAD(t)
	sealed, _ := emailaccounts.SealPassword(k, "p")

	a, err := emailaccounts.Create(ctx, d, emailaccounts.Account{
		Name: "m", OwnerID: uid, Provider: emailaccounts.ProviderCustom,
		Host: "h", Port: 993, UseTLS: true,
		AuthMethod: emailaccounts.AuthPassword, Username: "u", SealedSecret: sealed,
		Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}

	if err := emailaccounts.MarkSync(ctx, d, a.ID, 12345, "boom"); err != nil {
		t.Fatal(err)
	}
	got, _ := emailaccounts.Get(ctx, d, a.ID)
	if got.LastSyncAt != 12345 || got.LastError != "boom" {
		t.Fatalf("MarkSync did not persist: sync=%d err=%q", got.LastSyncAt, got.LastError)
	}

	// Clearing the error passes empty string; column becomes NULL, read as "".
	if err := emailaccounts.MarkSync(ctx, d, a.ID, 12346, ""); err != nil {
		t.Fatal(err)
	}
	got, _ = emailaccounts.Get(ctx, d, a.ID)
	if got.LastError != "" {
		t.Fatalf("MarkSync clear: want empty last_error, got %q", got.LastError)
	}
}

func TestCreateValidations(t *testing.T) {
	ctx := context.Background()
	d := setupDB(t)
	_, err := emailaccounts.Create(ctx, d, emailaccounts.Account{})
	if err == nil {
		t.Fatal("expected validation error for empty account")
	}

	uid := seedUser(t, ctx, d, "validation@example.com")
	sealed, _ := emailaccounts.SealPassword(newAEAD(t), "p")
	base := emailaccounts.Account{
		Name: "mail", OwnerID: uid, Provider: emailaccounts.ProviderCustom,
		Host: "mail.example.com", Port: 993, UseTLS: true,
		AuthMethod: emailaccounts.AuthPassword, Username: "u", SealedSecret: sealed,
	}
	for _, poll := range []int{-1, emailaccounts.MaxPollIntervalMin + 1} {
		candidate := base
		candidate.PollIntervalMin = poll
		if _, err := emailaccounts.Create(ctx, d, candidate); err == nil {
			t.Fatalf("poll interval %d should be rejected", poll)
		}
	}
	invalidPoll := 0
	created, err := emailaccounts.Create(ctx, d, base)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := emailaccounts.Patch(ctx, d, created.ID, emailaccounts.AccountPatch{PollIntervalMin: &invalidPoll}); err == nil {
		t.Fatal("zero poll interval patch should be rejected")
	}
	emptyHost := ""
	if _, err := emailaccounts.Patch(ctx, d, created.ID, emailaccounts.AccountPatch{Host: &emptyHost}); err == nil {
		t.Fatal("empty host patch should be rejected")
	}
	negativeSince := int64(-1)
	if _, err := emailaccounts.Patch(ctx, d, created.ID, emailaccounts.AccountPatch{SyncSince: &negativeSince}); err == nil {
		t.Fatal("negative sync_since patch should be rejected")
	}
	longName := strings.Repeat("x", 201)
	if _, err := emailaccounts.Patch(ctx, d, created.ID, emailaccounts.AccountPatch{Name: &longName}); err == nil {
		t.Fatal("overlong name patch should be rejected")
	}
	badCA := filepath.Join(t.TempDir(), "missing-ca.pem")
	base.TLSCAFile = badCA
	if _, err := emailaccounts.Create(ctx, d, base); err == nil || !strings.Contains(err.Error(), "tls_ca_file") {
		t.Fatalf("invalid CA file error = %v", err)
	}
	if _, err := d.ExecWrite(ctx, `UPDATE email_accounts SET tls_ca_file = ? WHERE id = ?`, badCA, created.ID); err != nil {
		t.Fatal(err)
	}
	on := true
	if _, err := emailaccounts.Patch(ctx, d, created.ID, emailaccounts.AccountPatch{Enabled: &on}); err == nil || !strings.Contains(err.Error(), "tls_ca_file") {
		t.Fatalf("enabling with invalid CA file error = %v", err)
	}
}

func TestSealRoundTripAndBadKey(t *testing.T) {
	k1 := newAEAD(t)
	k2 := newAEAD(t) // distinct key material

	sealed, err := emailaccounts.SealPassword(k1, "s3cret!")
	if err != nil {
		t.Fatal(err)
	}
	pw, err := emailaccounts.OpenPassword(k1, sealed)
	if err != nil {
		t.Fatal(err)
	}
	if pw != "s3cret!" {
		t.Fatalf("password round-trip mismatch: %q", pw)
	}

	// Wrong key must fail (AEAD auth tag mismatch).
	if _, err := emailaccounts.OpenPassword(k2, sealed); err == nil {
		t.Fatal("open with foreign key must fail")
	}
}

func TestMicrosoftOAuthCredentialEnvelope(t *testing.T) {
	k := newAEAD(t)
	clientID := "11111111-1111-1111-1111-111111111111"
	cache := []byte(`{"account":"abc","refresh_token":"xyz"}`)
	sealed, err := emailaccounts.SealMicrosoftOAuthCredential(k, emailaccounts.MicrosoftOAuthCredential{
		ClientID: clientID, CacheJSON: cache,
	})
	if err != nil {
		t.Fatal(err)
	}
	opened, err := emailaccounts.OpenMicrosoftOAuthCredential(k, sealed)
	if err != nil {
		t.Fatal(err)
	}
	if opened.ClientID != clientID || !bytes.Equal(opened.CacheJSON, cache) {
		t.Fatalf("opened credential = %#v", opened)
	}
	legacySeal, err := k.Seal(cache)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := emailaccounts.OpenMicrosoftOAuthCredential(k, legacySeal); err == nil {
		t.Fatal("raw token cache should require reauthentication")
	}
}
