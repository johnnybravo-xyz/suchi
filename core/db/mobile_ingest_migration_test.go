package db_test

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/johnnybravo-xyz/suchi/core/db"
	migrations "github.com/johnnybravo-xyz/suchi/core/db/migrations"
)

func TestTaxonomyMigrationReleaseBoundary(t *testing.T) {
	migs, err := db.LoadMigrations(migrations.FS, ".")
	if err != nil {
		t.Fatal(err)
	}
	if got := migrationVersions(migs); !reflect.DeepEqual(got, []int{1, 2, 3}) {
		t.Fatalf("embedded migration versions = %v, want released [1 2] and one unreleased 3", got)
	}
	if !migs[2].RebuildTables {
		t.Fatal("migration 0003 must use the atomic table-rebuild runner")
	}
	// SHA-256 of exact files shipped in tag v0.1.0-beta.2.
	for filename, want := range map[string]string{
		"0001_baseline.sql":              "ab11d9ea4287c40853dd6a8f02bca12220c591606916776f14dff4bfddc217c4",
		"0002_document_intelligence.sql": "4d36e4afe006bd7792bd335280349e8e09bbb6727ee6c46a02eb293db28e8277",
	} {
		data, err := migrations.FS.ReadFile(filename)
		if err != nil {
			t.Fatal(err)
		}
		if got := fmt.Sprintf("%x", sha256.Sum256(data)); got != want {
			t.Errorf("released migration %s changed: SHA-256=%s, want %s", filename, got, want)
		}
	}
}

func TestMobileIngestMigrationUpgradesPopulatedBeta2(t *testing.T) {
	ctx := context.Background()
	d, err := db.Open(ctx, filepath.Join(t.TempDir(), "upgrade.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	migs, err := db.LoadMigrations(migrations.FS, ".")
	if err != nil {
		t.Fatal(err)
	}
	var baseline []db.Migration
	for _, migration := range migs {
		if migration.Version <= 2 {
			baseline = append(baseline, migration)
		}
	}
	if len(baseline) != 2 {
		t.Fatalf("beta.2 migrations = %d, want 2", len(baseline))
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	if err := db.Migrate(ctx, d, baseline, log); err != nil {
		t.Fatal(err)
	}
	assertSchemaVersion(t, d, 2)
	assertBeta2Schema(t, d)
	seedMobileMigrationOwnerAndCategory(t, d)
	if _, err := d.ExecWrite(ctx, `
		INSERT INTO documents(
			id, owner_id, original_blob, original_size, title,
			jd_category_id, created_at, updated_at
		) VALUES (1, 1, 'old-sha', 12, 'Existing', 1, 10, 10)
	`); err != nil {
		t.Fatal(err)
	}

	if err := db.Migrate(ctx, d, migs, log); err != nil {
		t.Fatal(err)
	}
	assertSchemaVersion(t, d, 3)
	assertBeta2Schema(t, d)
	var pairingCount int
	if err := d.Read.QueryRowContext(ctx, `SELECT COUNT(*) FROM mobile_pairings`).Scan(&pairingCount); err != nil || pairingCount != 0 {
		t.Fatalf("mobile pairing schema count=%d err=%v", pairingCount, err)
	}
	if err := db.Migrate(ctx, d, migs, log); err != nil {
		t.Fatalf("repeat mobile migration: %v", err)
	}

	var (
		contentSource string
		confidence    sql.NullFloat64
		language      string
		receivedAt    sql.NullInt64
		splitOrigin   int64
	)
	if err := d.Read.QueryRowContext(ctx, `
		SELECT content_source, device_content_confidence,
		       device_ocr_language, device_content_received_at, split_origin_id
		FROM documents WHERE id = 1
	`).Scan(&contentSource, &confidence, &language, &receivedAt, &splitOrigin); err != nil {
		t.Fatal(err)
	}
	if contentSource != "" || confidence.Valid || language != "" || receivedAt.Valid || splitOrigin != 0 {
		t.Fatalf("unexpected migrated defaults: source=%q confidence=%v language=%q received=%v origin=%d",
			contentSource, confidence, language, receivedAt, splitOrigin)
	}
	if _, err := d.ExecWrite(ctx, `UPDATE documents SET content_source = 'untrusted' WHERE id = 1`); err == nil {
		t.Fatal("invalid content_source was accepted")
	}
	if _, err := d.ExecWrite(ctx, `
		UPDATE documents
		SET content = 'device text', content_source = 'device_ocr',
		    device_content_confidence = 0.8, device_ocr_language = 'en_US',
		    device_content_received_at = 20, split_origin_id = 1, split_index = 0
		WHERE id = 1
	`); err != nil {
		t.Fatal(err)
	}
	if _, err := d.ExecWrite(ctx, `
		INSERT INTO documents(
			system_id, id, owner_id, original_blob, original_size, title,
			jd_category_id, created_at, updated_at, split_origin_id, split_index
		) VALUES (1, 2, 1, 'new-sha', 13, 'Collision', 1, 11, 11, 1, 0)
	`); err == nil {
		t.Fatal("duplicate split origin/index was accepted")
	}
	if _, err := d.ExecWrite(ctx, `
		INSERT INTO upload_idempotency(
			user_id, idempotency_key, operation, predecessor_id,
			request_fingerprint, sha256, document_id,
			response_status, response_json, created_at
		) VALUES (1, 'key', 'document', 0, 'fingerprint', 'old-sha', 1, 201, '{}', 20)
	`); err != nil {
		t.Fatal(err)
	}
	if _, err := d.ExecWrite(ctx, `DELETE FROM documents WHERE id = 1`); err != nil {
		t.Fatal(err)
	}
	var idempotencyRows int
	if err := d.Read.QueryRowContext(ctx, `SELECT COUNT(*) FROM upload_idempotency`).Scan(&idempotencyRows); err != nil {
		t.Fatal(err)
	}
	if idempotencyRows != 0 {
		t.Fatalf("idempotency rows after document deletion = %d, want 0", idempotencyRows)
	}
}

func TestMobileIngestMigrationCreatesFreshSchema(t *testing.T) {
	ctx := context.Background()
	d, err := db.Open(ctx, filepath.Join(t.TempDir(), "fresh.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	migs, err := db.LoadMigrations(migrations.FS, ".")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Migrate(ctx, d, migs, slog.New(slog.NewTextHandler(io.Discard, nil))); err != nil {
		t.Fatal(err)
	}
	assertSchemaVersion(t, d, 3)
	execMigrationFixture(t, d, `
		INSERT INTO users(id,email,display_name,role,created_at,updated_at)
			VALUES(1,'mobile@example.test','Mobile','member',1,1);
		INSERT INTO jd_areas(system_id,code_start,code_end,name,position)
			VALUES(1,40,49,'System',1);
		INSERT INTO jd_categories(system_id,id,area_start,code,name,system)
			VALUES(1,1,40,49,'Inbox',1);
		UPDATE jd_systems SET inbox_category_id=1 WHERE id=1;
	`)
	if _, err := d.ExecWrite(ctx, `
		INSERT INTO documents(
			system_id, id, owner_id, original_blob, original_size, title,
			jd_category_id, created_at, updated_at, content, content_source,
			device_content_confidence, device_ocr_language,
			device_content_received_at, split_origin_id, split_index
		) VALUES (1, 1, 1, 'sha', 12, 'Fresh', 1, 10, 10, 'text', 'server',
		          0.7, 'en_US', 10, 1, 0)
	`); err != nil {
		t.Fatal(err)
	}
	if _, err := d.ExecWrite(ctx, `
		INSERT INTO upload_idempotency(
			user_id, idempotency_key, operation, predecessor_id,
			request_fingerprint, sha256, document_id,
			response_status, response_json, created_at
		) VALUES (1, 'key', 'version', 1, 'fingerprint', 'sha', 1, 200, '{}', 10)
	`); err != nil {
		t.Fatal(err)
	}
	execMigrationFixture(t, d, `
		INSERT INTO mobile_pairings(user_id,system_id,code_hash,name,expires_at)
			VALUES(1,1,printf('%064d',1),'Fresh device',9999999999);
		INSERT INTO api_tokens(user_id,system_id,name,token_hash,scopes,source,created_at)
			VALUES(1,1,'Fresh mobile','fresh-token','documents:read','mobile_pairing',10);
	`)
	assertMigrationScalar(t, d, `SELECT count(*) FROM jd_system_members WHERE user_id=1 AND system_id=1`, 1)
	assertMigrationScalar(t, d, `SELECT count(*) FROM mobile_pairings WHERE user_id=1 AND system_id=1`, 1)
	assertMigrationScalar(t, d, `SELECT count(*) FROM api_tokens WHERE user_id=1 AND system_id=1 AND source='mobile_pairing'`, 1)
}

func seedMobileMigrationOwnerAndCategory(t *testing.T, d *db.DB) {
	t.Helper()
	ctx := context.Background()
	if _, err := d.ExecWrite(ctx, `
		INSERT INTO users(id, email, display_name, role, created_at, updated_at)
		VALUES (1, 'mobile@example.test', 'Mobile', 'member', 1, 1);
		INSERT INTO jd_areas(code_start, code_end, name, position)
		VALUES (40, 49, 'Inbox', 1);
		INSERT INTO jd_categories(id, area_start, code, name, system)
		VALUES (1, 40, 49, 'Inbox', 1);
	`); err != nil {
		t.Fatal(err)
	}
}

func TestMobileTokenMigrationPreservesPublishedBeta2Credentials(t *testing.T) {
	d, err := db.Open(t.Context(), filepath.Join(t.TempDir(), "beta2-tokens.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	migs, err := db.LoadMigrations(migrations.FS, ".")
	if err != nil {
		t.Fatal(err)
	}
	var baseline []db.Migration
	for _, migration := range migs {
		if migration.Version <= 2 {
			baseline = append(baseline, migration)
		}
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	if err := db.Migrate(t.Context(), d, baseline, log); err != nil {
		t.Fatal(err)
	}
	assertSchemaVersion(t, d, 2)
	seedMobileMigrationOwnerAndCategory(t, d)
	if _, err := d.ExecWrite(t.Context(), `INSERT INTO api_tokens(user_id,name,token_hash,scopes,created_at,last_used_at)
		VALUES(1,'Suchi mobile','legacy-hash','documents:read',10,20)`); err != nil {
		t.Fatal(err)
	}
	if err := db.Migrate(t.Context(), d, migs, log); err != nil {
		t.Fatal(err)
	}
	assertSchemaVersion(t, d, 3)
	var name, hash, scopes, source string
	var created, used int64
	if err := d.Read.QueryRow(`SELECT name,token_hash,scopes,created_at,last_used_at,source FROM api_tokens`).
		Scan(&name, &hash, &scopes, &created, &used, &source); err != nil {
		t.Fatal(err)
	}
	if name != "Suchi mobile" || hash != "legacy-hash" || scopes != "documents:read" || created != 10 || used != 20 || source != "" {
		t.Fatal("upgrade changed a legacy credential or guessed pairing provenance")
	}
	if _, err := d.ExecWrite(t.Context(), `UPDATE api_tokens SET source='invalid'`); err == nil {
		t.Fatal("invalid token source accepted")
	}
	if err := db.Migrate(t.Context(), d, migs, log); err != nil {
		t.Fatal(err)
	}
}

func TestTaxonomySystemsPreservePopulatedBeta2(t *testing.T) {
	ctx := t.Context()
	d, err := db.Open(ctx, filepath.Join(t.TempDir(), "systems-upgrade.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	migs, err := db.LoadMigrations(migrations.FS, ".")
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	if err := db.Migrate(ctx, d, migs[:2], log); err != nil {
		t.Fatal(err)
	}
	seedMobileMigrationOwnerAndCategory(t, d)
	execMigrationFixture(t, d, `
		INSERT INTO settings(key,value_json,updated_at) VALUES
			('taxonomy','"flat"',1),('jd_inbox_category_id','1',1),
			('preset','"older"',1),('taxonomy_preset_id','"explicit"',1),
			('taxonomy_preset_version','7',1),('taxonomy_preset_sha256','"digest"',1),
			('taxonomy_authoring','{"name":"Legacy Archive","id":"explicit","version":7}',1),
			('unrelated','"retained"',1);
		INSERT INTO documents(id,owner_id,original_blob,original_size,title,content,jd_category_id,created_at,updated_at,trashed_at,previous_version_id,split_parent_id,split_index,email_parent_id,legacy_id,archive_serial_number) VALUES
			(147,1,'old',12,'Preserved','migrationneedle',1,10,11,NULL,NULL,NULL,NULL,NULL,17,19),
			(148,1,'version',13,'Version','',1,10,11,NULL,147,NULL,NULL,NULL,NULL,NULL),
			(149,1,'trash',14,'Trash','',1,10,11,12,NULL,NULL,NULL,NULL,NULL,NULL),
			(150,1,'split',15,'Split','',1,10,11,NULL,NULL,147,0,NULL,NULL,NULL),
			(151,1,'attachment',16,'Attachment','',1,10,11,NULL,NULL,NULL,NULL,147,NULL,NULL);
		INSERT INTO tags(id,name,slug,created_at,updated_at) VALUES(1,'Tax','tax',1,1);
		INSERT INTO document_tags(document_id,tag_id,classifier_owned) VALUES(147,1,1);
		INSERT INTO object_acls(object_kind,object_id,principal_kind,principal_id,perm_bits,created_at)
			VALUES('document',147,'user',1,1,1);
		INSERT INTO document_sources(document_id,kind,label,detail,observed_at)
			VALUES(147,'upload','Original upload','retained detail',1);
		INSERT INTO notes(document_id,user_id,note,created_at) VALUES(147,1,'History',1);
		INSERT INTO render_moves(document_id,prev_path,new_path,state,created_at)
			VALUES(147,'','legacy/path.txt','applied',1);
		INSERT INTO document_intelligence(document_id,intelligence_type,value_json,evidence_text,confidence,extractor,extraction_version,created_at,updated_at)
			VALUES(147,'date','"2026-01-01"','migrationneedle',0.9,'legacy',1,1,1);
		INSERT INTO api_tokens(id,user_id,name,token_hash,scopes,created_at)
			VALUES(7,1,'Mobile','unchanged-hash','documents:read',1);
		INSERT INTO email_accounts(id,name,owner_id,provider,host,port,auth_method,username,sealed_secret,created_at,updated_at) VALUES
			(20,'Retained mailbox',1,'imap','imap.example.test',993,'password','retained',X'010203',1,1),
			(80,'Deleted mailbox',1,'imap','imap.example.test',993,'password','deleted',X'040506',1,1);
		DELETE FROM email_accounts WHERE id=80;
		INSERT INTO jobs(id,kind,payload,state,attempts,next_run_at,created_at,updated_at) VALUES
			(1,'taxonomy_index','{}','pending',2,3,4,5),
			(2,'taxonomy_index','{}','running',3,4,5,6),
			(3,'taxonomy_index','{}','dead',4,5,6,7);
		INSERT INTO audit_events(ts,actor_kind,action,object_kind,object_id)
			VALUES(1,'system','document.update','document',147),(1,'system','auth.login','user',1);
	`)
	for range 2 {
		if err := db.Migrate(ctx, d, migs, log); err != nil {
			t.Fatal(err)
		}
	}
	assertSchemaVersion(t, d, 3)
	assertMigrationScalar(t, d, `SELECT count(*) FROM documents WHERE system_id=1`, 5)
	assertMigrationScalar(t, d, `SELECT count(*) FROM documents WHERE content_source='' AND device_content_confidence IS NULL AND device_ocr_language='' AND device_content_received_at IS NULL AND split_origin_id=0`, 5)
	assertMigrationScalar(t, d, `SELECT count(*) FROM mobile_pairings`, 0)
	assertMigrationScalar(t, d, `SELECT count(*) FROM upload_idempotency`, 0)
	assertMigrationScalar(t, d, `SELECT count(*) FROM documents WHERE id=147 AND legacy_id=17 AND archive_serial_number=19 AND original_blob='old'`, 1)
	assertMigrationScalar(t, d, `SELECT count(*) FROM documents WHERE (id=148 AND previous_version_id=147) OR (id=149 AND trashed_at=12) OR (id=150 AND split_parent_id=147 AND split_index=0) OR (id=151 AND email_parent_id=147)`, 4)
	assertMigrationScalar(t, d, `SELECT count(*) FROM jd_systems WHERE id=1 AND code='' AND name='Legacy Archive' AND taxonomy='flat' AND inbox_category_id=1 AND preset_id='explicit' AND preset_version=7 AND preset_sha256='digest' AND json_extract(authoring_json,'$.name')='Legacy Archive'`, 1)
	assertMigrationScalar(t, d, `SELECT count(*) FROM settings WHERE key IN ('taxonomy','jd_inbox_category_id','preset','taxonomy_preset_id','taxonomy_preset_version','taxonomy_preset_sha256','taxonomy_authoring')`, 0)
	assertMigrationScalar(t, d, `SELECT count(*) FROM settings WHERE key='unrelated' AND value_json='"retained"'`, 1)
	assertMigrationScalar(t, d, `SELECT count(*) FROM jd_system_members WHERE system_id=1 AND user_id=1`, 1)
	assertMigrationScalar(t, d, `SELECT count(*) FROM api_tokens WHERE id=7 AND system_id=1 AND token_hash='unchanged-hash' AND source=''`, 1)
	assertMigrationScalar(t, d, `SELECT count(*) FROM email_accounts WHERE id=20 AND system_id=1 AND name='Retained mailbox' AND sealed_secret=X'010203'`, 1)
	assertMigrationScalar(t, d, `SELECT count(*) FROM document_tags WHERE document_id=147 AND tag_id=1 AND classifier_owned=1`, 1)
	assertMigrationScalar(t, d, `SELECT count(*) FROM object_acls WHERE object_id=147 AND principal_id=1 AND perm_bits=1`, 1)
	assertMigrationScalar(t, d, `SELECT count(*) FROM document_sources WHERE document_id=147 AND detail='retained detail'`, 1)
	assertMigrationScalar(t, d, `SELECT count(*) FROM notes WHERE document_id=147 AND note='History'`, 1)
	assertMigrationScalar(t, d, `SELECT count(*) FROM render_moves WHERE document_id=147 AND new_path='legacy/path.txt' AND prev_blob='' AND new_blob=''`, 1)
	assertMigrationScalar(t, d, `SELECT count(*) FROM document_intelligence WHERE document_id=147 AND evidence_text='migrationneedle'`, 1)
	assertMigrationScalar(t, d, `SELECT count(*) FROM jobs WHERE system_id=1 AND payload='{"system_id":1}' AND attempts=id+1 AND next_run_at=id+2 AND created_at=id+3 AND updated_at=id+4 AND ((id=1 AND state='pending') OR (id=2 AND state='running') OR (id=3 AND state='dead'))`, 3)
	assertMigrationScalar(t, d, `SELECT count(*) FROM audit_events WHERE action='document.update' AND system_id=1`, 1)
	assertMigrationScalar(t, d, `SELECT count(*) FROM audit_events WHERE action='auth.login' AND system_id IS NULL`, 1)
	if got := ftsHits(t, d, "migrationneedle"); got != 1 {
		t.Fatalf("migrated FTS hits=%d", got)
	}
	assertMigrationScalar(t, d, `SELECT count(*) FROM pragma_foreign_key_check`, 0)
	execMigrationFixture(t, d, `
		DELETE FROM documents WHERE id=151;
		INSERT INTO documents(system_id,owner_id,original_blob,original_size,jd_category_id,created_at,updated_at)
			VALUES(1,1,'after-purge',1,1,1,1);
		UPDATE documents SET content='newmigrationneedle' WHERE id=147;
		DELETE FROM api_tokens WHERE id=7;
		INSERT INTO api_tokens(system_id,user_id,name,token_hash,scopes,created_at)
			VALUES(1,1,'After revoke','new-hash','documents:read',20);
		INSERT INTO email_accounts(system_id,name,owner_id,provider,host,port,auth_method,username,sealed_secret,created_at,updated_at)
			VALUES(1,'After deletion',1,'imap','imap.example.test',993,'password','new',X'070809',1,1);
	`)
	assertMigrationScalar(t, d, `SELECT id FROM documents WHERE original_blob='after-purge'`, 152)
	assertMigrationScalar(t, d, `SELECT id FROM api_tokens WHERE token_hash='new-hash'`, 8)
	assertMigrationScalar(t, d, `SELECT id FROM email_accounts WHERE name='After deletion'`, 81)
	if got := ftsHits(t, d, "newmigrationneedle"); got != 1 {
		t.Fatalf("rebuilt FTS update hits=%d", got)
	}
	if got := ftsHits(t, d, "migrationneedle"); got != 0 {
		t.Fatalf("rebuilt FTS retained stale term: %d", got)
	}
}

func TestTaxonomySystemsLeaveInvalidLegacyInboxForScopedRepair(t *testing.T) {
	for _, pointer := range []string{"missing", "null", "999", "9"} {
		t.Run(pointer, func(t *testing.T) {
			d, err := db.Open(t.Context(), filepath.Join(t.TempDir(), "legacy-inbox.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer d.Close()
			migs, err := db.LoadMigrations(migrations.FS, ".")
			if err != nil {
				t.Fatal(err)
			}
			log := slog.New(slog.NewTextHandler(io.Discard, nil))
			if err := db.Migrate(t.Context(), d, migs[:2], log); err != nil {
				t.Fatal(err)
			}
			seedMobileMigrationOwnerAndCategory(t, d)
			// Historical filing decades are not fed through today's strict
			// author-file grammar or renumbered during the storage migration.
			execMigrationFixture(t, d, `
				INSERT INTO jd_areas(code_start,code_end,name,position) VALUES(0,9,'Legacy decade',7);
				INSERT INTO jd_categories(id,area_start,code,name,system) VALUES(9,0,1,'Old filing',0);
			`)
			if pointer != "missing" {
				if _, err := d.ExecWrite(t.Context(), `INSERT INTO settings(key,value_json,updated_at) VALUES('jd_inbox_category_id',?,1)`, pointer); err != nil {
					t.Fatal(err)
				}
			}
			if err := db.Migrate(t.Context(), d, migs, log); err != nil {
				t.Fatal(err)
			}
			assertMigrationScalar(t, d, `SELECT count(*) FROM jd_systems WHERE id=1 AND inbox_category_id IS NULL`, 1)
			assertMigrationScalar(t, d, `SELECT count(*) FROM jd_categories WHERE system_id=1 AND id=9 AND area_start=0 AND code=1 AND name='Old filing'`, 1)
			assertMigrationScalar(t, d, `SELECT count(*) FROM jd_areas WHERE system_id=1 AND code_start=0 AND code_end=9 AND position=7`, 1)
		})
	}
}

func TestTaxonomyMigrationFailureRestoresPublishedBeta2(t *testing.T) {
	for _, tc := range []struct {
		name, failure, want string
	}{
		{"SQL failure", `INSERT INTO missing_migration_test_table VALUES(1);`, "missing_migration_test_table"},
		{"foreign key failure", `INSERT INTO notes(document_id,user_id,note,created_at) VALUES(999999,1,'Broken reference',1);`, "foreign key violation"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, err := db.Open(t.Context(), filepath.Join(t.TempDir(), "rollback.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer d.Close()
			migs, err := db.LoadMigrations(migrations.FS, ".")
			if err != nil {
				t.Fatal(err)
			}
			log := slog.New(slog.NewTextHandler(io.Discard, nil))
			if err := db.Migrate(t.Context(), d, migs[:2], log); err != nil {
				t.Fatal(err)
			}
			seedMobileMigrationOwnerAndCategory(t, d)
			execMigrationFixture(t, d, `
				INSERT INTO settings(key,value_json,updated_at) VALUES('taxonomy','"jd"',1),('jd_inbox_category_id','1',1);
				INSERT INTO documents(id,owner_id,original_blob,original_size,title,content,jd_category_id,created_at,updated_at)
					VALUES(147,1,'retained',1,'Retained','rollbackneedle',1,1,1);
				INSERT INTO api_tokens(user_id,name,token_hash,scopes,created_at)
					VALUES(1,'Retained token','retained-token','documents:read',1);
			`)
			broken := append([]db.Migration(nil), migs...)
			broken[2].SQL += "\n" + tc.failure
			if err := db.Migrate(t.Context(), d, broken, log); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("injected migration failure = %v, want %s", err, tc.want)
			}
			assertSchemaVersion(t, d, 2)
			assertBeta2Schema(t, d)
			assertMigrationScalar(t, d, `SELECT count(*) FROM documents WHERE id=147 AND original_blob='retained' AND content='rollbackneedle'`, 1)
			assertMigrationScalar(t, d, `SELECT count(*) FROM api_tokens WHERE token_hash='retained-token'`, 1)
			assertMigrationScalar(t, d, `SELECT count(*) FROM settings WHERE key IN ('taxonomy','jd_inbox_category_id')`, 2)
			assertMigrationScalar(t, d, `SELECT count(*) FROM sqlite_schema WHERE name IN ('jd_systems','mobile_pairings','upload_idempotency')`, 0)
			assertMigrationScalar(t, d, `SELECT count(*) FROM pragma_table_info('documents') WHERE name IN ('system_id','content_source','split_origin_id')`, 0)
			if got := ftsHits(t, d, "rollbackneedle"); got != 1 {
				t.Fatalf("rollback lost FTS entry: %d", got)
			}
			for pragma, want := range map[string]int{"foreign_keys": 1, "legacy_alter_table": 0} {
				var got int
				if err := d.Write.QueryRowContext(t.Context(), "PRAGMA "+pragma).Scan(&got); err != nil || got != want {
					t.Fatalf("writer %s=%d err=%v, want %d", pragma, got, err, want)
				}
			}
			if err := db.Migrate(t.Context(), d, migs, log); err != nil {
				t.Fatalf("retry after failed upgrade: %v", err)
			}
			assertSchemaVersion(t, d, 3)
			assertMigrationScalar(t, d, `SELECT count(*) FROM documents WHERE system_id=1 AND id=147 AND original_blob='retained'`, 1)
		})
	}
}

func execMigrationFixture(t *testing.T, d *db.DB, query string) {
	t.Helper()
	if _, err := d.ExecWrite(t.Context(), query); err != nil {
		t.Fatal(err)
	}
}

func assertMigrationScalar(t *testing.T, d *db.DB, query string, want int64) {
	t.Helper()
	var got int64
	if err := d.Read.QueryRowContext(t.Context(), query).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("%s: got %d, want %d", query, got, want)
	}
}
