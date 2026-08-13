package importer_test

import (
	"context"
	"database/sql"
	"log/slog"
	"os"
	"path/filepath"
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
	// Two keyword rules — one per Keywords entry.
	if res.KeywordsSeeded != 2 {
		t.Fatalf("keywords seeded: got %d, want 2", res.KeywordsSeeded)
	}
	if res.AutomationsSeeded != 1 {
		t.Fatalf("automations seeded: got %d, want 1", res.AutomationsSeeded)
	}

	// Preset rules carry preset_slug.
	assertQueryEquals(t, d, `SELECT COUNT(*) FROM rules WHERE preset_slug = ?`,
		[]any{pf.ID}, 2)
	assertQueryEquals(t, d, `SELECT COUNT(*) FROM automations WHERE preset_slug = ?`,
		[]any{pf.ID}, 1)

	// jd_category_code → jd_category_id resolved.
	var resolved int
	err = d.Read.QueryRowContext(context.Background(), `
		SELECT COUNT(*) FROM automation_actions
		WHERE params_json LIKE '%jd_category_id%'
	`).Scan(&resolved)
	if err != nil {
		t.Fatal(err)
	}
	if resolved != 1 {
		t.Fatalf("resolved actions: got %d, want 1", resolved)
	}
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
	// User-owned rule (survives re-apply).
	if _, err := d.Write.ExecContext(context.Background(), `
		INSERT INTO rules(name, if_kind, if_value, then_kind, then_value,
		                  priority, enabled, created_at, updated_at)
		VALUES ('user-authored', 'title_contains', 'x', 'add_tag', 'y', 100, 1, 0, 0)
	`); err != nil {
		t.Fatal(err)
	}

	// Clear the JD tree between preset applies (in production this is
	// applyPreset's responsibility; the importer is called from within
	// its write-tx). Rules + automations are wiped by ApplyReplace.
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

	assertQueryEquals(t, d, `SELECT COUNT(*) FROM rules WHERE preset_slug = ?`, []any{"one"}, 0)
	assertQueryEquals(t, d, `SELECT COUNT(*) FROM rules WHERE preset_slug = ?`, []any{"two"}, 2)
	assertQueryEquals(t, d, `SELECT COUNT(*) FROM rules WHERE preset_slug IS NULL AND name = ?`,
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
		t.Fatalf("SkipSeeds should skip rules + automations: %+v", res)
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
					FilterContentMatching: "electricity|gas bill",
				},
				Actions: []presetfile.Action{{
					Kind: "assign_jd_category",
					Params: map[string]any{
						"jd_category_code": 11,
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
