package api

// Locks in the shape POST /api/documents/ returns on a hash collision:
// the SPA reads matched.{id,title,added_at,correspondent,jd_category_id,
// storage_path} to render "you already uploaded this" without a second
// round-trip. Any accidental drift here would silently break that panel.

import (
	"context"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/johnnybravo-xyz/suchi/core/db"
)

// seedJDCategoryRow inserts one area+category using arbitrary code
// values. The shared seedJDCategory helper in mobile_compat_test.go
// only seeds code=11; these tests need 31 (Utilities) and 49 (Inbox)
// so they seed their own.
func seedJDCategoryRow(t *testing.T, d *db.DB, catID, code int64, name string) {
	t.Helper()
	ctx := context.Background()
	areaStart := code - (code % 10)
	if _, err := d.Write.ExecContext(ctx, `
		INSERT OR IGNORE INTO jd_areas(code_start, code_end, name, position)
		VALUES (?, ?, ?, 0)
	`, areaStart, areaStart+9, "test area"); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Write.ExecContext(ctx, `
		INSERT INTO jd_categories(id, area_start, code, name, description, system)
		VALUES (?, ?, ?, ?, NULL, 0)
	`, catID, areaStart, code, name); err != nil {
		t.Fatal(err)
	}
}

func TestLoadDuplicateMatch_FullProjection(t *testing.T) {
	ctx := context.Background()
	d := openTestDB(t)
	s := &Server{DB: d, Log: slog.New(slog.NewTextHandler(os.Stderr, nil))}

	// Owner + correspondent + storage-path scaffold.
	seedUser(t, d, 1)
	if _, err := d.Write.ExecContext(ctx, `
		INSERT INTO correspondents(id, name, slug, created_at, updated_at)
		VALUES (10, 'BESCOM', 'bescom', 0, 0)
	`); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Write.ExecContext(ctx, `
		INSERT INTO storage_paths(id, name, slug, path, created_at, updated_at)
		VALUES (20, '31 Utilities/BESCOM', '31-utilities-bescom', '31 Utilities/BESCOM/', 0, 0)
	`); err != nil {
		t.Fatal(err)
	}
	// openTestDB runs migrations only, not jd.EnsureTree; seed the
	// category we need for this row.
	seedJDCategoryRow(t, d, 31, 31, "Utilities")
	var catID int64 = 31

	added := time.Date(2026, 8, 13, 14, 23, 0, 0, time.UTC).Unix()
	if _, err := d.Write.ExecContext(ctx, `
		INSERT INTO documents(
			id, owner_id, original_blob, original_size, title,
			jd_category_id, correspondent_id, storage_path_id,
			added_at, created_at, updated_at
		) VALUES (
			4271, 1, 'sha-abc', 1024, 'BESCOM electricity bill Aug 2026',
			?, 10, 20, ?, 0, 0
		)
	`, catID, added); err != nil {
		t.Fatal(err)
	}

	got := s.loadDuplicateMatch(ctx, 4271)
	if got.ID != 4271 {
		t.Errorf("id: got %d want 4271", got.ID)
	}
	if got.Title != "BESCOM electricity bill Aug 2026" {
		t.Errorf("title: got %q", got.Title)
	}
	if got.AddedAt != "2026-08-13T14:23:00Z" {
		t.Errorf("added_at: got %q want RFC3339 UTC", got.AddedAt)
	}
	if got.Correspondent != "BESCOM" {
		t.Errorf("correspondent: got %q", got.Correspondent)
	}
	if got.JDCategoryID != 31 {
		t.Errorf("jd_category_id: got %d want 31", got.JDCategoryID)
	}
	if got.StoragePath != "31 Utilities/BESCOM" {
		t.Errorf("storage_path: got %q", got.StoragePath)
	}
}

// A minimally-filled row (no correspondent, no storage path) should
// return `omitempty` fields as empty strings — the SPA relies on that
// to hide the "Filed under …" line entirely.
func TestLoadDuplicateMatch_MinimalRow(t *testing.T) {
	ctx := context.Background()
	d := openTestDB(t)
	s := &Server{DB: d, Log: slog.New(slog.NewTextHandler(os.Stderr, nil))}

	seedUser(t, d, 1)
	seedJDCategoryRow(t, d, 49, 49, "Inbox")
	var inbox int64 = 49
	created := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).Unix()
	if _, err := d.Write.ExecContext(ctx, `
		INSERT INTO documents(id, owner_id, original_blob, original_size, title,
		                     jd_category_id, created_at, updated_at)
		VALUES (99, 1, 'sha-xyz', 1, '', ?, ?, ?)
	`, inbox, created, created); err != nil {
		t.Fatal(err)
	}
	got := s.loadDuplicateMatch(ctx, 99)
	if got.ID != 99 || got.JDCategoryID != 49 {
		t.Errorf("id/jd: got %+v", got)
	}
	if got.Correspondent != "" || got.StoragePath != "" {
		t.Errorf("optional fields should be empty: got %+v", got)
	}
	// created_at fallback when added_at is null.
	if got.AddedAt != "2026-01-01T00:00:00Z" {
		t.Errorf("added_at fallback: got %q", got.AddedAt)
	}
}

// A non-existent id returns just the id — no 500 from the caller.
func TestLoadDuplicateMatch_MissingRow(t *testing.T) {
	ctx := context.Background()
	d := openTestDB(t)
	s := &Server{DB: d, Log: slog.New(slog.NewTextHandler(os.Stderr, nil))}
	got := s.loadDuplicateMatch(ctx, 12345)
	if got.ID != 12345 {
		t.Errorf("id echo: got %d", got.ID)
	}
	if got.Title != "" || got.AddedAt != "" {
		t.Errorf("expected zero-value projection on missing row: %+v", got)
	}
}
