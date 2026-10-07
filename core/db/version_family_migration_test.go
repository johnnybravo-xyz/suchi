// SPDX-License-Identifier: AGPL-3.0-or-later

package db_test

import (
	"database/sql"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/johnnybravo-xyz/suchi/core/db"
	"github.com/johnnybravo-xyz/suchi/core/db/migrations"
)

func TestVersionFamilyMigrationBackfillsBranches(t *testing.T) {
	stable, err := db.LoadMigrations(migrations.FS, ".")
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	d := openAdoptionDB(t, t.TempDir()+"/suchi.db")
	if err := db.Migrate(t.Context(), d, stable[:1], log); err != nil {
		t.Fatal(err)
	}
	seedVersionMigrationPrincipals(t, d)
	root := insertVersionMigrationDocument(t, d, "root", nil)
	first := insertVersionMigrationDocument(t, d, "first", &root)
	_ = insertVersionMigrationDocument(t, d, "second", &root)
	_ = insertVersionMigrationDocument(t, d, "grandchild", &first)
	standalone := insertVersionMigrationDocument(t, d, "standalone", nil)

	if err := migrations.Prepare(t.Context(), d, log); err != nil {
		t.Fatal(err)
	}

	rows, err := d.Read.Query(`SELECT id, version_family_key FROM documents WHERE id != ? ORDER BY id`, standalone)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var family string
	for rows.Next() {
		var id int64
		var key string
		if err := rows.Scan(&id, &key); err != nil {
			t.Fatal(err)
		}
		if family == "" {
			family = key
		}
		if key != family {
			t.Fatalf("document %d family=%q, want %q", id, key, family)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(family, "m:") || len(family) != 34 {
		t.Fatalf("backfilled family key=%q", family)
	}
	var standaloneKey sql.NullString
	if err := d.Read.QueryRow(`SELECT version_family_key FROM documents WHERE id=?`, standalone).Scan(&standaloneKey); err != nil {
		t.Fatal(err)
	}
	if standaloneKey.Valid {
		t.Fatalf("standalone family key=%q, want NULL", standaloneKey.String)
	}
	assertStableIdentity(t, d)
}

func TestVersionFamilyMigrationRejectsCycles(t *testing.T) {
	stable, err := db.LoadMigrations(migrations.FS, ".")
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	d := openAdoptionDB(t, t.TempDir()+"/suchi.db")
	if err := db.Migrate(t.Context(), d, stable[:1], log); err != nil {
		t.Fatal(err)
	}
	seedVersionMigrationPrincipals(t, d)
	first := insertVersionMigrationDocument(t, d, "first", nil)
	second := insertVersionMigrationDocument(t, d, "second", &first)
	if _, err := d.Write.Exec(`UPDATE documents SET previous_version_id=? WHERE id=?`, second, first); err != nil {
		t.Fatal(err)
	}

	err = migrations.Prepare(t.Context(), d, log)
	if err == nil || !strings.Contains(err.Error(), "version_family_tree_valid") {
		t.Fatalf("migration error=%v, want version family cycle rejection", err)
	}
	assertSchemaVersion(t, d, 1)
	assertMigrationScalar(t, d, `SELECT count(*) FROM pragma_table_info('documents') WHERE name='version_family_key'`, 0)
}

func TestDocumentLinkTopologyTriggersRejectSelfAndVersionFamily(t *testing.T) {
	stable, err := db.LoadMigrations(migrations.FS, ".")
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	d := openAdoptionDB(t, t.TempDir()+"/suchi.db")
	if err := db.Migrate(t.Context(), d, stable, log); err != nil {
		t.Fatal(err)
	}
	seedVersionMigrationPrincipals(t, d)
	source := insertVersionMigrationDocument(t, d, "source", nil)
	version := insertVersionMigrationDocument(t, d, "version", nil)
	peer := insertVersionMigrationDocument(t, d, "peer", nil)
	if _, err := d.Write.Exec(`
		UPDATE documents SET version_family_key='v:00000000000000000000000000000000'
		WHERE id IN (?,?);
		INSERT INTO custom_fields(system_id,name,data_type,created_at,updated_at)
		VALUES(1,'Related','documentlink',1,1)
	`, source, version); err != nil {
		t.Fatal(err)
	}
	var fieldID int64
	if err := d.Read.QueryRow(`SELECT id FROM custom_fields WHERE name='Related'`).Scan(&fieldID); err != nil {
		t.Fatal(err)
	}
	write := func(target int64) error {
		_, err := d.Write.Exec(`
			INSERT INTO document_custom_field_values(document_id,field_id,value_int)
			VALUES(?,?,?)
		`, source, fieldID, target)
		return err
	}
	for name, target := range map[string]int64{"self": source, "same family": version} {
		if err := write(target); err == nil || !strings.Contains(err.Error(), "invalid document link topology") {
			t.Fatalf("%s link error=%v", name, err)
		}
	}
	if err := write(peer); err != nil {
		t.Fatalf("unrelated peer link: %v", err)
	}
	if _, err := d.Write.Exec(`
		UPDATE document_custom_field_values SET value_int=?
		WHERE document_id=? AND field_id=?
	`, source, source, fieldID); err == nil || !strings.Contains(err.Error(), "invalid document link topology") {
		t.Fatalf("self-link update error=%v", err)
	}
}

func seedVersionMigrationPrincipals(t *testing.T, d *db.DB) {
	t.Helper()
	if _, err := d.Write.Exec(`
		INSERT INTO users(id,email,display_name,role,created_at,updated_at)
		VALUES(1,'owner@example.test','Owner','admin',1,1);
		INSERT INTO jd_areas(system_id,code_start,code_end,name,position) VALUES(1,0,9,'Inbox',0);
		INSERT INTO jd_categories(id,area_start,code,name,system,system_id)
		VALUES(1,0,0,'Inbox',1,1)`); err != nil {
		t.Fatal(err)
	}
}

func insertVersionMigrationDocument(t *testing.T, d *db.DB, blob string, previous *int64) int64 {
	t.Helper()
	result, err := d.Write.Exec(`
		INSERT INTO documents(owner_id,original_blob,original_size,title,jd_category_id,created_at,updated_at,previous_version_id,system_id)
		VALUES(1,?,1,?,1,1,1,?,1)`, blob, blob, previous)
	if err != nil {
		t.Fatal(err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	return id
}
