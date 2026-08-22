package ingest_test

import (
	"context"
	"database/sql"
	"io"
	"log/slog"
	"testing"

	"github.com/johnnybravo-xyz/suchi/core/db"
	migrations "github.com/johnnybravo-xyz/suchi/core/db/migrations"
	"github.com/johnnybravo-xyz/suchi/core/ingest"
)

func openSourceDB(t *testing.T) *db.DB {
	t.Helper()
	ctx := context.Background()
	d, err := db.Open(ctx, t.TempDir()+"/suchi.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	migs, err := db.LoadMigrations(migrations.FS, ".")
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	if err := db.Migrate(ctx, d, migs, log); err != nil {
		t.Fatal(err)
	}
	_, err = d.Write.ExecContext(ctx, `
		INSERT INTO users(id, email, display_name, role, created_at, updated_at)
		VALUES (1, 'owner@example.com', 'Owner', 'admin', 0, 0);
		INSERT INTO jd_areas(code_start, code_end, name, position)
		VALUES (0, 9, 'System', 0);
		INSERT INTO jd_categories(id, area_start, code, name, system)
		VALUES (1, 0, 1, 'Inbox', 1);
		INSERT INTO documents(id, owner_id, original_blob, original_size, title,
		                      jd_category_id, created_at, updated_at)
		VALUES (1, 1, 'sha-1', 1, 'one', 1, 0, 0),
		       (2, 1, 'sha-2', 1, 'two', 1, 0, 0);
		INSERT INTO email_accounts(
			id, name, owner_id, provider, host, port, folder,
			auth_method, username, sealed_secret, created_at, updated_at
		) VALUES (
			1, 'Personal Outlook', 1, 'microsoft', 'outlook.office365.com',
			993, 'INBOX', 'oauth', 'ritesh@example.com', X'00', 0, 0
		);
	`)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func TestRecordAndCopySources(t *testing.T) {
	d := openSourceDB(t)
	ctx := context.Background()
	for _, label := range []string{"Personal Outlook", "Renamed Outlook"} {
		if err := d.WriteTx(ctx, func(tx *sql.Tx) error {
			return ingest.RecordMailboxSource(ctx, tx, 1, 1,
				label, "ritesh@example.com / INBOX", 100)
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := d.WriteTx(ctx, func(tx *sql.Tx) error {
		return ingest.CopySources(ctx, tx, 1, 2)
	}); err != nil {
		t.Fatal(err)
	}
	for _, id := range []int64{1, 2} {
		var count int
		var accountID sql.NullInt64
		if err := d.Read.QueryRowContext(ctx,
			`SELECT COUNT(*), MAX(email_account_id) FROM document_sources WHERE document_id = ?`, id).
			Scan(&count, &accountID); err != nil {
			t.Fatal(err)
		}
		if count != 1 {
			t.Fatalf("document %d source count = %d, want 1", id, count)
		}
		if !accountID.Valid || accountID.Int64 != 1 {
			t.Fatalf("document %d email_account_id = %v, want 1", id, accountID)
		}
	}
}
