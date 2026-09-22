package jd_test

// Built-ins share the additive importer: applying or reapplying a preset must
// preserve both live and recoverable filing, not reset documents to Inbox.

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/johnnybravo-xyz/suchi/core/db"
	migrations "github.com/johnnybravo-xyz/suchi/core/db/migrations"
	"github.com/johnnybravo-xyz/suchi/core/jd"
)

func openTestDB(t *testing.T) *db.DB {
	t.Helper()
	ctx := context.Background()
	d, err := db.Open(ctx, filepath.Join(t.TempDir(), "test.db"))
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

func TestApplyPresetPreservesFiledAndTrashedDocuments(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))

	// Seed a user (documents.owner_id FK).
	if _, err := d.Write.ExecContext(ctx, `
		INSERT INTO users(id, email, display_name, role, created_at, updated_at)
		VALUES (1, 'u@t.local', 't', 'admin', 0, 0)
	`); err != nil {
		t.Fatal(err)
	}

	// Apply solo — plants the tree.
	if err := jd.ApplyPreset(ctx, d, log, "solo", jd.ApplyPresetOpts{SystemID: 1}); err != nil {
		t.Fatalf("initial preset apply: %v", err)
	}

	// Pick a non-inbox category to file the doc under.
	var nonInboxID int64
	if err := d.Read.QueryRowContext(ctx,
		`SELECT id FROM jd_categories WHERE system = 0 ORDER BY id LIMIT 1`).Scan(&nonInboxID); err != nil {
		t.Fatalf("pick non-inbox: %v", err)
	}

	// Insert one visible filed doc and one trashed doc. The latter is not
	// visible in the filing-tree picker, but its category FK must survive replacement.
	if _, err := d.Write.ExecContext(ctx, `
		INSERT INTO documents(
			system_id, owner_id, title, original_blob, original_size,
			jd_category_id, created_at, added_at, updated_at
		) VALUES (1, 1, 'test.md', 'sha-x', 1, ?, 0, 0, 0);
		INSERT INTO documents(
			system_id, owner_id, title, original_blob, original_size,
			jd_category_id, created_at, added_at, updated_at, trashed_at
		) VALUES (1, 1, 'trashed.md', 'sha-trash', 1, ?, 0, 0, 0, 1)
	`, nonInboxID, nonInboxID); err != nil {
		t.Fatal(err)
	}

	// A conflicting built-in must not silently replace existing categories.
	if err := jd.ApplyPreset(ctx, d, log, "household", jd.ApplyPresetOpts{SystemID: 1}); err == nil {
		t.Fatal("conflicting built-in silently replaced the current tree")
	}
	// Reapplying the same preset is safe without parking or renumbering docs.
	if err := jd.ApplyPreset(ctx, d, log, "solo", jd.ApplyPresetOpts{SystemID: 1}); err != nil {
		t.Fatal(err)
	}

	var parked int
	if err := d.Read.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM documents WHERE jd_category_id = ?`, nonInboxID).Scan(&parked); err != nil {
		t.Fatal(err)
	}
	if parked != 2 {
		t.Errorf("documents retaining their category = %d, want 2", parked)
	}
}

func TestApplyEveryPresetFromInboxOnlyBootstrap(t *testing.T) {
	for _, preset := range jd.Presets() {
		t.Run(preset.ID, func(t *testing.T) {
			d := openTestDB(t)
			ctx := context.Background()
			log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
			if err := jd.EnsureBootstrapTree(ctx, d, log, jd.ModeJD, 1); err != nil {
				t.Fatal(err)
			}
			if _, err := d.Write.ExecContext(ctx, `
				INSERT INTO users(id, email, display_name, role, created_at, updated_at)
				VALUES (1, 'u@t.local', 't', 'admin', 0, 0);
				INSERT INTO documents(system_id, owner_id, title, original_blob, original_size,
				                      jd_category_id, created_at, added_at, updated_at)
				VALUES (1, 1, 'waiting.pdf', 'sha-bootstrap', 1,
				        (SELECT id FROM jd_categories WHERE system = 1), 0, 0, 0)
			`); err != nil {
				t.Fatal(err)
			}
			if err := jd.ApplyPreset(ctx, d, log, preset.ID, jd.ApplyPresetOpts{SystemID: 1}); err != nil {
				t.Fatalf("apply from bootstrap: %v", err)
			}
			var onInbox int
			if err := d.Read.QueryRowContext(ctx, `
				SELECT COUNT(*) FROM documents d
				JOIN jd_categories c ON c.id = d.jd_category_id
				WHERE c.system = 1
			`).Scan(&onInbox); err != nil {
				t.Fatal(err)
			}
			if onInbox != 1 {
				t.Fatalf("documents on new Inbox = %d, want 1", onInbox)
			}
		})
	}
}
