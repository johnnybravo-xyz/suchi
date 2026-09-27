// SPDX-License-Identifier: AGPL-3.0-or-later

package db_test

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/johnnybravo-xyz/suchi/core/db"
	"github.com/johnnybravo-xyz/suchi/core/db/compatibility"
	"github.com/johnnybravo-xyz/suchi/core/db/migrations"
)

func TestStableCatalogAndBetaAdoption(t *testing.T) {
	stable, err := db.LoadMigrations(migrations.FS, ".")
	if err != nil {
		t.Fatal(err)
	}
	if len(stable) != 1 || stable[0].Version != db.StableSchemaVersion {
		t.Fatalf("active migrations = %+v, want only stable version 1", stable)
	}
	beta, err := db.LoadMigrations(compatibility.FS, ".")
	if err != nil {
		t.Fatal(err)
	}
	if len(beta) != 4 {
		t.Fatalf("compatibility migrations = %d, want 4", len(beta))
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	t.Run("empty and stable are idempotent", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "suchi.db")
		d := openAdoptionDB(t, path)
		for range 2 {
			if err := migrations.Prepare(t.Context(), d, log); err != nil {
				t.Fatal(err)
			}
		}
		assertStableIdentity(t, d)
		if snapshots := adoptionSnapshots(t, path); len(snapshots) != 0 {
			t.Fatalf("fresh stable database created snapshots: %v", snapshots)
		}
	})

	for sourceVersion := 1; sourceVersion <= 4; sourceVersion++ {
		t.Run(fmt.Sprintf("schema%d", sourceVersion), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "suchi.db")
			d := openAdoptionDB(t, path)
			if err := db.Migrate(t.Context(), d, beta[:sourceVersion], log); err != nil {
				t.Fatal(err)
			}
			if _, err := d.ExecWrite(t.Context(), `
				INSERT INTO users(id,email,display_name,role,created_at,updated_at)
				VALUES(42,'preserved@example.test','Preserved','member',1,1);
				INSERT INTO settings(key,value_json,updated_at) VALUES('adoption-probe','"preserved"',1)`); err != nil {
				t.Fatal(err)
			}

			if err := migrations.Prepare(t.Context(), d, log); err != nil {
				t.Fatal(err)
			}
			assertStableIdentity(t, d)
			assertMigrationScalar(t, d, `SELECT count(*) FROM users WHERE id=42 AND email='preserved@example.test'`, 1)
			assertMigrationScalar(t, d, `SELECT count(*) FROM settings WHERE key='adoption-probe' AND value_json='"preserved"'`, 1)

			snapshots := adoptionSnapshots(t, path)
			if len(snapshots) != 1 {
				t.Fatalf("snapshots = %v, want one", snapshots)
			}
			info, err := os.Stat(snapshots[0])
			if err != nil {
				t.Fatal(err)
			}
			if info.Mode().Perm() != 0o600 {
				t.Fatalf("snapshot mode = %o, want 600", info.Mode().Perm())
			}
			snapshot := openAdoptionDB(t, snapshots[0])
			assertSchemaVersion(t, snapshot, sourceVersion)
			assertMigrationScalar(t, snapshot, `SELECT count(*) FROM users WHERE id=42`, 1)
			if err := snapshot.Close(); err != nil {
				t.Fatal(err)
			}

			if err := migrations.Prepare(t.Context(), d, log); err != nil {
				t.Fatalf("repeat stable preparation: %v", err)
			}
			if got := len(adoptionSnapshots(t, path)); got != 1 {
				t.Fatalf("repeat preparation created %d snapshots, want 1", got)
			}
		})
	}
}

func TestStableAdoptionRejectsUnknownSchemasWithoutWrites(t *testing.T) {
	beta, err := db.LoadMigrations(compatibility.FS, ".")
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	tests := []struct {
		name  string
		setup func(*testing.T, *db.DB)
	}{
		{name: "unknown", setup: func(t *testing.T, d *db.DB) {
			_, err := d.ExecWrite(t.Context(), `CREATE TABLE unexpected(id INTEGER PRIMARY KEY)`)
			if err != nil {
				t.Fatal(err)
			}
		}},
		{name: "spoofed lineage", setup: func(t *testing.T, d *db.DB) {
			if err := migrations.Prepare(t.Context(), d, log); err != nil {
				t.Fatal(err)
			}
			if _, err := d.ExecWrite(t.Context(), `UPDATE schema_lineage SET name='final-beta-schema-3'`); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "partial beta", setup: func(t *testing.T, d *db.DB) {
			if err := db.Migrate(t.Context(), d, beta[:2], log); err != nil {
				t.Fatal(err)
			}
			if _, err := d.ExecWrite(t.Context(), `DROP INDEX idx_document_intelligence_document`); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "future", setup: func(t *testing.T, d *db.DB) {
			if err := migrations.Prepare(t.Context(), d, log); err != nil {
				t.Fatal(err)
			}
			if _, err := d.Write.ExecContext(t.Context(), `PRAGMA user_version=99`); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "suchi.db")
			d := openAdoptionDB(t, path)
			test.setup(t, d)
			before := adoptionDBState(t, d)
			if err := migrations.Prepare(t.Context(), d, log); err == nil || !strings.Contains(err.Error(), "unsupported core schema") {
				t.Fatalf("error = %v, want unsupported schema", err)
			}
			if after := adoptionDBState(t, d); after != before {
				t.Fatalf("rejected database changed\nbefore: %s\nafter:  %s", before, after)
			}
			if snapshots := adoptionSnapshots(t, path); len(snapshots) != 0 {
				t.Fatalf("rejected database created snapshots: %v", snapshots)
			}
		})
	}
}

func TestStableAdoptionSnapshotFailureAndRollback(t *testing.T) {
	stable, err := db.LoadMigrations(migrations.FS, ".")
	if err != nil {
		t.Fatal(err)
	}
	beta, err := db.LoadMigrations(compatibility.FS, ".")
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	t.Run("snapshot failure", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "suchi.db")
		d := openAdoptionDB(t, path)
		if err := db.Migrate(t.Context(), d, beta[:2], log); err != nil {
			t.Fatal(err)
		}
		before := adoptionDBState(t, d)
		d.Path = filepath.Join(filepath.Dir(path), "missing", "suchi.db")
		if err := db.PrepareStable(t.Context(), d, stable, beta, log); err == nil || !strings.Contains(err.Error(), "pre-adoption snapshot") {
			t.Fatalf("error = %v, want snapshot failure", err)
		}
		if after := adoptionDBState(t, d); after != before {
			t.Fatalf("snapshot failure changed database\nbefore: %s\nafter:  %s", before, after)
		}
	})

	t.Run("migration rollback", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "suchi.db")
		d := openAdoptionDB(t, path)
		if err := db.Migrate(t.Context(), d, beta[:2], log); err != nil {
			t.Fatal(err)
		}
		if _, err := d.ExecWrite(t.Context(), `INSERT INTO settings(key,value_json,updated_at) VALUES('rollback-probe','true',1)`); err != nil {
			t.Fatal(err)
		}
		before := adoptionDBState(t, d)
		broken := append([]db.Migration(nil), beta...)
		broken[2].SQL += `
INSERT INTO missing_adoption_table VALUES(1);`
		if err := db.PrepareStable(t.Context(), d, stable, broken, log); err == nil || !strings.Contains(err.Error(), "missing_adoption_table") {
			t.Fatalf("error = %v, want injected migration failure", err)
		}
		if after := adoptionDBState(t, d); after != before {
			t.Fatalf("failed adoption did not roll back\nbefore: %s\nafter:  %s", before, after)
		}
		if got := len(adoptionSnapshots(t, path)); got != 1 {
			t.Fatalf("retained snapshots = %d, want 1", got)
		}
		var foreignKeys int
		if err := d.Write.QueryRowContext(t.Context(), `PRAGMA foreign_keys`).Scan(&foreignKeys); err != nil || foreignKeys != 1 {
			t.Fatalf("writer foreign_keys=%d err=%v, want 1", foreignKeys, err)
		}
	})
}

func openAdoptionDB(t *testing.T, path string) *db.DB {
	t.Helper()
	d, err := db.Open(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	return d
}

func assertStableIdentity(t *testing.T, d *db.DB) {
	t.Helper()
	assertSchemaVersion(t, d, db.StableSchemaVersion)
	assertMigrationScalar(t, d, `SELECT count(*) FROM schema_lineage WHERE singleton=1 AND name='stable-v1'`, 1)
	assertMigrationScalar(t, d, `SELECT count(*) FROM pragma_foreign_key_check`, 0)
}

func adoptionSnapshots(t *testing.T, databasePath string) []string {
	t.Helper()
	paths, err := filepath.Glob(databasePath + ".pre-stable-v1.*.sqlite")
	if err != nil {
		t.Fatal(err)
	}
	return paths
}

func adoptionDBState(t *testing.T, d *db.DB) string {
	t.Helper()
	var version int
	if err := d.Read.QueryRowContext(t.Context(), `PRAGMA user_version`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	rows, err := d.Read.QueryContext(t.Context(), `SELECT type,name,tbl_name,COALESCE(sql,'') FROM sqlite_schema ORDER BY type,name,tbl_name`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var state strings.Builder
	fmt.Fprintf(&state, "version=%d\n", version)
	for rows.Next() {
		var typ, name, table, sql string
		if err := rows.Scan(&typ, &name, &table, &sql); err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(&state, "%s\x00%s\x00%s\x00%s\n", typ, name, table, sql)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return state.String()
}
