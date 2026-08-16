package localauth

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/johnnybravo-xyz/suchi/core/db"
	migrations "github.com/johnnybravo-xyz/suchi/core/db/migrations"
)

func openTestPlugin(t *testing.T) *Plugin {
	t.Helper()
	ctx := context.Background()
	d, err := db.Open(ctx, filepath.Join(t.TempDir(), "t.db"))
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
	p, err := New(ctx, d, log)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestEnsureDevAdmin_Refusals(t *testing.T) {
	ctx := context.Background()

	cases := []struct {
		name     string
		email    string
		password string
		wantErr  string
	}{
		{"empty_email", "", DevAdminPassword, "email and password required"},
		{"empty_password", DevAdminEmail, "", "email and password required"},
		{"short_password", DevAdminEmail, "1234567", "at least 8 chars"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := openTestPlugin(t)
			err := p.EnsureDevAdmin(ctx, tc.email, tc.password)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("want %q in err, got %v", tc.wantErr, err)
			}
		})
	}
}

func TestEnsureDevAdmin_RefusesRoleCollision(t *testing.T) {
	ctx := context.Background()
	p := openTestPlugin(t)
	// Seed a real member user with the same email dev-mode wants.
	if _, err := p.db.Write.ExecContext(ctx, `
		INSERT INTO users(email, display_name, role, created_at, updated_at)
		VALUES (?, 'member seed', 'member', 0, 0)
	`, "victim@suchi.local"); err != nil {
		t.Fatal(err)
	}
	err := p.EnsureDevAdmin(ctx, "victim@suchi.local", DevAdminPassword)
	if err == nil || !strings.Contains(err.Error(), "role") {
		t.Fatalf("want role-collision refusal, got %v", err)
	}
	// The member must still be a member — no silent promotion.
	var role string
	if err := p.db.Read.QueryRowContext(ctx,
		`SELECT role FROM users WHERE email = ?`, "victim@suchi.local").Scan(&role); err != nil {
		t.Fatal(err)
	}
	if role != "member" {
		t.Fatalf("member silently promoted to %q — refusal path is broken", role)
	}
}

// TestEnsureDevAdmin_RefusesOtherAdmin locks in the accidental-prod
// lateral-takeover guard: an operator with a legit admin who boots with
// SUCHI_DEV=1 + loopback PUBLIC_URL must not gain a second admin row
// carrying the public default credentials.
func TestEnsureDevAdmin_RefusesOtherAdmin(t *testing.T) {
	ctx := context.Background()
	p := openTestPlugin(t)
	if _, err := p.db.Write.ExecContext(ctx, `
		INSERT INTO users(email, display_name, role, created_at, updated_at)
		VALUES (?, 'owner', 'admin', 0, 0)
	`, "owner@example.com"); err != nil {
		t.Fatal(err)
	}
	err := p.EnsureDevAdmin(ctx, DevAdminEmail, DevAdminPassword)
	if err == nil || !strings.Contains(err.Error(), "other admin") {
		t.Fatalf("want other-admin refusal, got %v", err)
	}
	// Verify dev@suchi.local was NOT inserted.
	var count int
	if err := p.db.Read.QueryRowContext(ctx,
		`SELECT count(*) FROM users WHERE email = ?`, DevAdminEmail).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("dev admin row created despite refusal — takeover path is open")
	}
}

// TestEnsureDevAdmin_AllowsMemberSideBySide confirms multi-account
// testing survives the new refusal — only OTHER admins block dev-mode;
// member accounts (Alice, Bob) do not.
func TestEnsureDevAdmin_AllowsMemberSideBySide(t *testing.T) {
	ctx := context.Background()
	p := openTestPlugin(t)
	if _, err := p.db.Write.ExecContext(ctx, `
		INSERT INTO users(email, display_name, role, created_at, updated_at)
		VALUES (?, 'alice', 'member', 0, 0)
	`, "alice@example.com"); err != nil {
		t.Fatal(err)
	}
	if err := p.EnsureDevAdmin(ctx, DevAdminEmail, DevAdminPassword); err != nil {
		t.Fatalf("EnsureDevAdmin refused despite only a member peer: %v", err)
	}
	var role string
	if err := p.db.Read.QueryRowContext(ctx,
		`SELECT role FROM users WHERE email = ?`, DevAdminEmail).Scan(&role); err != nil {
		t.Fatal(err)
	}
	if role != "admin" {
		t.Fatalf("want admin, got %q", role)
	}
}

func TestEnsureDevAdmin_HappyPathInsertsAndBurnsToken(t *testing.T) {
	ctx := context.Background()
	p := openTestPlugin(t)
	// Fresh plugin has a setup token minted (usersEmpty=true).
	if p.SetupToken() == "" {
		t.Fatal("expected setup token to be minted on fresh DB")
	}
	if err := p.EnsureDevAdmin(ctx, DevAdminEmail, DevAdminPassword); err != nil {
		t.Fatal(err)
	}
	if p.SetupToken() != "" {
		t.Fatal("setup token should be burned after EnsureDevAdmin succeeds")
	}
	var role string
	if err := p.db.Read.QueryRowContext(ctx,
		`SELECT role FROM users WHERE email = ?`, DevAdminEmail).Scan(&role); err != nil {
		t.Fatal(err)
	}
	if role != "admin" {
		t.Fatalf("want admin role, got %q", role)
	}
}

func TestEnsureDevAdmin_DoesNotResurrectDisabledAdmin(t *testing.T) {
	ctx := context.Background()
	p := openTestPlugin(t)
	// Insert an admin with disabled=1 — simulates a security quarantine.
	if _, err := p.db.Write.ExecContext(ctx, `
		INSERT INTO users(email, display_name, role, disabled, created_at, updated_at)
		VALUES (?, 'quarantined', 'admin', 1, 0, 0)
	`, DevAdminEmail); err != nil {
		t.Fatal(err)
	}
	if err := p.EnsureDevAdmin(ctx, DevAdminEmail, DevAdminPassword); err != nil {
		t.Fatal(err)
	}
	var disabled int
	if err := p.db.Read.QueryRowContext(ctx,
		`SELECT disabled FROM users WHERE email = ?`, DevAdminEmail).Scan(&disabled); err != nil {
		t.Fatal(err)
	}
	if disabled != 1 {
		t.Fatalf("disabled flag was silently reset — quarantine broken")
	}
}

func TestEnsureDevAdmin_IdempotentPasswordReset(t *testing.T) {
	ctx := context.Background()
	p := openTestPlugin(t)
	if err := p.EnsureDevAdmin(ctx, DevAdminEmail, DevAdminPassword); err != nil {
		t.Fatal(err)
	}
	var firstHash string
	if err := p.db.Read.QueryRowContext(ctx,
		`SELECT password_hash FROM users WHERE email = ?`, DevAdminEmail).Scan(&firstHash); err != nil {
		t.Fatal(err)
	}
	if err := p.EnsureDevAdmin(ctx, DevAdminEmail, "rotatedrotated"); err != nil {
		t.Fatal(err)
	}
	var secondHash string
	if err := p.db.Read.QueryRowContext(ctx,
		`SELECT password_hash FROM users WHERE email = ?`, DevAdminEmail).Scan(&secondHash); err != nil {
		t.Fatal(err)
	}
	if firstHash == secondHash {
		t.Fatal("password_hash should rotate on re-invocation with a new password")
	}
}
