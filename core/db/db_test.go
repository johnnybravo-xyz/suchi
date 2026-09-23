// SPDX-License-Identifier: AGPL-3.0-or-later

package db_test

import (
	"context"
	"database/sql"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/johnnybravo-xyz/suchi/core/db"
	migrations "github.com/johnnybravo-xyz/suchi/core/db/migrations"
)

func TestWriteTxFailureReleasesWriter(t *testing.T) {
	for _, failure := range []string{"commit", "panic"} {
		t.Run(failure, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
			defer cancel()
			d, err := db.Open(ctx, filepath.Join(t.TempDir(), "failure.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer d.Close()
			if _, err := d.ExecWrite(ctx, `CREATE TABLE parents(id INTEGER PRIMARY KEY);
				CREATE TABLE children(parent_id INTEGER REFERENCES parents(id) DEFERRABLE INITIALLY DEFERRED)`); err != nil {
				t.Fatal(err)
			}
			func() {
				defer func() {
					if got := recover(); failure == "panic" && got != "forced transaction panic" {
						t.Fatalf("panic = %v", got)
					} else if failure != "panic" && got != nil {
						t.Fatalf("unexpected panic: %v", got)
					}
				}()
				err = d.WriteTx(ctx, func(tx *sql.Tx) error {
					if _, err := tx.ExecContext(ctx, `INSERT INTO children(parent_id) VALUES (1)`); err != nil {
						return err
					}
					if failure == "panic" {
						panic("forced transaction panic")
					}
					return nil
				})
				if err == nil {
					t.Fatal("invalid transaction succeeded")
				}
			}()
			if _, err := d.ExecWrite(ctx, `INSERT INTO parents(id) VALUES (2)`); err != nil {
				t.Fatalf("writer unusable after %s: %v", failure, err)
			}
			var children int
			if err := d.Read.QueryRowContext(ctx, `SELECT count(*) FROM children`).Scan(&children); err != nil || children != 0 {
				t.Fatalf("failed transaction leaked rows: children=%d error=%v", children, err)
			}
		})
	}
}

func TestOpenPreservesLiteralFilenameAndDatabaseIsolation(t *testing.T) {
	root := t.TempDir()
	var databases []*db.DB
	for _, name := range []string{"archive #one%2f.db", "archive #two%2f.db"} {
		path := filepath.Join(root, name)
		d, err := db.Open(t.Context(), path)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = d.Close() })
		if _, err = d.ExecWrite(t.Context(), `CREATE TABLE marker(value TEXT); INSERT INTO marker(value) VALUES(?)`, name); err != nil {
			t.Fatal(err)
		}
		if _, err = os.Stat(path); err != nil {
			t.Fatalf("database not created at literal filename %q: %v", name, err)
		}
		databases = append(databases, d)
	}
	for _, d := range databases {
		var value string
		if err := d.Read.QueryRowContext(t.Context(), `SELECT value FROM marker`).Scan(&value); err != nil {
			t.Fatal(err)
		}
		if value != filepath.Base(d.Path) {
			t.Fatalf("distinct filenames shared a database: %q contains %q", d.Path, value)
		}
	}
}

// Smoke test: open a DB, run migrations, verify pragmas + writer discipline.
// Fast, hermetic — no external services.
func TestOpenAndMigrate(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "test.db")

	ctx := context.Background()
	d, err := db.Open(ctx, path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer d.Close()

	// Writer discipline: write pool must be capped at 1.
	if got := d.Write.Stats().MaxOpenConnections; got != 1 {
		t.Fatalf("write pool MaxOpenConns = %d, want 1", got)
	}
	if got := d.Read.Stats().MaxOpenConnections; got != 4 {
		t.Fatalf("read pool MaxOpenConns = %d, want 4", got)
	}

	// Pragma check: WAL journal, foreign_keys ON.
	var journal string
	if err := d.Read.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&journal); err != nil {
		t.Fatalf("pragma journal_mode: %v", err)
	}
	if journal != "wal" {
		t.Errorf("journal_mode = %q, want wal", journal)
	}

	// Run migrations.
	migs, err := db.LoadMigrations(migrations.FS, ".")
	if err != nil {
		t.Fatalf("load migrations: %v", err)
	}
	if len(migs) == 0 {
		t.Fatal("no migrations found — did embed break?")
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	if err := db.Migrate(ctx, d, migs, log); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	// user_version bumped.
	var v int
	if err := d.Read.QueryRowContext(ctx, "PRAGMA user_version").Scan(&v); err != nil {
		t.Fatalf("user_version scan: %v", err)
	}
	if v != migs[len(migs)-1].Version {
		t.Errorf("user_version = %d, want %d", v, migs[len(migs)-1].Version)
	}

	// A trivial write to prove the write pool is functional.
	_, err = d.ExecWrite(ctx, `
		INSERT INTO settings(key, value_json, updated_at) VALUES ('probe', '"ok"', 0)
	`)
	if err != nil {
		t.Fatalf("insert: %v", err)
	}

	// Re-migrate: must be a no-op (idempotent).
	if err := db.Migrate(ctx, d, migs, log); err != nil {
		t.Fatalf("re-migrate: %v", err)
	}

	tooNew := migs[len(migs)-1].Version + 1
	if _, err := d.Write.ExecContext(ctx, "PRAGMA user_version = "+strconv.Itoa(tooNew)); err != nil {
		t.Fatal(err)
	}
	if err := db.Migrate(ctx, d, migs, log); err == nil || !strings.Contains(err.Error(), "newer") {
		t.Fatalf("newer database must be rejected, got %v", err)
	}
}

func TestBeta1UpgradeToBeta2(t *testing.T) {
	ctx := context.Background()
	migs, err := db.LoadMigrations(migrations.FS, ".")
	if err != nil {
		t.Fatal(err)
	}
	// Beta.2 has one schema step after the published beta.1 baseline.
	if len(migs) < 2 || migs[0].Version != 1 || migs[1].Version != 2 {
		t.Fatalf("migration versions = %v, want beta.2 prefix [1 2]", migrationVersions(migs))
	}
	migs = migs[:2]
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))

	t.Run("upgrade", func(t *testing.T) {
		d, err := db.Open(ctx, filepath.Join(t.TempDir(), "upgrade.db"))
		if err != nil {
			t.Fatal(err)
		}
		defer d.Close()

		if err := db.Migrate(ctx, d, migs[:1], log); err != nil {
			t.Fatal(err)
		}
		assertSchemaVersion(t, d, 1)
		if _, err := d.ExecWrite(ctx, `
			INSERT INTO settings(key, value_json, updated_at)
			VALUES ('beta1-probe', '"preserved"', 1)`); err != nil {
			t.Fatal(err)
		}
		if _, err := d.ExecWrite(ctx, `
			INSERT INTO users(id, email, display_name, role, created_at, updated_at)
			VALUES (1, 'owner@example.com', 'Owner', 'admin', 0, 0);
			INSERT INTO jd_areas(code_start, code_end, name, position) VALUES (0, 9, 'System', 0);
			INSERT INTO jd_categories(id, area_start, code, name) VALUES (1, 0, 1, 'Inbox');
			INSERT INTO documents(id, owner_id, original_blob, original_size, jd_category_id, created_at, updated_at)
			VALUES (1, 1, 'legacy-review-document', 1, 1, 0, 0);
			INSERT INTO tags(id, name, slug, created_at, updated_at)
			VALUES (1, 'needs-review', 'needs-review', 0, 0);
			INSERT INTO document_tags(document_id, tag_id) VALUES (1, 1);
			INSERT INTO automations(id, name, preset_slug, created_at, updated_at)
			VALUES (1, 'Housing keywords', 'solo', 0, 0),
			       (2, 'Edited Housing keywords', NULL, 0, 0),
			       (3, 'Explicit preset regex', 'solo', 0, 0);
			INSERT INTO automation_triggers(automation_id, type, filter_content_re, created_at)
			SELECT id, 'document_added', 'lease|electricity bill', 0 FROM automations;
			INSERT INTO automation_actions(automation_id, kind, params_json, created_at)
			VALUES (1, 'assign_jd_category', '{"jd_category_id":1,"_preset_keywords":["lease","electricity bill"]}', 0),
			       (2, 'assign_jd_category', '{"jd_category_id":1,"_preset_keywords":["lease","electricity bill"]}', 0),
			       (3, 'assign_jd_category', '{"jd_category_id":1}', 0);
		`); err != nil {
			t.Fatal(err)
		}

		if err := db.Migrate(ctx, d, migs, log); err != nil {
			t.Fatal(err)
		}
		assertSchemaVersion(t, d, 2)
		assertBeta2Schema(t, d)
		// Already-version-2 development archives can safely apply only the repair.
		_, repair, ok := strings.Cut(migs[1].SQL, "-- Repair preset keyword matching.")
		if !ok {
			t.Fatal("missing standalone preset repair block")
		}
		for range 2 {
			if _, err := d.ExecWrite(ctx, "-- Repair preset keyword matching."+repair); err != nil {
				t.Fatal(err)
			}
		}
		for _, id := range []int{1, 2, 3} {
			var pattern string
			if err := d.Read.QueryRow(`SELECT filter_content_re FROM automation_triggers WHERE automation_id = ?`, id).Scan(&pattern); err != nil {
				t.Fatal(err)
			}
			re := regexp.MustCompile("(?i)" + pattern)
			if !re.MatchString("Your LEASE.") || !re.MatchString("electricity bill") {
				t.Fatalf("rule %d lost keyword matches: %q", id, pattern)
			}
			for _, text := range []string{"Please find attached", "leaseholder", "electricity billing", "élease", "lease租"} {
				if got := re.MatchString(text); got != (id != 1) {
					t.Fatalf("rule %d matched %q: %v", id, text, got)
				}
			}
			if id != 1 && pattern != "lease|electricity bill" {
				t.Fatalf("custom rule %d changed to %q", id, pattern)
			}
			if id == 1 && strings.Count(pattern, "(?:^|") != 1 {
				t.Fatalf("repair wrapped the pattern more than once: %q", pattern)
			}
		}
		var value string
		if err := d.Read.QueryRowContext(ctx,
			"SELECT value_json FROM settings WHERE key = 'beta1-probe'",
		).Scan(&value); err != nil {
			t.Fatal(err)
		}
		if value != `"preserved"` {
			t.Fatalf("preserved setting = %q", value)
		}
		var owned int
		if err := d.Read.QueryRowContext(ctx,
			`SELECT classifier_owned FROM document_tags WHERE document_id = 1 AND tag_id = 1`).Scan(&owned); err != nil {
			t.Fatal(err)
		}
		if owned != 0 {
			t.Fatalf("legacy tag classifier ownership = %d, want 0", owned)
		}
		if _, err := d.ExecWrite(ctx, `UPDATE document_tags SET classifier_owned = 2`); err == nil {
			t.Fatal("classifier ownership accepted a non-boolean value")
		}
	})

	t.Run("fresh install", func(t *testing.T) {
		d, err := db.Open(ctx, filepath.Join(t.TempDir(), "fresh.db"))
		if err != nil {
			t.Fatal(err)
		}
		defer d.Close()

		if err := db.Migrate(ctx, d, migs, log); err != nil {
			t.Fatal(err)
		}
		assertSchemaVersion(t, d, 2)
		assertBeta2Schema(t, d)
	})
}

func migrationVersions(migs []db.Migration) []int {
	versions := make([]int, len(migs))
	for i, migration := range migs {
		versions[i] = migration.Version
	}
	return versions
}

func assertSchemaVersion(t *testing.T, d *db.DB, want int) {
	t.Helper()
	var got int
	if err := d.Read.QueryRow("PRAGMA user_version").Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("user_version = %d, want %d", got, want)
	}
}

func assertBeta2Schema(t *testing.T, d *db.DB) {
	t.Helper()
	objects := []struct {
		typ  string
		name string
	}{
		{typ: "table", name: "document_intelligence"},
		{typ: "index", name: "idx_document_intelligence_review"},
		{typ: "index", name: "idx_document_intelligence_document"},
		{typ: "index", name: "documents_live_created"},
	}
	for _, object := range objects {
		var found int
		if err := d.Read.QueryRow(
			"SELECT count(*) FROM sqlite_schema WHERE type = ? AND name = ?",
			object.typ, object.name,
		).Scan(&found); err != nil {
			t.Fatal(err)
		}
		if found != 1 {
			t.Errorf("%s %q count = %d, want 1", object.typ, object.name, found)
		}
	}
}

func TestLiveDocumentListUsesCreatedIndex(t *testing.T) {
	ctx := context.Background()
	d, err := db.Open(ctx, filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	migs, err := db.LoadMigrations(migrations.FS, ".")
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	if err := db.Migrate(ctx, d, migs, log); err != nil {
		t.Fatal(err)
	}

	rows, err := d.Read.QueryContext(ctx, `
		EXPLAIN QUERY PLAN
		SELECT d.id FROM documents d
		WHERE d.trashed_at IS NULL
		ORDER BY d.created_at DESC, d.id DESC
		LIMIT 50`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var details strings.Builder
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		details.WriteString(detail)
		details.WriteByte('\n')
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	plan := details.String()
	if !strings.Contains(plan, "USING INDEX documents_live_created") ||
		strings.Contains(plan, "USE TEMP B-TREE") {
		t.Fatalf("default document page still sorts:\n%s", plan)
	}
}
