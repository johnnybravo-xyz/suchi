package importer_test

import (
	"errors"
	"testing"

	"github.com/johnnybravo-xyz/suchi/core/automations"
	"github.com/johnnybravo-xyz/suchi/core/jd/importer"
	"github.com/johnnybravo-xyz/suchi/core/jd/systems"
	"github.com/johnnybravo-xyz/suchi/core/render/view"
	"github.com/johnnybravo-xyz/suchi/core/taxonomy/exporter"
)

func TestFirstSystemImportRetainsIdentityMembershipAndGrants(t *testing.T) {
	d := openTestDB(t)
	pf := smallPreset(t)
	apply(t, d, pf, importer.Options{})
	id := document(t, d, "Retained invoice", "electricity")
	if _, err := d.ExecWrite(t.Context(), `INSERT INTO users(id,email,display_name,role,created_at,updated_at) VALUES(2,'member@test','Member','member',0,0);
	INSERT INTO object_acls(object_kind,object_id,principal_kind,principal_id,perm_bits,created_at) VALUES('document',?,'user',2,1,0)`, id); err != nil {
		t.Fatal(err)
	}
	pf.System, pf.Name = "S01", "Sharma Audit"
	preview, err := importer.Preview(t.Context(), d, pf, importer.Options{})
	if err != nil {
		t.Fatal(err)
	}
	before, err := systems.Get(t.Context(), d.Read, 1)
	if err != nil {
		t.Fatal(err)
	}
	if before.Code != "" || preview.SystemCode != "S01" || preview.SystemCreated || !preview.SystemsIntroduced {
		t.Fatalf("preview named archive or has wrong destination: %+v %+v", before, preview)
	}
	if _, err := importer.Apply(t.Context(), d, logger(), pf, importer.Options{ExpectedStateHash: preview.StateHash}); err != nil {
		t.Fatal(err)
	}
	after, err := systems.Get(t.Context(), d.Read, 1)
	if err != nil {
		t.Fatal(err)
	}
	if after.Code != "S01" || after.Name != "Sharma Audit" {
		t.Fatalf("wrong adoption: %+v", after)
	}
	if n := count(t, d, `SELECT COUNT(*) FROM documents WHERE id=? AND system_id=1 AND title='Retained invoice'`, id); n != 1 {
		t.Fatal("document identity lost")
	}
	if n := count(t, d, `SELECT COUNT(*) FROM jd_system_members WHERE system_id=1 AND user_id=2`); n != 1 {
		t.Fatal("membership lost")
	}
	if n := count(t, d, `SELECT COUNT(*) FROM object_acls WHERE object_kind='document' AND object_id=? AND principal_id=2 AND perm_bits=1`, id); n != 1 {
		t.Fatal("document grant lost")
	}
	if n := count(t, d, `SELECT COUNT(*) FROM jobs WHERE kind=? AND doc_id=?`, view.Kind, id); n != 1 {
		t.Fatal("adoption did not queue render-only relocation")
	}
	if _, err := d.ExecWrite(t.Context(), `UPDATE jd_systems SET name='Locally named audit' WHERE id=1`); err != nil {
		t.Fatal(err)
	}
	pf.Name = "Upstream changed name"
	apply(t, d, pf, importer.Options{})
	pf.System = ""
	apply(t, d, pf, importer.Options{})
	exported, err := exporter.BuildExport(t.Context(), d, 1, true)
	if err != nil {
		t.Fatal(err)
	}
	if exported.System != "S01" || exported.Name != "Locally named audit" {
		t.Fatalf("reimport removed identity or local name: %+v", exported)
	}
	if _, err := d.ExecWrite(t.Context(), `INSERT INTO users(id,email,display_name,role,created_at,updated_at) VALUES(3,'new@test','New','member',0,0)`); err != nil {
		t.Fatal(err)
	}
	if n := count(t, d, `SELECT COUNT(*) FROM jd_system_members WHERE user_id=3`); n != 0 {
		t.Fatal("new user implicitly admitted after introduction")
	}
}

func TestFirstSystemSeparateDestinationAndFailedTransition(t *testing.T) {
	for _, fail := range []bool{true, false} {
		t.Run(map[bool]string{true: "rollback", false: "preserve"}[fail], func(t *testing.T) {
			d := openTestDB(t)
			apply(t, d, smallPreset(t), importer.Options{})
			id := document(t, d, "Existing archive", "")
			pf := smallPreset(t)
			pf.System, pf.Name = "S01", "New firm"
			opts := importer.Options{ExistingSystemCode: "A00"}
			preview, err := importer.Preview(t.Context(), d, pf, opts)
			if err != nil {
				t.Fatal(err)
			}
			if !preview.SystemCreated || preview.ExistingSystemCode != "A00" {
				t.Fatalf("wrong separate preview: %+v", preview)
			}
			if n := count(t, d, `SELECT COUNT(*) FROM jd_systems WHERE code<>''`); n != 0 {
				t.Fatal("preview wrote system records")
			}
			if fail {
				if _, err := d.ExecWrite(t.Context(), `CREATE TRIGGER fail_new_rule BEFORE INSERT ON automation_actions BEGIN SELECT RAISE(ABORT,'reject rule'); END`); err != nil {
					t.Fatal(err)
				}
			}
			opts.ExpectedStateHash = preview.StateHash
			_, err = importer.Apply(t.Context(), d, logger(), pf, opts)
			if fail {
				if err == nil {
					t.Fatal("injected rule failure ignored")
				}
				if n := count(t, d, `SELECT COUNT(*) FROM jd_systems WHERE code<>''`); n != 0 {
					t.Fatal("failed apply left named or new system")
				}
				if n := count(t, d, `SELECT COUNT(*) FROM jd_categories WHERE system_id<>1`); n != 0 {
					t.Fatal("failed apply left foreign tree")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			original, err := systems.Get(t.Context(), d.Read, 1)
			if err != nil {
				t.Fatal(err)
			}
			created, err := systems.ByCode(t.Context(), d.Read, "S01")
			if err != nil {
				t.Fatal(err)
			}
			if original.Code != "A00" || original.Name == "New firm" || created.ID == 1 || created.Name != "New firm" || created.InboxCategoryID == 0 {
				t.Fatalf("wrong preservation: %+v %+v", original, created)
			}
			if n := count(t, d, `SELECT COUNT(*) FROM documents WHERE id=? AND system_id=1`, id); n != 1 {
				t.Fatal("separate import moved document")
			}
			if n := count(t, d, `SELECT COUNT(*) FROM jd_system_members WHERE system_id=?`, created.ID); n != 0 {
				t.Fatal("new system implicitly admitted members")
			}
			if n := count(t, d, `SELECT COUNT(*) FROM approval_defs WHERE system_id=? AND active=1`, created.ID); n != 2 {
				t.Fatal("new system missing builtin workflows")
			}
		})
	}
}

func TestSystemPreviewIsolationAndCodeCreationRace(t *testing.T) {
	d := openTestDB(t)
	pf := smallPreset(t)
	pf.System = "S01"
	apply(t, d, pf, importer.Options{})
	other := smallPreset(t)
	other.System = "S02"
	apply(t, d, other, importer.Options{})
	preview, err := importer.Preview(t.Context(), d, pf, importer.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.ExecWrite(t.Context(), `UPDATE jd_categories SET description='Other cabinet edit' WHERE system_id=(SELECT id FROM jd_systems WHERE code='S02'); UPDATE automations SET enabled=0 WHERE system_id=(SELECT id FROM jd_systems WHERE code='S02')`); err != nil {
		t.Fatal(err)
	}
	if _, err := importer.Apply(t.Context(), d, logger(), pf, importer.Options{ExpectedStateHash: preview.StateHash}); err != nil {
		t.Fatalf("unrelated system edit staled preview: %v", err)
	}
	third := smallPreset(t)
	third.System = "S03"
	preview, err = importer.Preview(t.Context(), d, third, importer.Options{})
	if err != nil {
		t.Fatal(err)
	}
	apply(t, d, third, importer.Options{})
	if _, err := importer.Apply(t.Context(), d, logger(), third, importer.Options{ExpectedStateHash: preview.StateHash}); !errors.Is(err, importer.ErrStalePreview) {
		t.Fatalf("conflicting creation accepted: %v", err)
	}
}

func TestImportRechecksActorAndTargetMembershipState(t *testing.T) {
	for _, change := range []string{"disable", "demote", "membership"} {
		t.Run(change, func(t *testing.T) {
			d := openTestDB(t)
			pf := smallPreset(t)
			apply(t, d, pf, importer.Options{})
			document(t, d, "Actor", "")
			pf.System = "S01"
			opts := importer.Options{ActorID: 1}
			preview, err := importer.Preview(t.Context(), d, pf, opts)
			if err != nil {
				t.Fatal(err)
			}
			switch change {
			case "disable":
				_, err = d.ExecWrite(t.Context(), `UPDATE users SET disabled=1 WHERE id=1`)
			case "demote":
				_, err = d.ExecWrite(t.Context(), `UPDATE users SET role='member' WHERE id=1`)
			case "membership":
				_, err = d.ExecWrite(t.Context(), `INSERT INTO jd_system_members(system_id,user_id,created_at) VALUES(1,1,0)`)
			}
			if err != nil {
				t.Fatal(err)
			}
			opts.ExpectedStateHash = preview.StateHash
			if _, err := importer.Apply(t.Context(), d, logger(), pf, opts); err == nil {
				t.Fatal("changed authority/preview accepted")
			}
			if n := count(t, d, `SELECT COUNT(*) FROM jd_systems WHERE code<>''`); n != 0 {
				t.Fatal("rejected import named system")
			}
		})
	}
}

func TestRepeatedCodesNamesAndRulesStayInTheirSystem(t *testing.T) {
	d := openTestDB(t)
	pf := smallPreset(t)
	pf.System = "S01"
	apply(t, d, pf, importer.Options{})
	id := document(t, d, "Invoice", "electricity")
	other := smallPreset(t)
	other.System = "S02"
	apply(t, d, other, importer.Options{})
	if _, err := d.ExecWrite(t.Context(), `UPDATE automations SET enabled=0 WHERE system_id=1`); err != nil {
		t.Fatal(err)
	}
	if err := automations.ApplyOnDocumentAdded(t.Context(), d, logger(), id); err != nil {
		t.Fatal(err)
	}
	if code := count(t, d, `SELECT c.code FROM documents d JOIN jd_categories c ON c.id=d.jd_category_id WHERE d.id=?`, id); code != 49 {
		t.Fatal("foreign system rule classified document")
	}
	if n := count(t, d, `SELECT COUNT(*) FROM document_tags WHERE document_id=?`, id); n != 0 {
		t.Fatal("foreign system rule tagged document")
	}
	if n := count(t, d, `SELECT COUNT(*) FROM tags WHERE name='Tax'`); n != 2 {
		t.Fatal("same-name starters collided across systems")
	}
	selected, err := systems.ByCode(t.Context(), d.Read, "S02")
	if err != nil {
		t.Fatal(err)
	}
	other.System = ""
	other.Areas[0].Categories[0].Description = "Incoming description is not a rename"
	apply(t, d, other, importer.Options{TargetSystem: "S02"})
	if n := count(t, d, `SELECT COUNT(*) FROM automations WHERE system_id=1 AND enabled=1`); n != 0 {
		t.Fatal("selected unprefixed reimport resurrected another system rules")
	}
	exported, err := exporter.BuildExport(t.Context(), d, selected.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	if exported.System != "S02" || len(exported.Seeds.Automations) != 2 {
		t.Fatalf("wrong scoped export: %+v", exported)
	}
}
