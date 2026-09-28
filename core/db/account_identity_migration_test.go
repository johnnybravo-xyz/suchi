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
	objects := map[string]string{
		"audit_actor":                   "index",
		"audit_events":                  "table",
		"audit_events_system_immutable": "trigger",
		"audit_events_system_replace":   "trigger",
		"audit_object":                  "index",
		"audit_system":                  "index",
		"audit_ts":                      "index",
		"users":                         "table",
		"users_default_system_demotion": "trigger",
		"users_default_system_insert":   "trigger",
		"users_oidc_identity":           "index",
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
