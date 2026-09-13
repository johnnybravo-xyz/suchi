package systems_test

import (
	"database/sql"
	"errors"
	"io"
	"log/slog"
	"math"
	"path/filepath"
	"testing"

	"github.com/johnnybravo-xyz/suchi/core/db"
	"github.com/johnnybravo-xyz/suchi/core/db/migrations"
	"github.com/johnnybravo-xyz/suchi/core/jd/systems"
)

func TestCanonicalAddresses(t *testing.T) {
	for _, tc := range []struct {
		code     string
		category int
		id       int64
		address  string
	}{
		{"S01", 13, 147, "S01.13.147"},
		{"A00", 0, 1, "A00.00.1"},
		{"Z99", 99, math.MaxInt64, "Z99.99.9223372036854775807"},
	} {
		if got := systems.Address(tc.code, tc.category, tc.id); got != tc.address {
			t.Fatalf("address=%q, want %q", got, tc.address)
		}
		code, category, id, err := systems.ParseAddress(tc.address)
		if err != nil || code != tc.code || category != tc.category || id != tc.id {
			t.Fatalf("parse %q: %q %d %d %v", tc.address, code, category, id, err)
		}
	}
	for _, value := range []string{
		"", "S01", "S01.13", "s01.13.147", "AUD.13.147", "S1.13.147",
		"S001.13.147", "S01.1.147", "S01.013.147", "S01.13.0", "S01.13.0147",
		"S01.13.-1", "S01.13.+1", " S01.13.147", "S01.13.147 ", "S01.13.147\n",
		"S01.13.9223372036854775808", "S01.13.1.1", "S01.１３.147", "S01.13.١",
	} {
		if _, _, _, err := systems.ParseAddress(value); !errors.Is(err, systems.ErrBadAddress) {
			t.Errorf("accepted noncanonical address %q: %v", value, err)
		}
	}
	if systems.Address("", 13, 147) != "" || systems.Address("S01", 13, 0) != "" || systems.Address("S01", 100, 147) != "" {
		t.Fatal("generated an address without a valid filing identity")
	}
}

func openSystems(t *testing.T) *db.DB {
	t.Helper()
	d, err := db.Open(t.Context(), filepath.Join(t.TempDir(), "systems.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	migs, err := db.LoadMigrations(migrations.FS, ".")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Migrate(t.Context(), d, migs, slog.New(slog.NewTextHandler(io.Discard, nil))); err != nil {
		t.Fatal(err)
	}
	return d
}

func exec(t *testing.T, d *db.DB, query string) {
	t.Helper()
	if _, err := d.ExecWrite(t.Context(), query); err != nil {
		t.Fatal(err)
	}
}

func count(t *testing.T, d *db.DB, query string, want int) {
	t.Helper()
	var got int
	if err := d.Read.QueryRowContext(t.Context(), query).Scan(&got); err != nil || got != want {
		t.Fatalf("%s: got=%d want=%d err=%v", query, got, want, err)
	}
}

func TestIntroductionMembershipAndCredentialRevocation(t *testing.T) {
	d := openSystems(t)
	ctx := t.Context()
	exec(t, d, `INSERT INTO users(id,email,display_name,role,created_at,updated_at) VALUES
		(1,'admin@test','Admin','admin',1,1),(2,'member@test','Member','member',1,1),
		(3,'demoted@test','Demoted','admin',1,1);`)
	if introduced, err := systems.Introduced(ctx, d.Read); err != nil || introduced {
		t.Fatalf("unnamed archive introduced=%v err=%v", introduced, err)
	}
	exec(t, d, `UPDATE users SET role='member' WHERE id=3`)
	for _, id := range []int64{1, 2, 3} {
		if allowed, err := systems.CanEnter(ctx, d.Read, id, systems.DefaultID); err != nil || !allowed {
			t.Fatalf("unnamed archive access for %d: %v %v", id, allowed, err)
		}
	}
	if err := d.WriteTx(ctx, func(tx *sql.Tx) error {
		if err := systems.SetCode(ctx, tx, systems.DefaultID, "S01", 2); err != nil {
			return err
		}
		_, err := systems.Create(ctx, tx, "S02", "Advisory", "jd", 2)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if introduced, err := systems.Introduced(ctx, d.Read); err != nil || !introduced {
		t.Fatalf("named archive introduced=%v err=%v", introduced, err)
	}
	second, err := systems.ByCode(ctx, d.Read, "S02")
	if err != nil || second.Name != "Advisory" || second.InboxCategoryID != 0 {
		t.Fatalf("new system=%+v err=%v", second, err)
	}
	if _, err := systems.ByCode(ctx, d.Read, "s02"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("malformed code lookup=%v", err)
	}
	for _, tc := range []struct {
		user, system int64
		want         bool
	}{
		{1, second.ID, true}, {2, second.ID, false}, {3, second.ID, false},
		{1, 999, false}, {999, second.ID, false}, {0, systems.DefaultID, false},
	} {
		if got, err := systems.CanEnter(ctx, d.Read, tc.user, tc.system); err != nil || got != tc.want {
			t.Fatalf("entry user=%d system=%d got=%v want=%v err=%v", tc.user, tc.system, got, tc.want, err)
		}
	}
	exec(t, d, `INSERT INTO users(id,email,display_name,role,created_at,updated_at) VALUES
		(4,'new@test','New member','member',3,3),(5,'later-admin@test','Later admin','admin',3,3);
		UPDATE users SET role='member' WHERE id=5;`)
	count(t, d, `SELECT count(*) FROM jd_system_members WHERE user_id IN (4,5)`, 0)
	exec(t, d, `INSERT INTO jd_system_members(system_id,user_id,created_at) VALUES(2,2,3);
		INSERT INTO api_tokens(system_id,user_id,name,token_hash,scopes,created_at) VALUES(2,2,'S02 token','token','documents:read',3);
		INSERT INTO mobile_pairings(system_id,user_id,code_hash,name,expires_at) VALUES(2,2,printf('%064d',2),'Pending',9999999999);
		INSERT INTO share_links(system_id,token,doc_ids_json,created_by,created_at) VALUES(2,'share','[]',2,3);
		UPDATE users SET disabled=1 WHERE id=2;`)
	if allowed, err := systems.CanEnter(ctx, d.Read, 2, second.ID); err != nil || allowed {
		t.Fatalf("disabled member entered: %v %v", allowed, err)
	}
	exec(t, d, `UPDATE users SET disabled=0 WHERE id=2;
		DELETE FROM jd_system_members WHERE system_id=2 AND user_id=2;
		INSERT INTO jd_system_members(system_id,user_id,created_at) VALUES(2,2,4);`)
	if allowed, err := systems.CanEnter(ctx, d.Read, 2, second.ID); err != nil || !allowed {
		t.Fatalf("restored membership: %v %v", allowed, err)
	}
	count(t, d, `SELECT count(*) FROM api_tokens WHERE user_id=2`, 0)
	count(t, d, `SELECT count(*) FROM mobile_pairings WHERE user_id=2`, 0)
	count(t, d, `SELECT count(*) FROM share_links WHERE created_by=2 AND revoked_at IS NULL`, 0)
	exec(t, d, `UPDATE users SET disabled=1 WHERE id=1`)
	if allowed, err := systems.CanEnter(ctx, d.Read, 1, second.ID); err != nil || allowed {
		t.Fatalf("disabled administrator entered: %v %v", allowed, err)
	}
}

func TestNamespaceIntegrityAndStableCodes(t *testing.T) {
	d := openSystems(t)
	exec(t, d, `UPDATE jd_systems SET code='S01' WHERE id=1;
		INSERT INTO jd_systems(id,code,name,taxonomy,created_at,updated_at) VALUES(2,'S02','Advisory','jd',1,1);
		INSERT INTO users(id,email,display_name,role,created_at,updated_at) VALUES(1,'owner@test','Owner','admin',1,1);
		INSERT INTO jd_areas(system_id,code_start,code_end,name,position) VALUES(1,10,19,'Work',1),(2,10,19,'Work',1);
		INSERT INTO jd_categories(system_id,id,area_start,code,name,system) VALUES(1,1,10,13,'Tax',1),(2,2,10,13,'Tax',1);
		UPDATE jd_systems SET inbox_category_id=id;
		INSERT INTO documents(system_id,id,owner_id,original_blob,original_size,jd_category_id,created_at,updated_at,legacy_id,archive_serial_number)
			VALUES(1,147,1,'same-blob',1,1,1,1,17,19),(2,148,1,'same-blob',1,2,1,1,17,19);
		INSERT INTO tags(system_id,id,name,slug,created_at,updated_at) VALUES(1,1,'Tax','tax',1,1),(2,2,'Tax','tax',1,1);
		INSERT INTO correspondents(system_id,id,name,slug,created_at,updated_at) VALUES(1,1,'Client','client',1,1),(2,2,'Client','client',1,1);
		INSERT INTO document_types(system_id,id,name,slug,created_at,updated_at) VALUES(1,1,'Return','return',1,1),(2,2,'Return','return',1,1);
		INSERT INTO storage_paths(system_id,id,name,slug,path,created_at,updated_at) VALUES(1,1,'Records','records','records',1,1),(2,2,'Records','records','records',1,1);
		INSERT INTO custom_fields(system_id,id,name,data_type,created_at,updated_at) VALUES(1,1,'Related','documentlink',1,1),(2,2,'Related','documentlink',1,1);
		INSERT INTO approval_defs(system_id,id,slug,version,spec_json,created_at) VALUES(1,1,'change',1,'{}',1),(2,2,'change',1,'{}',1);
		INSERT INTO automations(system_id,name,created_at,updated_at) VALUES(1,'File tax',1,1),(2,'File tax',1,1);
		INSERT INTO document_tags(document_id,tag_id) VALUES(147,1);
		INSERT INTO document_correspondents(document_id,correspondent_id,role) VALUES(147,1,'sender');
		INSERT INTO document_custom_field_values(document_id,field_id,value_int) VALUES(147,1,148);`)
	// A cross-system document-link value is intentionally representable; only
	// the authenticated API can authorize its creation. Field identity itself
	// must still belong to the source system.
	for _, query := range []string{
		`INSERT OR REPLACE INTO jd_systems(id,code,name,taxonomy,created_at,updated_at) VALUES(1,'S03','Replacement','jd',1,1)`,
		`INSERT OR REPLACE INTO jd_systems(id,code,name,taxonomy,created_at,updated_at) VALUES(3,'S01','Replacement','jd',1,1)`,
		`UPDATE jd_systems SET code='S03' WHERE id=1`,
		`UPDATE jd_systems SET code='' WHERE id=1`,
		`DELETE FROM jd_systems WHERE id=2`,
		`UPDATE jd_systems SET inbox_category_id=2 WHERE id=1`,
		`UPDATE jd_categories SET system=0 WHERE id=1`,
		`UPDATE documents SET system_id=2 WHERE id=147`,
		`UPDATE documents SET jd_category_id=2 WHERE id=147`,
		`UPDATE documents SET correspondent_id=2 WHERE id=147`,
		`UPDATE documents SET document_type_id=2 WHERE id=147`,
		`UPDATE documents SET storage_path_id=2 WHERE id=147`,
		`UPDATE documents SET previous_version_id=148 WHERE id=147`,
		`UPDATE documents SET split_parent_id=148 WHERE id=147`,
		`UPDATE documents SET split_origin_id=148 WHERE id=147`,
		`UPDATE documents SET email_parent_id=148 WHERE id=147`,
		`UPDATE tags SET parent_id=2 WHERE id=1`,
		`UPDATE document_tags SET tag_id=2 WHERE document_id=147`,
		`UPDATE document_correspondents SET correspondent_id=2 WHERE document_id=147`,
		`UPDATE document_custom_field_values SET field_id=2 WHERE document_id=147`,
		`INSERT INTO approval_runs(system_id,def_id,doc_id,state,current_state,state_entered_at,started_at) VALUES(1,2,147,'running','start',1,1)`,
		`INSERT INTO approval_runs(system_id,def_id,doc_id,state,current_state,state_entered_at,started_at) VALUES(1,1,148,'running','start',1,1)`,
		`INSERT INTO jobs(system_id,kind,doc_id,next_run_at,created_at,updated_at) VALUES(2,'render',147,1,1,1)`,
		`INSERT INTO share_links(system_id,token,doc_ids_json,created_by,created_at) VALUES(1,'bad','[147,148]',1,1)`,
		`INSERT INTO documents(system_id,owner_id,original_blob,original_size,jd_category_id,created_at,updated_at) VALUES(1,1,'same-blob',1,1,1,1)`,
		`INSERT INTO documents(owner_id,original_blob,original_size,jd_category_id,created_at,updated_at) VALUES(1,'omitted-system',1,1,1,1)`,
	} {
		if _, err := d.ExecWrite(t.Context(), query); err == nil {
			t.Errorf("accepted invalid namespace operation: %s", query)
		}
	}
	// Nullable reference deletion must not attempt to null immutable ownership.
	exec(t, d, `UPDATE documents SET correspondent_id=1,document_type_id=1,storage_path_id=1 WHERE id=147;
		DELETE FROM correspondents WHERE id=1;
		DELETE FROM document_types WHERE id=1;
		DELETE FROM storage_paths WHERE id=1;`)
	count(t, d, `SELECT count(*) FROM documents WHERE id=147 AND system_id=1 AND correspondent_id IS NULL AND document_type_id IS NULL AND storage_path_id IS NULL`, 1)
}
