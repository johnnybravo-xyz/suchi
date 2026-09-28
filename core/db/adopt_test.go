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
	if len(beta) != 5 {
		t.Fatalf("compatibility migrations = %d, want 5", len(beta))
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

	t.Run("schema3 with extension boundary", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "suchi.db")
		d := openAdoptionDB(t, path)
		if err := db.Migrate(t.Context(), d, beta[:3], log); err != nil {
			t.Fatal(err)
		}
		set := db.MigrationSet{Component: "example", Migrations: []db.Migration{{
			Version: 1,
			Name:    "probe",
			SQL:     `CREATE TABLE extension_probe(id INTEGER PRIMARY KEY, value TEXT NOT NULL); INSERT INTO extension_probe VALUES(1,'preserved')`,
		}}}
		if err := db.MigrateSet(t.Context(), d, set, log); err != nil {
			t.Fatal(err)
		}

		if err := migrations.Prepare(t.Context(), d, log); err != nil {
			t.Fatal(err)
		}
		assertStableIdentity(t, d)
		assertMigrationScalar(t, d, `SELECT count(*) FROM extension_probe WHERE id=1 AND value='preserved'`, 1)
		assertMigrationScalar(t, d, `SELECT count(*) FROM _suchi_extension_migrations WHERE component='example' AND version=1`, 1)
	})
}

func TestStableAdoptionLegacyAgentBetaThree(t *testing.T) {
	beta, err := db.LoadMigrations(compatibility.FS, ".")
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	t.Run("empty legacy webhooks", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "suchi.db")
		d := openAdoptionDB(t, path)
		migrateLegacyAgentBetaThree(t, d, beta, log)
		if _, err := d.ExecWrite(t.Context(), `
			INSERT INTO automations(id,name,order_index,enabled,created_at,updated_at,system_id)
			VALUES(42,'Preserved',0,1,1,1,1);
			INSERT INTO automation_actions(id,automation_id,order_index,kind,params_json,created_at)
			VALUES(43,42,0,'discard','{}',1)`); err != nil {
			t.Fatal(err)
		}

		if err := migrations.Prepare(t.Context(), d, log); err != nil {
			t.Fatal(err)
		}
		assertStableIdentity(t, d)
		assertMigrationScalar(t, d, `SELECT count(*) FROM automation_actions WHERE id=43 AND automation_id=42 AND kind='discard'`, 1)
		assertMigrationScalar(t, d, `SELECT count(*) FROM sqlite_schema WHERE name='agent_webhooks'`, 0)
		if snapshots := adoptionSnapshots(t, path); len(snapshots) != 1 {
			t.Fatalf("snapshots = %v, want one", snapshots)
		}
	})

	t.Run("nonempty legacy webhooks", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "suchi.db")
		d := openAdoptionDB(t, path)
		migrateLegacyAgentBetaThree(t, d, beta, log)
		if _, err := d.ExecWrite(t.Context(), `
			INSERT INTO users(id,email,display_name,role,created_at,updated_at)
			VALUES(42,'preserved@example.test','Preserved','member',1,1);
			INSERT INTO agent_webhooks(id,owner_id,url,kind_prefix,secret_ciphertext,active,created_at)
			VALUES(1,42,'https://example.test/hook','agent:',X'01',1,1)`); err != nil {
			t.Fatal(err)
		}
		before := adoptionDBState(t, d)

		err := migrations.Prepare(t.Context(), d, log)
		if err == nil || !strings.Contains(err.Error(), "refusing to discard") {
			t.Fatalf("error = %v, want legacy webhook rejection", err)
		}
		if after := adoptionDBState(t, d); after != before {
			t.Fatalf("rejected database changed\nbefore: %s\nafter:  %s", before, after)
		}
		assertMigrationScalar(t, d, `SELECT count(*) FROM agent_webhooks WHERE id=1`, 1)
		if snapshots := adoptionSnapshots(t, path); len(snapshots) != 0 {
			t.Fatalf("rejected database created snapshots: %v", snapshots)
		}
	})
}

func migrateLegacyAgentBetaThree(t *testing.T, d *db.DB, beta []db.Migration, log *slog.Logger) {
	t.Helper()
	legacy := append([]db.Migration(nil), beta[:3]...)
	legacy[2].SQL = strings.Replace(legacy[2].SQL,
		"parent_id INTEGER REFERENCES tags(id) ON DELETE SET NULL,",
		"parent_id INTEGER REFERENCES tags(id) ON DELETE CASCADE,", 1)
	legacy[2].SQL = strings.Replace(legacy[2].SQL,
		"-- Beta.3 publishes this transition. Stable v1 retains the frozen SQL only for\n-- its guarded beta.2 compatibility path before adopting the squashed baseline.\n",
		"", 1)
	if err := db.Migrate(t.Context(), d, legacy, log); err != nil {
		t.Fatal(err)
	}
	rebuildLegacyAgentBetaThreeSchema(t, d)
}

func rebuildLegacyAgentBetaThreeSchema(t *testing.T, d *db.DB) {
	t.Helper()
	legacy := strings.ReplaceAll(`
		DROP TABLE automation_actions;
		CREATE TABLE automation_actions (
		  id             INTEGER PRIMARY KEY,
		  automation_id  INTEGER NOT NULL REFERENCES automations(id) ON DELETE CASCADE,
		  order_index  INTEGER NOT NULL DEFAULT 0,
		  -- assign_title | assign_tags | assign_correspondent | assign_document_type
		  -- | assign_jd_category | assign_storage_path | assign_owner | assign_custom_field
		  -- | create_agent_task | discard
		  -- | remove_tags | remove_correspondents | remove_document_type
		  -- | remove_storage_path | remove_custom_field
		  kind         TEXT NOT NULL,
		  -- Params live in a small JSON blob. Shape depends on kind:
		  --   assign_title            {"template": "{{correspondent}} — {{title}}"}
		  --   assign_tags             {"tag_ids": [1,2,3]}
		  --   assign_correspondent    {"correspondent_id": 5}
		  --   assign_document_type    {"document_type_id": 7}
		  --   assign_jd_category      {"jd_category_id": 8}
		  --   assign_storage_path     {"storage_path_id": 3}
		  --   assign_owner            {"owner_id": 2}
		  --   assign_custom_field     {"field_id": 4, "value": "..."}
		  --   create_agent_task       {"kind": "agent:workload", "payload": {}}
		  --   discard                 {}
		  --   remove_tags             {"tag_ids": [1,2]}
		  --   remove_correspondents   {"correspondent_ids": [5]}
		  --   remove_document_type    {}
		  --   remove_storage_path     {}
		  --   remove_custom_field     {"field_id": 4}
		  params_json  TEXT NOT NULL DEFAULT '{}',
		  created_at   INTEGER NOT NULL
		) STRICT;
		CREATE INDEX idx_automation_actions_aid  ON automation_actions(automation_id, order_index);

		CREATE TABLE agent_webhooks (
		    id                 INTEGER PRIMARY KEY,
		    owner_id           INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
		    url                TEXT NOT NULL,
		    kind_prefix        TEXT NOT NULL,     -- e.g. "agent:classify" or "agent:" for everything
		    secret_ciphertext  BLOB NOT NULL,     -- AEAD-sealed HMAC secret
		    label              TEXT,               -- optional operator-visible name
		    active             INTEGER NOT NULL DEFAULT 1 CHECK (active IN (0,1)),
		    created_at         INTEGER NOT NULL,
		    last_delivery_at   INTEGER,
		    last_status        INTEGER,            -- last HTTP status code from the receiver
		    last_error         TEXT                -- tail of the last error, if any
		) STRICT;
		CREATE INDEX agent_webhooks_owner_active
		    ON agent_webhooks(owner_id, active) WHERE active = 1`, "\n\t\t", "\n")
	if _, err := d.ExecWrite(t.Context(), legacy); err != nil {
		t.Fatal(err)
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
