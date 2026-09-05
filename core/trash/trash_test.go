package trash

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/johnnybravo-xyz/suchi/core/blob"
	"github.com/johnnybravo-xyz/suchi/core/db"
	migrations "github.com/johnnybravo-xyz/suchi/core/db/migrations"
	pluginapi "github.com/johnnybravo-xyz/suchi/plugin-api"
)

func newTestService(t *testing.T) (*Service, *db.DB, *blob.CAS, string) {
	t.Helper()
	ctx := context.Background()
	dataDir := t.TempDir()
	database, err := db.Open(ctx, filepath.Join(dataDir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	loaded, err := db.LoadMigrations(migrations.FS, ".")
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	if err := db.Migrate(ctx, database, loaded, log); err != nil {
		t.Fatal(err)
	}
	cas, err := blob.New(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	renderRoot := filepath.Join(dataDir, "rendered")
	if err := os.MkdirAll(renderRoot, 0o750); err != nil {
		t.Fatal(err)
	}
	service, err := New(database, renderRoot, log)
	if err != nil {
		t.Fatal(err)
	}
	seedBaseRows(t, database)
	return service, database, cas, renderRoot
}

func seedBaseRows(t *testing.T, database *db.DB) {
	t.Helper()
	_, err := database.Write.ExecContext(context.Background(), `
		INSERT INTO users(id, email, display_name, role, created_at, updated_at)
		VALUES (1, 'owner@example.test', 'Owner', 'admin', 0, 0),
		       (2, 'member@example.test', 'Member', 'member', 0, 0),
		       (3, 'avatar@example.test', 'Avatar owner', 'member', 0, 0);
		INSERT INTO jd_areas(code_start, code_end, name, position)
		VALUES (0, 9, 'Test', 0);
		INSERT INTO jd_categories(id, area_start, code, name, system)
		VALUES (1, 0, 1, 'Inbox', 1);
	`)
	if err != nil {
		t.Fatal(err)
	}
}

func putBlob(t *testing.T, cas *blob.CAS, content string) string {
	t.Helper()
	ref, err := cas.Put(strings.NewReader(content))
	if err != nil {
		t.Fatal(err)
	}
	return ref.SHA256
}

func seedDocument(t *testing.T, database *db.DB, id, ownerID int64, hash string, trashedAt *int64) {
	t.Helper()
	_, err := database.Write.ExecContext(context.Background(), `
		INSERT INTO documents(
			id, owner_id, original_blob, original_size, title, jd_category_id,
			created_at, updated_at, trashed_at
		) VALUES (?, ?, ?, 1, ?, 1, 1, 1, ?)
	`, id, ownerID, hash, "document", trashedAt)
	if err != nil {
		t.Fatal(err)
	}
}

func rowExists(t *testing.T, database *db.DB, table string, id int64) bool {
	t.Helper()
	var count int
	if err := database.Read.QueryRowContext(context.Background(),
		"SELECT COUNT(*) FROM "+table+" WHERE id = ?", id).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count == 1
}

func TestPurgeExpiredUsesExactThirtyDayBoundary(t *testing.T) {
	service, database, cas, _ := newTestService(t)
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	withinWindow := now.Add(-29 * 24 * time.Hour).Unix()
	exactlyExpired := now.Add(-Retention).Unix()
	newWindow := now.Add(-24 * time.Hour).Unix()

	seedDocument(t, database, 1, 1, putBlob(t, cas, "within-window"), &withinWindow)
	expiredHash := putBlob(t, cas, "expired")
	seedDocument(t, database, 2, 1, expiredHash, &exactlyExpired)
	seedDocument(t, database, 3, 1, putBlob(t, cas, "restored-live"), nil)
	seedDocument(t, database, 4, 1, putBlob(t, cas, "retrashed-new-window"), &newWindow)

	report, err := service.PurgeExpired(context.Background(), now)
	if err != nil {
		t.Fatal(err)
	}
	if report.Purged != 1 {
		t.Fatalf("purged=%d, want 1", report.Purged)
	}
	if rowExists(t, database, "documents", 2) {
		t.Fatal("document at the exact 30-day boundary survived")
	}
	for _, id := range []int64{1, 3, 4} {
		if !rowExists(t, database, "documents", id) {
			t.Fatalf("document %d was purged inside its recovery window", id)
		}
	}
	if _, err := cas.Stat(expiredHash); err != nil {
		t.Fatalf("retention removed blob before offline GC: %v", err)
	}
}

func TestPurgeRemovesOwnedStateAndRenderedFilesButRetainsBlobs(t *testing.T) {
	service, database, cas, renderRoot := newTestService(t)
	trashedAt := time.Now().Add(-time.Hour).Unix()
	sharedHash := putBlob(t, cas, "shared-document-blob")
	archiveHash := putBlob(t, cas, "unique-archive-blob")
	decryptedHash := putBlob(t, cas, "unique-decrypted-blob")
	avatarHash := putBlob(t, cas, "shared-avatar-blob")
	seedDocument(t, database, 10, 1, sharedHash, &trashedAt)
	seedDocument(t, database, 11, 2, sharedHash, nil)

	if _, err := database.Write.ExecContext(context.Background(),
		`UPDATE users SET avatar_sha = ? WHERE id = 3`, avatarHash); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Write.ExecContext(context.Background(), `
		UPDATE documents SET archive_blob = ?, decrypted_blob = ?, thumb_sha = ? WHERE id = 10;
		INSERT INTO notes(id, document_id, note, created_at) VALUES (1, 10, 'private note', 1);
		INSERT INTO approval_defs(id, slug, version, spec_json, created_by, created_at)
		VALUES (1, 'purge-test', 1, '{}', 1, 1);
		INSERT INTO approval_runs(id, def_id, doc_id, state, current_state, vars_json, state_entered_at, started_by, started_at)
		VALUES (1, 1, 10, 'running', 'review', '{}', 1, 1, 1);
		INSERT INTO approval_tasks(id, run_id, state_key, assignee, prompt, choices_json, status, created_at)
		VALUES (1, 1, 'review', 'user:1', 'Review', '[]', 'open', 1);
		INSERT INTO jobs(id, kind, doc_id, payload, state, next_run_at, created_at, updated_at)
		VALUES (1, 'test', 10, '{}', 'pending', 1, 1, 1);
		INSERT INTO object_acls(id, object_kind, object_id, principal_kind, principal_id, perm_bits, created_at, created_by)
		VALUES (1, 'document', 10, 'user', 2, 4, 1, 1);
		INSERT INTO decryption_passwords(id, owner_id, ciphertext, created_at, last_used_doc_id)
		VALUES (1, 1, X'01', 1, 10);
		INSERT INTO share_links(id, token, doc_ids_json, created_by, label, created_at)
		VALUES (1, 'purge-link', '[10,11]', 1, 'shared docs', 1);
		INSERT INTO audit_events(id, ts, actor_kind, actor_id, action, object_kind, object_id, after_json)
		VALUES (1, 1, 'user', 1, 'document.update', 'document', 10, '{"title":"private"}');
	`, archiveHash, decryptedHash, avatarHash); err != nil {
		t.Fatal(err)
	}

	validRelative := filepath.Join("Owner", "document.pdf")
	validPath := filepath.Join(renderRoot, validRelative)
	if err := os.MkdirAll(filepath.Dir(validPath), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("missing-cas-target", validPath); err != nil {
		t.Fatal(err)
	}
	outsidePath := filepath.Join(filepath.Dir(renderRoot), "outside.txt")
	if err := os.WriteFile(outsidePath, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Write.ExecContext(context.Background(), `
		INSERT INTO render_moves(id, document_id, prev_path, new_path, state, created_at, applied_at)
		VALUES (1, 10, '', ?, 'applied', 1, 1),
		       (2, 10, ?, '../outside.txt', 'pending', 2, NULL)
	`, filepath.ToSlash(validRelative), filepath.ToSlash(validRelative)); err != nil {
		t.Fatal(err)
	}

	actor := &pluginapi.Principal{Kind: "user", UserID: 1}
	report, err := service.PurgeOne(context.Background(), 10, actor, "request-1")
	if err != nil {
		t.Fatal(err)
	}
	if report.Purged != 1 || report.RenderedFilesRemoved != 1 || report.CleanupFailures != 1 {
		t.Fatalf("unexpected report: %+v", report)
	}
	if rowExists(t, database, "documents", 10) || !rowExists(t, database, "documents", 11) {
		t.Fatal("purge removed the wrong document rows")
	}
	for _, table := range []string{"notes", "approval_runs", "approval_tasks", "jobs", "object_acls", "share_links", "render_moves"} {
		var count int
		if err := database.Read.QueryRowContext(context.Background(), "SELECT COUNT(*) FROM "+table).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 0 {
			t.Fatalf("%s count=%d, want 0", table, count)
		}
	}
	var lastUsed sql.NullInt64
	if err := database.Read.QueryRowContext(context.Background(),
		`SELECT last_used_doc_id FROM decryption_passwords WHERE id = 1`).Scan(&lastUsed); err != nil {
		t.Fatal(err)
	}
	if lastUsed.Valid {
		t.Fatalf("last_used_doc_id=%d, want NULL", lastUsed.Int64)
	}
	if _, err := os.Lstat(validPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("rendered path error=%v, want not-exist", err)
	}
	if content, err := os.ReadFile(outsidePath); err != nil || string(content) != "keep" {
		t.Fatalf("unsafe render path touched: content=%q err=%v", content, err)
	}
	for name, hash := range map[string]string{
		"shared": sharedHash, "avatar": avatarHash, "archive": archiveHash, "decrypted": decryptedHash,
	} {
		if _, err := cas.Stat(hash); err != nil {
			t.Fatalf("%s hash was removed: %v", name, err)
		}
	}

	var (
		action, actorKind, afterJSON, requestID string
		actorID                                 int64
		beforeJSON                              sql.NullString
	)
	if err := database.Read.QueryRowContext(context.Background(), `
		SELECT action, actor_kind, actor_id, before_json, after_json, request_id
		FROM audit_events WHERE object_kind = 'document' AND object_id = 10
	`).Scan(&action, &actorKind, &actorID, &beforeJSON, &afterJSON, &requestID); err != nil {
		t.Fatal(err)
	}
	var after map[string]any
	if err := json.Unmarshal([]byte(afterJSON), &after); err != nil {
		t.Fatal(err)
	}
	if action != "document.purge" || actorKind != "user" || actorID != 1 || beforeJSON.Valid || requestID != "request-1" || after["owner_id"] != float64(1) {
		t.Fatalf("unexpected purge audit: action=%q kind=%q actor=%d before=%v after=%s request=%q",
			action, actorKind, actorID, beforeJSON, afterJSON, requestID)
	}
	if _, err := service.PurgeOne(context.Background(), 11, actor, "request-2"); !errors.Is(err, ErrNotTrashed) {
		t.Fatalf("live document purge error=%v, want ErrNotTrashed", err)
	}
}
