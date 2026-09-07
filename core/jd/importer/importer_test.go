package importer_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"testing"

	"github.com/johnnybravo-xyz/suchi/core/db"
	migrations "github.com/johnnybravo-xyz/suchi/core/db/migrations"
	"github.com/johnnybravo-xyz/suchi/core/jd/importer"
	"github.com/johnnybravo-xyz/suchi/core/jd/presetfile"
)

func TestApplyReplace_SeedsTreeKeywordsAndAutomations(t *testing.T) {
	d := openTestDB(t)
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	pf := smallPreset()

	res, err := importer.ImportForDB(context.Background(), d, log, pf, importer.Options{})
	if err != nil {
		t.Fatal(err)
	}

	if res.AreasSeeded != 2 || res.CategoriesSeeded != 3 {
		t.Fatalf("tree seed count wrong: %+v", res)
	}
	// Two keywords are grouped into one preset-owned automation.
	if res.KeywordsSeeded != 2 {
		t.Fatalf("keywords seeded: got %d, want 2", res.KeywordsSeeded)
	}
	if res.AutomationsSeeded != 1 {
		t.Fatalf("automations seeded: got %d, want 1", res.AutomationsSeeded)
	}

	assertQueryEquals(t, d, `SELECT COUNT(*) FROM automations WHERE preset_slug = ?`,
		[]any{pf.ID}, 2)
	var pattern string
	if err := d.Read.QueryRow(`SELECT filter_content_re FROM automation_triggers WHERE automation_id = (SELECT id FROM automations WHERE name = 'smalltest: file 11 Bills')`).Scan(&pattern); err != nil {
		t.Fatal(err)
	}
	re := regexp.MustCompile("(?i)" + pattern)
	if !re.MatchString("Your electricity bill.") || re.MatchString("electricity billing") {
		t.Fatalf("preset keyword pattern does not respect word boundaries: %q", pattern)
	}

	// jd_category_code → jd_category_id resolved.
	var resolved int
	err = d.Read.QueryRowContext(context.Background(), `
		SELECT COUNT(*) FROM automation_actions
		WHERE kind = 'assign_jd_category'
		  AND automation_id = (SELECT id FROM automations WHERE name = 'File utility bills')
	`).Scan(&resolved)
	if err != nil {
		t.Fatal(err)
	}
	if resolved != 1 {
		t.Fatalf("resolved actions: got %d, want 1", resolved)
	}
	var rawParams string
	if err := d.Read.QueryRowContext(context.Background(),
		`SELECT aa.params_json FROM automation_actions aa
		 JOIN automations a ON a.id = aa.automation_id
		 WHERE a.name = 'File utility bills' LIMIT 1`).Scan(&rawParams); err != nil {
		t.Fatal(err)
	}
	var params map[string]any
	if err := json.Unmarshal([]byte(rawParams), &params); err != nil {
		t.Fatal(err)
	}
	if _, exists := params["tags"]; exists {
		t.Fatalf("symbolic tags leaked into stored params: %s", rawParams)
	}
	if _, exists := params["tag_ids"]; !exists {
		t.Fatalf("resolved tag_ids missing from stored params: %s", rawParams)
	}
	assertQueryEquals(t, d, `
		SELECT COUNT(*)
		FROM automation_triggers tr
		JOIN automations a ON a.id = tr.automation_id
		JOIN tags t ON t.id = tr.filter_tag_id
		JOIN correspondents c ON c.id = tr.filter_corr_id
		JOIN document_types dt ON dt.id = tr.filter_doctype_id
		WHERE a.name = 'File utility bills'
		  AND t.name = 'utilities'
		  AND c.name = 'BESCOM'
		  AND dt.name = 'Utility bill'
		  AND tr.filter_title_re = '(?i)invoice'
	`, nil, 1)
}

func TestApplyReplace_ClearsPriorPresetOwnedRows(t *testing.T) {
	d := openTestDB(t)
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))

	// Seed preset "one".
	pfOne := smallPreset()
	pfOne.ID = "one"
	if _, err := importer.ImportForDB(context.Background(), d, log, pfOne, importer.Options{}); err != nil {
		t.Fatal(err)
	}
	// User-owned automation survives re-apply.
	if _, err := d.Write.ExecContext(context.Background(), `
		INSERT INTO automations(name, order_index, enabled, created_at, updated_at)
		VALUES ('user-authored', 100, 1, 0, 0)
	`); err != nil {
		t.Fatal(err)
	}

	// Clear the JD tree between preset applies (in production this is
	// applyPreset's responsibility; the importer is called from within
	// its write-tx). Preset automations are wiped by ApplyReplace.
	if _, err := d.Write.ExecContext(context.Background(), `DELETE FROM jd_categories`); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Write.ExecContext(context.Background(), `DELETE FROM jd_areas`); err != nil {
		t.Fatal(err)
	}
	// Now seed preset "two" — same DB, different slug.
	pfTwo := smallPreset()
	pfTwo.ID = "two"
	if _, err := importer.ImportForDB(context.Background(), d, log, pfTwo, importer.Options{}); err != nil {
		t.Fatal(err)
	}

	assertQueryEquals(t, d, `SELECT COUNT(*) FROM automations WHERE preset_slug = ?`, []any{"one"}, 0)
	assertQueryEquals(t, d, `SELECT COUNT(*) FROM automations WHERE preset_slug = ?`, []any{"two"}, 2)
	assertQueryEquals(t, d, `SELECT COUNT(*) FROM automations WHERE preset_slug IS NULL AND name = ?`,
		[]any{"user-authored"}, 1)
}

func TestApplyReplace_SkipSeeds(t *testing.T) {
	d := openTestDB(t)
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	res, err := importer.ImportForDB(context.Background(), d, log, smallPreset(),
		importer.Options{SkipSeeds: true})
	if err != nil {
		t.Fatal(err)
	}
	if res.KeywordsSeeded != 0 || res.AutomationsSeeded != 0 {
		t.Fatalf("SkipSeeds should skip keyword and explicit automations: %+v", res)
	}
	if res.AreasSeeded != 2 || res.CategoriesSeeded != 3 {
		t.Fatalf("tree still seeds: %+v", res)
	}
}

// smallPreset returns a tiny valid PresetFile with 2 areas, 3
// categories (one is inbox), 2 keywords, 1 seed automation using
// jd_category_code symbol resolution.
func smallPreset() *presetfile.PresetFile {
	return &presetfile.PresetFile{
		Format:  presetfile.Format,
		ID:      "smalltest",
		Version: 1,
		Name:    "Small test",
		Story:   "test",
		Inbox:   49,
		Areas: []presetfile.Area{
			{Code: 10, Name: "Life", Categories: []presetfile.Category{
				{Code: 11, Name: "Bills", Keywords: []string{"electricity bill", "gas bill"}},
			}},
			{Code: 40, Name: "System", Categories: []presetfile.Category{
				{Code: 40, Name: "meta"},
				{Code: 49, Name: "Inbox"},
			}},
		},
		Seeds: &presetfile.Seeds{
			Automations: []presetfile.SeedAutomation{{
				Name: "File utility bills",
				Trigger: presetfile.Trigger{
					Type:                  2,
					FilterTitleMatching:   "(?i)invoice",
					FilterContentMatching: "electricity|gas bill",
					FilterTag:             "utilities",
					FilterCorrespondent:   "BESCOM",
					FilterDocumentType:    "Utility bill",
				},
				Actions: []presetfile.Action{{
					Kind: "assign_jd_category",
					Params: map[string]any{
						"jd_category_code": 11,
						"tags":             []string{"Tax"},
					},
				}},
			}},
		},
	}
}

func openTestDB(t *testing.T) *db.DB {
	t.Helper()
	ctx := context.Background()
	d, err := db.Open(ctx, filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	migs, err := db.LoadMigrations(migrations.FS, ".")
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	if err := db.Migrate(ctx, d, migs, log); err != nil {
		t.Fatal(err)
	}
	return d
}

func assertQueryEquals(t *testing.T, d *db.DB, q string, args []any, want int) {
	t.Helper()
	var got int
	if err := d.Read.QueryRowContext(context.Background(), q, args...).Scan(&got); err != nil {
		if err == sql.ErrNoRows {
			got = 0
		} else {
			t.Fatal(err)
		}
	}
	if got != want {
		t.Errorf("query %q got %d, want %d", q, got, want)
	}
}
