// SPDX-License-Identifier: AGPL-3.0-or-later

package db_test

import (
	"database/sql"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/johnnybravo-xyz/suchi/core/db"
	"github.com/johnnybravo-xyz/suchi/core/db/compatibility"
	"github.com/johnnybravo-xyz/suchi/core/db/migrations"
)

func TestAccountIdentityMigrationPreservesBetaRelationships(t *testing.T) {
	beta, err := db.LoadMigrations(compatibility.FS, ".")
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	for _, sourceVersion := range []int{2, 3} {
		t.Run(fmt.Sprintf("beta%d", sourceVersion), func(t *testing.T) {
			d := openAdoptionDB(t, filepath.Join(t.TempDir(), "suchi.db"))
			if err := db.Migrate(t.Context(), d, beta[:2], log); err != nil {
				t.Fatal(err)
			}
			seedMobileMigrationOwnerAndCategory(t, d)
			if _, err := d.ExecWrite(t.Context(), `
				INSERT INTO users(id,email,display_name,role,created_at,updated_at) VALUES
					(42,'owner@example.test','Owner','admin',1,1),
					(43,'reviewer@example.test','Reviewer','member',1,1);
				INSERT INTO documents(id,owner_id,original_blob,original_size,title,jd_category_id,created_at,updated_at)
					VALUES(100,42,'identity-migration',1,'Preserved',1,1,1);
				INSERT INTO sessions(id,user_id,created_at,expires_at,last_seen_at)
					VALUES('reviewer-session',43,1,9999999999,1);
				INSERT INTO document_intelligence(
					id,document_id,intelligence_type,value_json,evidence_text,confidence,
					extractor,extraction_version,reviewed_by,created_at,updated_at
				) VALUES(77,100,'date','"2026-01-01"','source',0.9,'test',1,43,1,1);
				INSERT INTO audit_events(id,ts,actor_kind,actor_id,action,object_kind,object_id)
					VALUES(55,1,'user',42,'document.update','document',100);
			`); err != nil {
				t.Fatal(err)
			}
			if sourceVersion > 2 {
				if err := db.Migrate(t.Context(), d, beta[:sourceVersion], log); err != nil {
					t.Fatal(err)
				}
			}

			if err := migrations.Prepare(t.Context(), d, log); err != nil {
				t.Fatal(err)
			}
			assertStableIdentity(t, d)
			assertMigrationScalar(t, d, `SELECT count(*) FROM users WHERE id=42 AND oidc_issuer IS NULL AND oidc_subject IS NULL`, 1)
			assertMigrationScalar(t, d, `SELECT count(*) FROM users WHERE id IN (42,43) AND dev_seeded=0`, 2)
			assertMigrationScalar(t, d, `
				SELECT count(*)
				FROM pragma_table_info('users')
				WHERE name='dev_seeded' AND "notnull"=1 AND dflt_value='0'`, 1)
			if _, err := d.ExecWrite(t.Context(), `UPDATE users SET dev_seeded=2 WHERE id=42`); err == nil {
				t.Fatal("invalid development-account marker accepted")
			}
			assertMigrationScalar(t, d, `SELECT count(*) FROM documents WHERE id=100 AND owner_id=42`, 1)
			assertMigrationScalar(t, d, `SELECT count(*) FROM sessions WHERE id='reviewer-session' AND user_id=43`, 1)
			assertMigrationScalar(t, d, `SELECT count(*) FROM document_intelligence WHERE id=77 AND reviewed_by=43`, 1)
			assertMigrationScalar(t, d, `SELECT count(*) FROM audit_events WHERE id=55 AND actor_id=42 AND retained=0`, 1)
			assertMigrationScalar(t, d, `SELECT count(*) FROM pragma_integrity_check WHERE integrity_check='ok'`, 1)

			if _, err := d.ExecWrite(t.Context(), `DELETE FROM users WHERE id=42`); err == nil {
				t.Fatal("document owner NO ACTION reference allowed deletion")
			}
			if _, err := d.ExecWrite(t.Context(), `DELETE FROM users WHERE id=43`); err != nil {
				t.Fatal(err)
			}
			assertMigrationScalar(t, d, `SELECT count(*) FROM sessions WHERE user_id=43`, 0)
			var reviewer sql.NullInt64
			if err := d.Read.QueryRow(`SELECT reviewed_by FROM document_intelligence WHERE id=77`).Scan(&reviewer); err != nil || reviewer.Valid {
				t.Fatalf("reviewed_by after user deletion = %v, err=%v", reviewer, err)
			}
			assertMigrationScalar(t, d, `SELECT count(*) FROM audit_events WHERE id=55 AND actor_id=42`, 1)

			if _, err := d.ExecWrite(t.Context(), `UPDATE users SET oidc_issuer='https://issuer.example', oidc_subject='Opaque Subject' WHERE id=42`); err != nil {
				t.Fatal(err)
			}
			for _, invalid := range []string{
				`UPDATE users SET oidc_subject=NULL WHERE id=42`,
				`UPDATE users SET oidc_issuer='', oidc_subject='' WHERE id=42`,
			} {
				if _, err := d.ExecWrite(t.Context(), invalid); err == nil {
					t.Fatalf("invalid OIDC identity accepted: %s", invalid)
				}
			}
			if _, err := d.ExecWrite(t.Context(), `
				INSERT INTO users(id,email,display_name,role,created_at,updated_at,oidc_issuer,oidc_subject)
				VALUES(44,'collision@example.test','Collision','admin',1,1,'https://issuer.example','Opaque Subject')`); err == nil {
				t.Fatal("duplicate issuer/subject identity accepted")
			}
			if _, err := d.ExecWrite(t.Context(), `INSERT INTO audit_events(id,ts,actor_kind,action,object_kind) VALUES(56,1,'system','probe','server')`); err != nil {
				t.Fatal(err)
			}
			assertMigrationScalar(t, d, `SELECT retained FROM audit_events WHERE id=56`, 0)
			if _, err := d.ExecWrite(t.Context(), `UPDATE audit_events SET retained=2 WHERE id=56`); err == nil {
				t.Fatal("invalid retained flag accepted")
			}
		})
	}
}

func TestAccountIdentityFreshAndAdoptedSchemasMatch(t *testing.T) {
	beta, err := db.LoadMigrations(compatibility.FS, ".")
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	fresh := openAdoptionDB(t, filepath.Join(t.TempDir(), "fresh.db"))
	if err := migrations.Prepare(t.Context(), fresh, log); err != nil {
		t.Fatal(err)
	}
	adopted := openAdoptionDB(t, filepath.Join(t.TempDir(), "adopted.db"))
	if err := db.Migrate(t.Context(), adopted, beta[:4], log); err != nil {
		t.Fatal(err)
	}
	if err := migrations.Prepare(t.Context(), adopted, log); err != nil {
		t.Fatal(err)
	}

	if got, want := coreSchemaDDL(t, adopted), coreSchemaDDL(t, fresh); !reflect.DeepEqual(got, want) {
		t.Fatalf("adopted schema differs from fresh baseline\nadopted: %v\nfresh:   %v", got, want)
	}

	assertMigrationScalar(t, fresh, `
		SELECT count(*)
		FROM pragma_table_info('documents')
		WHERE name='correspondent_id'`, 0)
	assertMigrationScalar(t, fresh, `
		SELECT count(*)
		FROM sqlite_schema
		WHERE name IN (
			'documents_correspondent',
			'documents_correspondent_id_insert',
			'documents_correspondent_id_update',
			'documents_correspondent_revision',
			'plugin_kv',
			'object_acls_lookup'
		)`, 0)
	objects := map[string]string{
		"audit_actor":                             "index",
		"audit_events":                            "table",
		"render_moves":                            "table",
		"schema_lineage":                          "table",
		"audit_events_system_immutable":           "trigger",
		"audit_events_system_replace":             "trigger",
		"audit_object":                            "index",
		"audit_system":                            "index",
		"audit_ts":                                "index",
		"users":                                   "table",
		"users_default_system_demotion":           "trigger",
		"users_default_system_insert":             "trigger",
		"users_oidc_identity":                     "index",
		"document_correspondents_revision_delete": "trigger",
		"document_correspondents_revision_insert": "trigger",
		"document_correspondents_revision_update": "trigger",
	}
	for name, typ := range objects {
		var count int
		if err := adopted.Read.QueryRow(`SELECT count(*) FROM sqlite_schema WHERE name=? AND type=?`, name, typ).Scan(&count); err != nil || count != 1 {
			t.Fatalf("%s %s count=%d err=%v", typ, name, count, err)
		}
	}
}

func TestAccountIdentityRejectsOldStableLineage(t *testing.T) {
	beta, err := db.LoadMigrations(compatibility.FS, ".")
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	path := filepath.Join(t.TempDir(), "old-stable.db")
	d := openAdoptionDB(t, path)
	if err := db.Migrate(t.Context(), d, beta[:4], log); err != nil {
		t.Fatal(err)
	}
	if _, err := d.ExecWrite(t.Context(), `UPDATE schema_lineage SET name='stable-v1'; PRAGMA user_version=1`); err != nil {
		t.Fatal(err)
	}
	before := adoptionDBState(t, d)
	if err := migrations.Prepare(t.Context(), d, log); err == nil || !strings.Contains(err.Error(), "unsupported core schema") {
		t.Fatalf("old stable-v1 schema error=%v, want fail-closed rejection", err)
	}
	if after := adoptionDBState(t, d); after != before {
		t.Fatal("rejected old stable-v1 schema changed")
	}
	if snapshots := adoptionSnapshots(t, path); len(snapshots) != 0 {
		t.Fatalf("rejected old stable-v1 schema created snapshots: %v", snapshots)
	}
}

func TestAccountIdentityMigrationRollsBack(t *testing.T) {
	stable, err := db.LoadMigrations(migrations.FS, ".")
	if err != nil {
		t.Fatal(err)
	}
	beta, err := db.LoadMigrations(compatibility.FS, ".")
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	path := filepath.Join(t.TempDir(), "rollback.db")
	d := openAdoptionDB(t, path)
	if err := db.Migrate(t.Context(), d, beta[:4], log); err != nil {
		t.Fatal(err)
	}
	if _, err := d.ExecWrite(t.Context(), `
		INSERT INTO users(id,email,display_name,role,created_at,updated_at)
		VALUES(42,'preserved@example.test','Preserved','admin',1,1);
		INSERT INTO audit_events(id,ts,actor_kind,action,object_kind)
		VALUES(55,1,'system','preserved','server')`); err != nil {
		t.Fatal(err)
	}
	before := adoptionDBState(t, d)
	broken := append([]db.Migration(nil), beta...)
	broken[4].SQL += "\nINSERT INTO missing_identity_table VALUES(1);"
	if err := db.PrepareStable(t.Context(), d, stable, broken, log); err == nil || !strings.Contains(err.Error(), "missing_identity_table") {
		t.Fatalf("migration failure=%v, want injected account-identity failure", err)
	}
	if after := adoptionDBState(t, d); after != before {
		t.Fatal("failed account-identity migration changed schema")
	}
	assertMigrationScalar(t, d, `SELECT count(*) FROM users WHERE id=42 AND email='preserved@example.test'`, 1)
	assertMigrationScalar(t, d, `SELECT count(*) FROM audit_events WHERE id=55 AND action='preserved'`, 1)
	assertMigrationScalar(t, d, `SELECT count(*) FROM pragma_table_info('users') WHERE name IN ('oidc_issuer','oidc_subject')`, 0)
	if snapshots := adoptionSnapshots(t, path); len(snapshots) != 1 {
		t.Fatalf("retained snapshots=%v, want one", snapshots)
	}
}

func TestAccountIdentityMigratesWatchedFolderOwner(t *testing.T) {
	tests := []struct {
		name       string
		legacyJSON string
		ambiguous  bool
		conflict   bool
		wantID     string
		wantError  bool
	}{
		{name: "absent"},
		{name: "blank", legacyJSON: `"   "`},
		{name: "valid case insensitive", legacyJSON: `"  OWNER@EXAMPLE.TEST  "`, wantID: "42"},
		{name: "malformed JSON", legacyJSON: `not-json`, wantError: true},
		{name: "non-string JSON", legacyJSON: `42`, wantError: true},
		{name: "unresolved", legacyJSON: `"missing@example.test"`, wantError: true},
		{name: "ambiguous", legacyJSON: `"owner@example.test"`, ambiguous: true, wantError: true},
		{name: "conflicting new key", legacyJSON: `"owner@example.test"`, conflict: true, wantError: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			d, beta, log := openBetaFour(t)
			if _, err := d.ExecWrite(t.Context(), `
				INSERT INTO users(id,email,display_name,role,created_at,updated_at)
				VALUES(42,'owner@example.test','Owner','admin',1,1)`); err != nil {
				t.Fatal(err)
			}
			if test.ambiguous {
				if _, err := d.ExecWrite(t.Context(), `
					INSERT INTO users(id,email,display_name,role,created_at,updated_at)
					VALUES(43,'OWNER@EXAMPLE.TEST','Other owner','admin',1,1)`); err != nil {
					t.Fatal(err)
				}
			}
			if test.legacyJSON != "" {
				if _, err := d.ExecWrite(t.Context(), `
					INSERT INTO settings(key,value_json,updated_at)
					VALUES('ingest.fs_watch_owner',?,77)`, test.legacyJSON); err != nil {
					t.Fatal(err)
				}
			}
			if test.conflict {
				if _, err := d.ExecWrite(t.Context(), `
					INSERT INTO settings(key,value_json,updated_at)
					VALUES('ingest.fs_watch_owner_id','99',88)`); err != nil {
					t.Fatal(err)
				}
			}

			before := migrationSettingsState(t, d)
			err := db.Migrate(t.Context(), d, beta, log)
			if test.wantError {
				if err == nil {
					t.Fatal("migration succeeded, want watched-owner rejection")
				}
				assertSchemaVersion(t, d, 4)
				if after := migrationSettingsState(t, d); after != before {
					t.Fatalf("failed migration changed settings\nbefore: %s\nafter:  %s", before, after)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			assertMigrationScalar(t, d, `
				SELECT count(*) FROM settings
				WHERE key='ingest.fs_watch_owner'`, 0)
			if test.wantID == "" {
				assertMigrationScalar(t, d, `
					SELECT count(*) FROM settings
					WHERE key='ingest.fs_watch_owner_id'`, 0)
			} else {
				assertMigrationScalar(t, d, `
					SELECT count(*) FROM settings
					WHERE key='ingest.fs_watch_owner_id'
					  AND value_json='42' AND updated_at=77`, 1)
			}
		})
	}
}

func migrationSettingsState(t *testing.T, d *db.DB) string {
	t.Helper()
	var state string
	if err := d.Read.QueryRow(`
		SELECT COALESCE(group_concat(key || '=' || quote(value_json) || '@' || updated_at, '|'), '')
		FROM (SELECT key,value_json,updated_at FROM settings ORDER BY key)`).Scan(&state); err != nil {
		t.Fatal(err)
	}
	return state
}

func openBetaFour(t *testing.T) (*db.DB, []db.Migration, *slog.Logger) {
	t.Helper()
	beta, err := db.LoadMigrations(compatibility.FS, ".")
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	d := openAdoptionDB(t, filepath.Join(t.TempDir(), "beta4.db"))
	if err := db.Migrate(t.Context(), d, beta[:4], log); err != nil {
		t.Fatal(err)
	}
	return d, beta, log
}

func TestAccountIdentityMigratesCorrespondentRelationships(t *testing.T) {
	d, beta, log := openBetaFour(t)
	seedBetaFourOwnerAndCategory(t, d)
	if _, err := d.ExecWrite(t.Context(), `
		INSERT INTO correspondents(id,name,slug,created_at,updated_at,system_id) VALUES
			(10,'Legacy sender','legacy-sender',1,1,1),
			(11,'Other sender','other-sender',1,1,1),
			(12,'Recipient','recipient',1,1,1),
			(13,'Later sender','later-sender',1,1,1);
		INSERT INTO documents(
			id,owner_id,original_blob,original_size,title,jd_category_id,
			created_at,updated_at,correspondent_id,system_id
		) VALUES
			(100,42,'doc-100',1,'100',1,1,1,10,1),
			(101,42,'doc-101',1,'101',1,1,1,10,1),
			(102,42,'doc-102',1,'102',1,1,1,NULL,1),
			(103,42,'doc-103',1,'103',1,1,1,10,1),
			(104,42,'doc-104',1,'104',1,1,1,10,1),
			(105,42,'doc-105',1,'105',1,1,1,NULL,1);
		INSERT INTO document_correspondents(document_id,correspondent_id,role,position) VALUES
			(101,10,'sender',5),
			(101,11,'sender',0),
			(101,12,'recipient',7),
			(102,11,'sender',5),
			(102,10,'sender',5),
			(103,11,'sender',3),
			(104,10,'recipient',4);
		UPDATE documents
		SET correspondent_revision = CASE id
			WHEN 100 THEN 7
			WHEN 101 THEN 8
			WHEN 102 THEN 9
			WHEN 103 THEN 10
			WHEN 104 THEN 11
			WHEN 105 THEN 12
		END
		WHERE id BETWEEN 100 AND 105`); err != nil {
		t.Fatal(err)
	}

	if err := db.Migrate(t.Context(), d, beta, log); err != nil {
		t.Fatal(err)
	}
	var relations string
	if err := d.Read.QueryRow(`
		SELECT group_concat(document_id || ':' || role || ':' || correspondent_id || ':' || position, '|')
		FROM (
			SELECT document_id,role,correspondent_id,position
			FROM document_correspondents
			ORDER BY document_id,role,position,correspondent_id
		)`).Scan(&relations); err != nil {
		t.Fatal(err)
	}
	const wantRelations = "100:sender:10:0|" +
		"101:recipient:12:7|101:sender:10:0|101:sender:11:1|" +
		"102:sender:10:0|102:sender:11:1|" +
		"103:sender:10:0|103:sender:11:1|" +
		"104:recipient:10:4|104:sender:10:0"
	if relations != wantRelations {
		t.Fatalf("migrated correspondent relationships = %q, want %q", relations, wantRelations)
	}
	var revisions string
	if err := d.Read.QueryRow(`
		SELECT group_concat(id || ':' || correspondent_revision, '|')
		FROM (SELECT id,correspondent_revision FROM documents ORDER BY id)`).Scan(&revisions); err != nil {
		t.Fatal(err)
	}
	if revisions != "100:7|101:8|102:9|103:10|104:11|105:12" {
		t.Fatalf("correspondent revisions = %q", revisions)
	}
	assertMigrationScalar(t, d, `
		SELECT count(*) FROM pragma_table_info('documents')
		WHERE name='correspondent_id'`, 0)
	assertMigrationScalar(t, d, `
		SELECT count(*) FROM sqlite_schema
		WHERE name IN (
			'documents_correspondent',
			'documents_correspondent_id_insert',
			'documents_correspondent_id_update',
			'documents_correspondent_revision'
		)`, 0)

	if _, err := d.ExecWrite(t.Context(), `
		INSERT INTO document_correspondents(document_id,correspondent_id,role,position)
		VALUES(100,13,'sender',9);
		UPDATE document_correspondents SET position=8
		WHERE document_id=100 AND correspondent_id=13 AND role='sender';
		DELETE FROM document_correspondents
		WHERE document_id=100 AND correspondent_id=13 AND role='sender'`); err != nil {
		t.Fatal(err)
	}
	assertMigrationScalar(t, d, `SELECT correspondent_revision FROM documents WHERE id=100`, 10)
}

func seedBetaFourOwnerAndCategory(t *testing.T, d *db.DB) {
	t.Helper()
	if _, err := d.ExecWrite(t.Context(), `
		INSERT INTO users(id,email,display_name,role,created_at,updated_at)
		VALUES(42,'owner@example.test','Owner','admin',1,1);
		INSERT INTO jd_areas(code_start,code_end,name,position,system_id)
		VALUES(40,49,'Inbox',1,1);
		INSERT INTO jd_categories(id,area_start,code,name,system,system_id)
		VALUES(1,40,49,'Inbox',1,1)`); err != nil {
		t.Fatal(err)
	}
}

func TestAccountIdentityDropsOnlyUnusedPluginKV(t *testing.T) {
	t.Run("empty unextended table", func(t *testing.T) {
		d, beta, log := openBetaFour(t)
		if err := db.Migrate(t.Context(), d, beta, log); err != nil {
			t.Fatal(err)
		}
		assertMigrationScalar(t, d, `
			SELECT count(*) FROM sqlite_schema WHERE name='plugin_kv'`, 0)
	})

	t.Run("stored rows", func(t *testing.T) {
		d, beta, log := openBetaFour(t)
		if _, err := d.ExecWrite(t.Context(), `
			INSERT INTO plugin_kv(plugin_name,key,value_json,updated_at)
			VALUES('example','key','{"preserved":true}',1)`); err != nil {
			t.Fatal(err)
		}
		if err := db.Migrate(t.Context(), d, beta, log); err == nil {
			t.Fatal("migration discarded a plugin_kv row")
		}
		assertSchemaVersion(t, d, 4)
		assertMigrationScalar(t, d, `
			SELECT count(*) FROM plugin_kv
			WHERE plugin_name='example' AND key='key'
			  AND value_json='{"preserved":true}'`, 1)
	})

	dependencies := []struct {
		name       string
		objectName string
		sql        string
	}{
		{
			name:       "index",
			objectName: "extension_plugin_kv_key",
			sql:        `CREATE INDEX extension_plugin_kv_key ON plugin_kv(key)`,
		},
		{
			name:       "trigger",
			objectName: "extension_plugin_kv_writer",
			sql: `CREATE TABLE extension_probe(id INTEGER PRIMARY KEY);
				CREATE TRIGGER extension_plugin_kv_writer AFTER INSERT ON extension_probe
				BEGIN
					INSERT INTO plugin_kv(plugin_name,key,value_json,updated_at)
					VALUES('example',NEW.id,'null',1);
				END`,
		},
	}
	for _, dependency := range dependencies {
		t.Run(dependency.name, func(t *testing.T) {
			d, beta, log := openBetaFour(t)
			if err := db.MigrateSet(t.Context(), d, db.MigrationSet{
				Component: "plugin-kv-probe-" + dependency.name,
				Migrations: []db.Migration{{
					Version: 1,
					Name:    "probe",
					SQL:     dependency.sql,
				}},
			}, log); err != nil {
				t.Fatal(err)
			}
			if err := db.Migrate(t.Context(), d, beta, log); err == nil {
				t.Fatalf("migration discarded plugin_kv-dependent %s", dependency.name)
			}
			assertSchemaVersion(t, d, 4)
			var objectCount int
			if err := d.Read.QueryRow(`
				SELECT count(*) FROM sqlite_schema
				WHERE name=?`, dependency.objectName).Scan(&objectCount); err != nil {
				t.Fatal(err)
			}
			if objectCount != 1 {
				t.Fatalf("%s count=%d after rejected migration", dependency.objectName, objectCount)
			}
			assertMigrationScalar(t, d, `
				SELECT count(*) FROM sqlite_schema
				WHERE name='plugin_kv' AND type='table'`, 1)
		})
	}
}

func TestAccountIdentityPreservesExtensionDDLAcrossRebuilds(t *testing.T) {
	d, _, log := openBetaFour(t)
	seedBetaFourOwnerAndCategory(t, d)
	const indexDDL = `CREATE INDEX extension_users_display_name ON users(display_name)`
	const triggerDDL = `CREATE TRIGGER extension_documents_insert AFTER INSERT ON documents
BEGIN
	INSERT INTO extension_replay_log(document_id,title) VALUES(NEW.id,NEW.title);
END`
	if err := db.MigrateSet(t.Context(), d, db.MigrationSet{
		Component: "rebuild-probe",
		Migrations: []db.Migration{{
			Version: 1,
			Name:    "probe",
			SQL: `CREATE TABLE extension_replay_log(document_id INTEGER NOT NULL, title TEXT NOT NULL);
` + indexDDL + `;
` + triggerDDL + `;`,
		}},
	}, log); err != nil {
		t.Fatal(err)
	}

	if err := migrations.Prepare(t.Context(), d, log); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{
		"extension_users_display_name": indexDDL,
		"extension_documents_insert":   triggerDDL,
	} {
		var got string
		if err := d.Read.QueryRow(`
			SELECT sql FROM sqlite_schema WHERE name=?`, name).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Fatalf("%s DDL changed\n got: %q\nwant: %q", name, got, want)
		}
	}
	if _, err := d.ExecWrite(t.Context(), `
		INSERT INTO documents(
			id,owner_id,original_blob,original_size,title,jd_category_id,
			created_at,updated_at,system_id
		) VALUES(200,42,'extension-doc',1,'Extension trigger',1,1,1,1)`); err != nil {
		t.Fatal(err)
	}
	assertMigrationScalar(t, d, `
		SELECT count(*) FROM extension_replay_log
		WHERE document_id=200 AND title='Extension trigger'`, 1)
	if err := migrations.Prepare(t.Context(), d, log); err != nil {
		t.Fatalf("repeat preparation with extension DDL: %v", err)
	}
}

func TestAccountIdentityPrunesDoneJobsAtExactBoundary(t *testing.T) {
	d, beta, log := openBetaFour(t)
	const cutoff = "1000000"
	migrationsAtFixedTime := append([]db.Migration(nil), beta...)
	migrationsAtFixedTime[4].SQL = strings.Replace(
		migrationsAtFixedTime[4].SQL,
		"unixepoch() - 604800",
		cutoff,
		1,
	)
	if migrationsAtFixedTime[4].SQL == beta[4].SQL {
		t.Fatal("retention cutoff expression not found in compatibility migration")
	}
	if _, err := d.ExecWrite(t.Context(), `
		INSERT INTO jobs(id,kind,state,next_run_at,created_at,updated_at) VALUES
			(1,'probe','done',0,1,999999),
			(2,'probe','done',0,1,1000000),
			(3,'probe','done',0,1,1000001),
			(4,'probe','pending',0,1,1),
			(5,'probe','running',0,1,1),
			(6,'probe','dead',0,1,1)`); err != nil {
		t.Fatal(err)
	}
	if err := db.Migrate(t.Context(), d, migrationsAtFixedTime, log); err != nil {
		t.Fatal(err)
	}
	var ids string
	if err := d.Read.QueryRow(`
		SELECT group_concat(id, ',')
		FROM (SELECT id FROM jobs ORDER BY id)`).Scan(&ids); err != nil {
		t.Fatal(err)
	}
	if ids != "2,3,4,5,6" {
		t.Fatalf("retained job ids = %q, want exact-boundary and non-done jobs", ids)
	}
}

func coreSchemaDDL(t *testing.T, d *db.DB) []string {
	t.Helper()
	rows, err := d.Read.Query(`
		SELECT type || char(0) || name || char(0) || tbl_name || char(0) || COALESCE(sql,'')
		FROM sqlite_schema
		WHERE name NOT GLOB 'sqlite_*'
		ORDER BY type,name,tbl_name,COALESCE(sql,'')`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var value string
		if err := rows.Scan(&value); err != nil {
			t.Fatal(err)
		}
		out = append(out, value)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}
