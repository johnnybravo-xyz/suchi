package db_test

import (
	"io"
	"log/slog"
	"path/filepath"
	"testing"

	"github.com/johnnybravo-xyz/suchi/core/db"
	"github.com/johnnybravo-xyz/suchi/core/db/migrations"
)

func TestTaxonomyMigrationBindsExistingApprovalWork(t *testing.T) {
	d, err := db.Open(t.Context(), filepath.Join(t.TempDir(), "approval-upgrade.db"))
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
	execMigrationFixture(t, d, `
		INSERT INTO approval_defs(id,slug,version,spec_json,active,created_at)
		VALUES(1,'review',1,'{}',1,1);
		INSERT INTO approval_runs(id,def_id,state,current_state,vars_json,state_entered_at,started_at)
		VALUES(5,1,'running','review','{}',1,1);
		INSERT INTO approval_transitions(id,run_id,from_state,to_state,trigger,payload_json,occurred_at)
		VALUES(7,5,'prepare','review','success','{}',1);
		INSERT INTO approval_tasks(id,run_id,state_key,assignee,prompt,choices_json,status,created_at)
		VALUES(9,5,'prepare','user:1','Earlier','["approve"]','resolved',1),
		      (10,5,'review','user:1','Current','["approve"]','open',1);
		INSERT INTO jobs(id,kind,payload,state,next_run_at,created_at,updated_at)
		VALUES(19,'approval:advance','{"run_id":5,"trigger":""}','done',1,1,1),
		      (20,'approval:advance','{"run_id":5,"trigger":""}','pending',1,1,1),
		      (21,'approval:advance','invalid payload','dead',1,1,1),
		      (22,'approval:advance','{"run_id":500,"trigger":""}','running',1,1,1);
	`)
	if err := db.Migrate(t.Context(), d, migs, log); err != nil {
		t.Fatal(err)
	}
	assertMigrationScalar(t, d, `SELECT state_revision FROM approval_tasks WHERE id=10`, 0)
	assertMigrationScalar(t, d, `SELECT state_revision FROM approval_tasks WHERE id=9`, -1)
	assertMigrationScalar(t, d, `SELECT json_extract(payload,'$.revision') FROM jobs WHERE id=20`, 0)
	assertMigrationScalar(t, d, `SELECT COUNT(*) FROM jobs WHERE id=21 AND payload='invalid payload' AND state='dead'`, 1)
	assertMigrationScalar(t, d, `SELECT seq FROM sqlite_sequence WHERE name='approval_runs'`, 500)
	assertMigrationScalar(t, d, `SELECT seq FROM sqlite_sequence WHERE name='approval_tasks'`, 10)
	assertMigrationScalar(t, d, `SELECT COUNT(*) FROM pragma_foreign_key_check`, 0)
}
