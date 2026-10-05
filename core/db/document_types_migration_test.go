// SPDX-License-Identifier: AGPL-3.0-or-later

package db_test

import (
	"io"
	"log/slog"
	"path/filepath"
	"testing"

	"github.com/johnnybravo-xyz/suchi/core/db"
	migrations "github.com/johnnybravo-xyz/suchi/core/db/migrations"
)

func TestDocumentTypesMigrateToNamespacedTags(t *testing.T) {
	migs, err := db.LoadMigrations(migrations.FS, ".")
	if err != nil {
		t.Fatal(err)
	}
	if len(migs) != 2 {
		t.Fatalf("stable migrations = %d, want 2", len(migs))
	}

	d, err := db.Open(t.Context(), filepath.Join(t.TempDir(), "document-types.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	if err := db.Migrate(t.Context(), d, migs[:1], log); err != nil {
		t.Fatal(err)
	}

	execMigrationFixture(t, d, `
		INSERT INTO users(id,email,display_name,role,created_at,updated_at)
		VALUES(1,'owner@example.test','Owner','admin',1,1);
		INSERT INTO jd_areas(system_id,code_start,code_end,name,position)
		VALUES(1,0,9,'System',0);
		INSERT INTO jd_categories(id,system_id,area_start,code,name)
		VALUES(1,1,0,1,'Inbox');

		INSERT INTO document_types(id,system_id,name,slug,matching_algorithm,match,is_insensitive,created_at,updated_at) VALUES
			(10,1,'Invoice','invoice',4,'invoice-regex',0,1,2),
			(11,1,'Statement','statement',2,'statement-regex',1,3,4);
		INSERT INTO tags(id,system_id,name,slug,created_at,updated_at) VALUES
			(20,1,'urgent','urgent',1,1),
			(22,1,'type:Invoice','type-invoice',1,1);
		INSERT INTO documents(id,system_id,owner_id,original_blob,original_size,title,jd_category_id,document_type_id,created_at,updated_at)
		VALUES(100,1,1,'blob',1,'Invoice',1,10,1,1);

		INSERT INTO automations(id,system_id,name,created_at,updated_at) VALUES
			(30,1,'type only',1,1),
			(31,1,'tag and type',1,1);
		INSERT INTO automation_triggers(id,automation_id,type,filter_doctype_id,created_at) VALUES
			(40,30,'document_added',10,1),
			(41,31,'document_added',10,1);
		UPDATE automation_triggers SET filter_tag_id=20 WHERE id=41;
		INSERT INTO automation_actions(id,automation_id,order_index,kind,params_json,created_at) VALUES
			(50,30,0,'assign_document_type','{"document_type_id":10}',1),
			(51,30,1,'remove_document_type','{}',1),
			(52,30,2,'assign_title','{"template":"{{ document_type }} - {{title}}"}',1);

		INSERT INTO storage_paths(id,system_id,name,slug,path,created_at,updated_at)
		VALUES(60,1,'By type','by-type','{{document_type}}/{{title}}',1,1);
		INSERT INTO saved_views(id,system_id,owner_id,name,filter_json,created_at,updated_at)
		VALUES(70,1,1,'Invoices','{"q":"type:\"Invoice\"","document_type__id":10,"tags__id__in":"20"}',1,1);
		INSERT INTO object_acls(id,object_kind,object_id,principal_kind,principal_id,perm_bits,created_at,created_by) VALUES
			(80,'tag',22,'user',1,1,1,1),
			(81,'document_type',10,'user',1,2,2,1);

		INSERT INTO approval_defs(id,system_id,slug,version,spec_json,created_at)
		VALUES(90,1,'document-change',1,'{}',1);
		INSERT INTO approval_runs(id,system_id,def_id,doc_id,state,current_state,vars_json,state_entered_at,started_at)
		VALUES(91,1,90,100,'running','review','{"field":"document_type"}',1,1);
		INSERT INTO approval_tasks(id,run_id,state_key,assignee,prompt,choices_json,status,created_at)
		VALUES(92,91,'review','role:admin','Review','["apply","reject"]','open',1);
	`)

	if err := db.Migrate(t.Context(), d, migs, log); err != nil {
		t.Fatal(err)
	}
	assertSchemaVersion(t, d, 2)

	assertMigrationScalar(t, d, `SELECT count(*) FROM sqlite_schema WHERE type='table' AND name='document_types'`, 0)
	assertMigrationScalar(t, d, `SELECT count(*) FROM pragma_table_info('documents') WHERE name IN ('document_type_id','document_type_revision')`, 0)
	assertMigrationScalar(t, d, `SELECT count(*) FROM pragma_table_info('automation_triggers') WHERE name='filter_doctype_id'`, 0)
	assertMigrationScalar(t, d, `SELECT count(*) FROM document_tags dt JOIN tags t ON t.id=dt.tag_id WHERE dt.document_id=100 AND dt.classifier_owned=0 AND t.name='type:Invoice'`, 1)
	assertMigrationScalar(t, d, `SELECT count(*) FROM tags WHERE system_id=1 AND name='type:Statement' AND slug='type-statement' AND matching_algorithm=2 AND match='statement-regex'`, 1)
	assertMigrationScalar(t, d, `SELECT filter_tag_id FROM automation_triggers WHERE id=40`, 22)
	assertMigrationScalar(t, d, `SELECT suspended FROM automations WHERE id=31`, 1)
	assertMigrationText(t, d, `SELECT kind || ':' || params_json FROM automation_actions WHERE id=50`, `assign_tags:{"tag_ids":[22]}`)
	assertMigrationScalar(t, d, `SELECT count(*) FROM json_each((SELECT params_json FROM automation_actions WHERE id=51),'$.tag_ids')`, 2)
	assertMigrationText(t, d, `SELECT json_extract(params_json,'$.template') FROM automation_actions WHERE id=52`, `{{tags}} - {{title}}`)
	assertMigrationText(t, d, `SELECT path FROM storage_paths WHERE id=60`, `{{tag_list}}/{{title}}`)
	assertMigrationText(t, d, `SELECT json_extract(filter_json,'$.q') FROM saved_views WHERE id=70`, `tag:"type:Invoice"`)
	assertMigrationText(t, d, `SELECT json_extract(filter_json,'$.tags__id__in') FROM saved_views WHERE id=70`, `20,22`)
	assertMigrationScalar(t, d, `SELECT json_type(filter_json,'$.document_type__id') IS NULL FROM saved_views WHERE id=70`, 1)
	assertMigrationScalar(t, d, `SELECT perm_bits FROM object_acls WHERE object_kind='tag' AND object_id=22 AND principal_kind='user' AND principal_id=1`, 3)
	assertMigrationText(t, d, `SELECT state FROM approval_runs WHERE id=91`, `cancelled`)
	assertMigrationText(t, d, `SELECT status FROM approval_tasks WHERE id=92`, `expired`)
}

func assertMigrationText(t *testing.T, d *db.DB, query, want string) {
	t.Helper()
	var got string
	if err := d.Read.QueryRowContext(t.Context(), query).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("%s: got %q, want %q", query, got, want)
	}
}
