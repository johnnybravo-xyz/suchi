package importer_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"path/filepath"
	"testing"

	"github.com/johnnybravo-xyz/suchi/core/automations"
	"github.com/johnnybravo-xyz/suchi/core/db"
	migrations "github.com/johnnybravo-xyz/suchi/core/db/migrations"
	"github.com/johnnybravo-xyz/suchi/core/jd/importer"
	"github.com/johnnybravo-xyz/suchi/core/jd/presetfile"
)

const smallTaxonomy = `format = "suchi-taxonomy/v1"
id = "smalltest"
version = 1
name = "Small test"
market = "global"
language = "en"
story = "File utility invoices while keeping unrelated paperwork in Inbox."
[[areas]]
code = 10
name = "Life"
[[areas.categories]]
code = 11
name = "Bills"
description = "Utility bills."
keywords = ["electricity bill", "gas bill"]
[[seeds.automations]]
name = "File utility invoices"
[seeds.automations.trigger]
type = 2
filter_title_matching = "Invoice"
filter_content_matching = "electricity"
[[seeds.automations.actions]]
kind = "assign_jd_category"
[seeds.automations.actions.params]
jd_category_code = 11
[[seeds.automations.actions]]
kind = "assign_tags"
[seeds.automations.actions.params]
tags = ["Tax"]
`

func smallPreset(t *testing.T) *presetfile.PresetFile {
	t.Helper()
	pf, err := presetfile.Parse([]byte(smallTaxonomy), presetfile.FormatTOML)
	if err != nil {
		t.Fatal(err)
	}
	return pf
}
func logger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }
func openTestDB(t *testing.T) *db.DB {
	t.Helper()
	d, err := db.Open(t.Context(), filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	migs, err := db.LoadMigrations(migrations.FS, ".")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Migrate(t.Context(), d, migs, logger()); err != nil {
		t.Fatal(err)
	}
	return d
}
func apply(t *testing.T, d *db.DB, pf *presetfile.PresetFile, opts importer.Options) *importer.Diff {
	t.Helper()
	diff, err := importer.ImportForDB(t.Context(), d, logger(), pf, opts)
	if err != nil {
		t.Fatal(err)
	}
	return diff
}
func count(t *testing.T, d *db.DB, query string, args ...any) int {
	t.Helper()
	var n int
	if err := d.Read.QueryRowContext(t.Context(), query, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}
func document(t *testing.T, d *db.DB, title, content string) int64 {
	t.Helper()
	if _, err := d.Write.ExecContext(t.Context(), `INSERT INTO users(id,email,display_name,role,created_at,updated_at) VALUES(1,'test@example.test','Test','admin',0,0) ON CONFLICT DO NOTHING`); err != nil {
		t.Fatal(err)
	}
	res, err := d.Write.ExecContext(t.Context(), `INSERT INTO documents(system_id,owner_id,title,content,original_blob,original_size,jd_category_id,created_at,added_at,updated_at) VALUES(1,1,?,?,?,1,(SELECT id FROM jd_categories WHERE system_id=1 AND system=1),0,0,0)`, title, content, title)
	if err != nil {
		t.Fatal(err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestImportedRulesExecuteLiteralBoundariesAndActions(t *testing.T) {
	d := openTestDB(t)
	apply(t, d, smallPreset(t), importer.Options{})
	for _, tc := range []struct {
		title, content string
		code           int
		tagged         bool
	}{
		{"ordinary", "Your electricity bill.", 11, false},
		{"unrelated", "electricity billing", 49, false},
		{"Invoice", "electricity", 11, true},
	} {
		id := document(t, d, tc.title, tc.content)
		if err := automations.ApplyOnDocumentAdded(t.Context(), d, logger(), id); err != nil {
			t.Fatal(err)
		}
		code := count(t, d, `SELECT c.code FROM documents d JOIN jd_categories c ON c.id=d.jd_category_id WHERE d.id=?`, id)
		if code != tc.code {
			t.Fatalf("%q filed as %d, want %d", tc.content, code, tc.code)
		}
		tags := count(t, d, `SELECT COUNT(*) FROM document_tags dt JOIN tags t ON t.id=dt.tag_id WHERE dt.document_id=? AND t.name='Tax'`, id)
		if (tags == 1) != tc.tagged {
			t.Fatalf("%q Tax tag count=%d", tc.title, tags)
		}
	}
}

func TestReimportPreservesDisabledOriginalsAndUserFork(t *testing.T) {
	d := openTestDB(t)
	pf := smallPreset(t)
	apply(t, d, pf, importer.Options{})
	store := automations.New(d)
	rules, err := store.List(t.Context(), 1)
	if err != nil {
		t.Fatal(err)
	}
	var originalID, keywordID int64
	for _, r := range rules {
		if r.Name == "File utility invoices" {
			originalID = r.ID
		} else {
			keywordID = r.ID
		}
	}
	falseValue := false
	if _, err := store.Update(t.Context(), 1, keywordID, automations.AutomationPatch{Enabled: &falseValue}); err != nil {
		t.Fatal(err)
	}
	newTitle := []automations.Action{{Kind: "assign_title", Params: map[string]any{"template": "Reviewed invoice"}}}
	fork, err := store.Update(t.Context(), 1, originalID, automations.AutomationPatch{Actions: &newTitle})
	if err != nil {
		t.Fatal(err)
	}
	if fork.ID == originalID || fork.PresetSlug != "" {
		t.Fatalf("expected a user-owned fork, got %+v", fork)
	}
	if _, err := d.Write.ExecContext(t.Context(), `UPDATE jd_categories SET description='My local filing note' WHERE code=11`); err != nil {
		t.Fatal(err)
	}
	diff := apply(t, d, pf, importer.Options{})
	if diff.Mode != "merge" || len(diff.RulesToAdd) != 0 || len(diff.RulesPreserved) != 2 {
		t.Fatalf("reimport effects=%+v", diff)
	}
	for _, id := range []int64{originalID, keywordID} {
		rule, err := store.Get(t.Context(), 1, id)
		if err != nil {
			t.Fatal(err)
		}
		if rule.Enabled {
			t.Fatalf("disabled rule %d resurrected", id)
		}
	}
	id := document(t, d, "Invoice", "electricity bill")
	if err := automations.ApplyOnDocumentAdded(t.Context(), d, logger(), id); err != nil {
		t.Fatal(err)
	}
	var title, description string
	if err := d.Read.QueryRowContext(t.Context(), `SELECT title FROM documents WHERE id=?`, id).Scan(&title); err != nil {
		t.Fatal(err)
	}
	if title != "Reviewed invoice" {
		t.Fatalf("fork no longer controls title: %q", title)
	}
	if code := count(t, d, `SELECT c.code FROM documents d JOIN jd_categories c ON c.id=d.jd_category_id WHERE d.id=?`, id); code != 49 {
		t.Fatalf("disabled filing behavior returned: category %d", code)
	}
	if err := d.Read.QueryRowContext(t.Context(), `SELECT description FROM jd_categories WHERE code=11`).Scan(&description); err != nil {
		t.Fatal(err)
	}
	if description != "My local filing note" {
		t.Fatalf("local description replaced: %q", description)
	}
}

func TestSkipSeedsStillValidatesWholeFileAndCanApplyLater(t *testing.T) {
	d := openTestDB(t)
	pf := smallPreset(t)
	pf.Seeds.Automations[0].Actions[0].Params["document_id"] = 1
	if _, err := importer.ImportForDB(t.Context(), d, logger(), pf, importer.Options{SkipSeeds: true}); err == nil {
		t.Fatal("unsupported action parameter accepted with skipped seeds")
	}
	if n := count(t, d, `SELECT COUNT(*) FROM jd_areas`); n != 0 {
		t.Fatal("invalid file changed tree")
	}
	pf = smallPreset(t)
	apply(t, d, pf, importer.Options{SkipSeeds: true, ContentSHA256: "same-bytes"})
	if n := count(t, d, `SELECT COUNT(*) FROM automations`); n != 0 {
		t.Fatal("tree-only import installed behavior")
	}
	diff := apply(t, d, pf, importer.Options{ContentSHA256: "same-bytes"})
	if len(diff.RulesToAdd) != 2 {
		t.Fatalf("same file with seeds requested was treated as no-op: %+v", diff)
	}
	var metadata string
	if err := d.Read.QueryRowContext(context.Background(), `SELECT authoring_json FROM jd_systems WHERE id=1`).Scan(&metadata); err != nil {
		t.Fatal(err)
	}
	var header map[string]any
	if err := json.Unmarshal([]byte(metadata), &header); err != nil {
		t.Fatal(err)
	}
	if header["story"] != pf.Story || header["language"] != "en" {
		t.Fatalf("author metadata lost: %v", header)
	}
}
