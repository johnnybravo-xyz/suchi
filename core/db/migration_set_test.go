package db_test

import (
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"

	"github.com/johnnybravo-xyz/suchi/core/db"
	"github.com/johnnybravo-xyz/suchi/core/db/migrations"
)

func openSetDB(t *testing.T) (*db.DB, *slog.Logger) {
	t.Helper()
	d, err := db.Open(t.Context(), filepath.Join(t.TempDir(), "sets.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	return d, slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestMigrationSetsIndependentIdempotentAndCoreUpgrade(t *testing.T) {
	d, log := openSetDB(t)
	core := []db.Migration{{Version: 1, SQL: `CREATE TABLE public_state(value INTEGER); INSERT INTO public_state VALUES(1)`}}
	if err := db.Migrate(t.Context(), d, core, log); err != nil {
		t.Fatal(err)
	}
	sets := []db.MigrationSet{
		{Component: "alpha", Migrations: []db.Migration{{Version: 1, SQL: `CREATE TABLE alpha(value INTEGER); INSERT INTO alpha VALUES(1)`}, {Version: 2, SQL: `INSERT INTO alpha VALUES(2)`}, {Version: 3, SQL: `INSERT INTO alpha VALUES(3)`}}},
		{Component: "beta", Migrations: []db.Migration{{Version: 1, SQL: `CREATE TABLE beta(value INTEGER); INSERT INTO beta VALUES(1)`}}},
	}
	for range 2 {
		for _, set := range sets {
			if err := db.MigrateSet(t.Context(), d, set, log); err != nil {
				t.Fatal(err)
			}
		}
	}
	assertSchemaVersion(t, d, 1)
	assertMigrationScalar(t, d, `SELECT count(*) FROM alpha`, 3)
	assertMigrationScalar(t, d, `SELECT count(*) FROM beta`, 1)
	assertMigrationScalar(t, d, `SELECT count(*) FROM _suchi_extension_migrations WHERE length(checksum)=64 AND applied_at>0`, 4)
	core = append(core, db.Migration{Version: 2, SQL: `INSERT INTO public_state VALUES(2)`})
	if err := db.Migrate(t.Context(), d, core, log); err != nil {
		t.Fatal(err)
	}
	assertSchemaVersion(t, d, 2)
	assertMigrationScalar(t, d, `SELECT count(*) FROM public_state`, 2)
	// Re-running preserves the applied record itself, not just its count.
	execMigrationFixture(t, d, `UPDATE _suchi_extension_migrations SET applied_at=1`)
	for _, set := range sets {
		if err := db.MigrateSet(t.Context(), d, set, log); err != nil {
			t.Fatal(err)
		}
	}
	assertMigrationScalar(t, d, `SELECT max(applied_at) FROM _suchi_extension_migrations`, 1)
}

func TestMigrationSetRollback(t *testing.T) {
	for _, rebuild := range []bool{false, true} {
		t.Run(map[bool]string{false: "normal", true: "rebuild"}[rebuild], func(t *testing.T) {
			d, log := openSetDB(t)
			set := db.MigrationSet{Component: "example", Migrations: []db.Migration{{Version: 1, SQL: `CREATE TABLE parent(id INTEGER PRIMARY KEY); INSERT INTO parent VALUES(1); CREATE TABLE child(parent_id INTEGER REFERENCES parent(id)); INSERT INTO child VALUES(1)`}}}
			if err := db.MigrateSet(t.Context(), d, set, log); err != nil {
				t.Fatal(err)
			}
			set.Migrations = append(set.Migrations, db.Migration{Version: 2, RebuildTables: rebuild, SQL: `CREATE TABLE pending(id INTEGER); INSERT INTO parent VALUES(2); INSERT INTO child VALUES(999)`})
			if err := db.MigrateSet(t.Context(), d, set, log); err == nil {
				t.Fatal("expected foreign key failure")
			}
			assertMigrationScalar(t, d, `SELECT count(*) FROM sqlite_schema WHERE name='pending'`, 0)
			assertMigrationScalar(t, d, `SELECT count(*) FROM parent`, 1)
			assertMigrationScalar(t, d, `SELECT count(*) FROM _suchi_extension_migrations`, 1)
			var enabled int
			if err := d.Write.QueryRowContext(t.Context(), `PRAGMA foreign_keys`).Scan(&enabled); err != nil || enabled != 1 {
				t.Fatalf("writer foreign keys=%d err=%v", enabled, err)
			}
			// Retry a corrected pending version; DDL and ledger commit together.
			set.Migrations[1].SQL = `CREATE TABLE pending(id INTEGER); INSERT INTO parent VALUES(2)`
			if err := db.MigrateSet(t.Context(), d, set, log); err != nil {
				t.Fatal(err)
			}
			assertMigrationScalar(t, d, `SELECT count(*) FROM sqlite_schema WHERE name='pending'`, 1)
			assertMigrationScalar(t, d, `SELECT count(*) FROM _suchi_extension_migrations`, 2)
		})
	}
}

func TestMigrationSetRejectsDriftAndMissingAppliedHistory(t *testing.T) {
	for _, change := range []string{"sql", "mode", "missing"} {
		t.Run(change, func(t *testing.T) {
			d, log := openSetDB(t)
			set := db.MigrationSet{Component: "example", Migrations: []db.Migration{{Version: 1, SQL: `CREATE TABLE example(id INTEGER)`}, {Version: 2, SQL: `INSERT INTO example VALUES(1)`}}}
			if err := db.MigrateSet(t.Context(), d, set, log); err != nil {
				t.Fatal(err)
			}
			want := "checksum mismatch"
			switch change {
			case "sql":
				set.Migrations[0].SQL += "; SELECT 1"
			case "mode":
				set.Migrations[0].RebuildTables = true
			case "missing":
				set.Migrations = set.Migrations[:1]
				want = "missing"
			}
			if change != "missing" {
				set.Migrations = append(set.Migrations, db.Migration{Version: 3, SQL: `INSERT INTO example VALUES(3)`})
			}
			err := db.MigrateSet(t.Context(), d, set, log)
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("error=%v, want %s", err, want)
			}
			assertMigrationScalar(t, d, `SELECT count(*) FROM example`, 1)
		})
	}
}

func TestMigrationSetRejectsDefinitionsBeforeChanges(t *testing.T) {
	valid := db.Migration{Version: 1, SQL: `CREATE TABLE must_not_exist(id INTEGER)`}
	for _, tc := range []struct {
		name, component string
		migs            []db.Migration
	}{
		{"empty component", "", []db.Migration{valid}},
		{"invalid component", "a'; DROP TABLE users; --", []db.Migration{valid}},
		{"reserved", "core", []db.Migration{valid}},
		{"no versions", "example", nil},
		{"missing first", "example", []db.Migration{{Version: 2, SQL: "SELECT 1"}}},
		{"zero", "example", []db.Migration{{SQL: "SELECT 1"}}},
		{"duplicate", "example", []db.Migration{valid, valid}},
		{"gap", "example", []db.Migration{valid, {Version: 3, SQL: "SELECT 1"}}},
		{"out of order", "example", []db.Migration{{Version: 2, SQL: "SELECT 1"}, valid}},
		{"empty SQL", "example", []db.Migration{valid, {Version: 2, SQL: "  \n"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, log := openSetDB(t)
			if err := db.MigrateSet(t.Context(), d, db.MigrationSet{Component: tc.component, Migrations: tc.migs}, log); err == nil {
				t.Fatal("invalid set accepted")
			}
			assertMigrationScalar(t, d, `SELECT count(*) FROM sqlite_schema WHERE type='table'`, 0)
		})
	}
}

func TestPublishedCoreOnlyDatabaseUpgradesWithMigrationSet(t *testing.T) {
	d, log := openSetDB(t)
	core, err := db.LoadMigrations(migrations.FS, ".")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Migrate(t.Context(), d, core[:2], log); err != nil {
		t.Fatal(err)
	}
	assertMigrationScalar(t, d, `SELECT count(*) FROM sqlite_schema WHERE name='_suchi_extension_migrations'`, 0)
	if err := db.Migrate(t.Context(), d, core, log); err != nil {
		t.Fatal(err)
	}
	if err := db.MigrateSet(t.Context(), d, db.MigrationSet{Component: "example", Migrations: []db.Migration{{Version: 1, SQL: `CREATE TABLE example(system_id INTEGER REFERENCES jd_systems(id))`}}}, log); err != nil {
		t.Fatal(err)
	}
	assertSchemaVersion(t, d, core[len(core)-1].Version)
}
