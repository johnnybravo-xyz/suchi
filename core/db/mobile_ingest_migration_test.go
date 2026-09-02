package db_test

import (
	"context"
	"database/sql"
	"io"
	"log/slog"
	"path/filepath"
	"testing"

	"github.com/johnnybravo-xyz/suchi/core/db"
	migrations "github.com/johnnybravo-xyz/suchi/core/db/migrations"
)

func TestMobileIngestMigrationUpgradesPopulatedBaseline(t *testing.T) {
	ctx := context.Background()
	d, err := db.Open(ctx, filepath.Join(t.TempDir(), "upgrade.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	migs, err := db.LoadMigrations(migrations.FS, ".")
	if err != nil {
		t.Fatal(err)
	}
	var baseline []db.Migration
	for _, migration := range migs {
		if migration.Version == 1 {
			baseline = append(baseline, migration)
		}
	}
	if len(baseline) != 1 {
		t.Fatalf("baseline migrations = %d, want 1", len(baseline))
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	if err := db.Migrate(ctx, d, baseline, log); err != nil {
		t.Fatal(err)
	}
	seedMobileMigrationOwnerAndCategory(t, d)
	if _, err := d.ExecWrite(ctx, `
		INSERT INTO documents(
			id, owner_id, original_blob, original_size, title,
			jd_category_id, created_at, updated_at
		) VALUES (1, 1, 'old-sha', 12, 'Existing', 1, 10, 10)
	`); err != nil {
		t.Fatal(err)
	}

	if err := db.Migrate(ctx, d, migs, log); err != nil {
		t.Fatal(err)
	}

	var (
		contentSource string
		confidence    sql.NullFloat64
		language      string
		receivedAt    sql.NullInt64
		splitOrigin   int64
	)
	if err := d.Read.QueryRowContext(ctx, `
		SELECT content_source, device_content_confidence,
		       device_ocr_language, device_content_received_at, split_origin_id
		FROM documents WHERE id = 1
	`).Scan(&contentSource, &confidence, &language, &receivedAt, &splitOrigin); err != nil {
		t.Fatal(err)
	}
	if contentSource != "" || confidence.Valid || language != "" || receivedAt.Valid || splitOrigin != 0 {
		t.Fatalf("unexpected migrated defaults: source=%q confidence=%v language=%q received=%v origin=%d",
			contentSource, confidence, language, receivedAt, splitOrigin)
	}
	if _, err := d.ExecWrite(ctx, `UPDATE documents SET content_source = 'untrusted' WHERE id = 1`); err == nil {
		t.Fatal("invalid content_source was accepted")
	}
	if _, err := d.ExecWrite(ctx, `
		UPDATE documents
		SET content = 'device text', content_source = 'device_ocr',
		    device_content_confidence = 0.8, device_ocr_language = 'en_US',
		    device_content_received_at = 20, split_origin_id = 1, split_index = 0
		WHERE id = 1
	`); err != nil {
		t.Fatal(err)
	}
	if _, err := d.ExecWrite(ctx, `
		INSERT INTO documents(
			id, owner_id, original_blob, original_size, title,
			jd_category_id, created_at, updated_at, split_origin_id, split_index
		) VALUES (2, 1, 'new-sha', 13, 'Collision', 1, 11, 11, 1, 0)
	`); err == nil {
		t.Fatal("duplicate split origin/index was accepted")
	}
	if _, err := d.ExecWrite(ctx, `
		INSERT INTO upload_idempotency(
			user_id, idempotency_key, operation, predecessor_id,
			request_fingerprint, sha256, document_id,
			response_status, response_json, created_at
		) VALUES (1, 'key', 'document', 0, 'fingerprint', 'old-sha', 1, 201, '{}', 20)
	`); err != nil {
		t.Fatal(err)
	}
	if _, err := d.ExecWrite(ctx, `DELETE FROM documents WHERE id = 1`); err != nil {
		t.Fatal(err)
	}
	var idempotencyRows int
	if err := d.Read.QueryRowContext(ctx, `SELECT COUNT(*) FROM upload_idempotency`).Scan(&idempotencyRows); err != nil {
		t.Fatal(err)
	}
	if idempotencyRows != 0 {
		t.Fatalf("idempotency rows after document deletion = %d, want 0", idempotencyRows)
	}
}

func TestMobileIngestMigrationCreatesFreshSchema(t *testing.T) {
	ctx := context.Background()
	d, err := db.Open(ctx, filepath.Join(t.TempDir(), "fresh.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	migs, err := db.LoadMigrations(migrations.FS, ".")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Migrate(ctx, d, migs, slog.New(slog.NewTextHandler(io.Discard, nil))); err != nil {
		t.Fatal(err)
	}
	seedMobileMigrationOwnerAndCategory(t, d)
	if _, err := d.ExecWrite(ctx, `
		INSERT INTO documents(
			id, owner_id, original_blob, original_size, title,
			jd_category_id, created_at, updated_at, content, content_source,
			device_content_confidence, device_ocr_language,
			device_content_received_at, split_origin_id, split_index
		) VALUES (1, 1, 'sha', 12, 'Fresh', 1, 10, 10, 'text', 'server',
		          0.7, 'en_US', 10, 1, 0)
	`); err != nil {
		t.Fatal(err)
	}
	if _, err := d.ExecWrite(ctx, `
		INSERT INTO upload_idempotency(
			user_id, idempotency_key, operation, predecessor_id,
			request_fingerprint, sha256, document_id,
			response_status, response_json, created_at
		) VALUES (1, 'key', 'version', 1, 'fingerprint', 'sha', 1, 200, '{}', 10)
	`); err != nil {
		t.Fatal(err)
	}
}

func seedMobileMigrationOwnerAndCategory(t *testing.T, d *db.DB) {
	t.Helper()
	ctx := context.Background()
	if _, err := d.ExecWrite(ctx, `
		INSERT INTO users(id, email, display_name, role, created_at, updated_at)
		VALUES (1, 'mobile@example.test', 'Mobile', 'member', 1, 1);
		INSERT INTO jd_areas(code_start, code_end, name, position)
		VALUES (40, 49, 'Inbox', 1);
		INSERT INTO jd_categories(id, area_start, code, name, system)
		VALUES (1, 40, 49, 'Inbox', 1);
	`); err != nil {
		t.Fatal(err)
	}
}
