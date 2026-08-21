package importer_test

// Merge-mode tests for ApplyMerge:
//   - additive insert on an empty tree,
//   - same-code same-name = no-op,
//   - same-code different-name + no remap = UnresolvedCollisionsError,
//   - collision + skip via remaps[code]=0,
//   - collision + remap via remaps[code]=<free code in decade>,
//   - keyword automations land under the *effective* code after remap,
//   - user-owned CoW copies survive re-apply of the same preset.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"testing"

	"github.com/johnnybravo-xyz/suchi/core/db"
	"github.com/johnnybravo-xyz/suchi/core/jd/importer"
	"github.com/johnnybravo-xyz/suchi/core/jd/presetfile"
)

func TestApplyMerge_AdditiveOnEmptyTree(t *testing.T) {
	d := openTestDB(t)
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))

	pf := smallPreset()
	pf.ID = "onto-empty"
	if err := runMerge(d, log, pf, nil); err != nil {
		t.Fatal(err)
	}
	assertQueryEquals(t, d, `SELECT COUNT(*) FROM jd_areas`, nil, 2)
	assertQueryEquals(t, d, `SELECT COUNT(*) FROM jd_categories`, nil, 3)
	assertQueryEquals(t, d, `SELECT COUNT(*) FROM automations WHERE preset_slug = ?`,
		[]any{"onto-empty"}, 2)
}

func TestApplyMerge_SameCodeSameNameNoOp(t *testing.T) {
	d := openTestDB(t)
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))

	// Seed the exact same tree twice via merge.
	pf := smallPreset()
	pf.ID = "sameshape"
	if err := runMerge(d, log, pf, nil); err != nil {
		t.Fatal(err)
	}
	// Second merge — same-code same-name across the board.
	if err := runMerge(d, log, pf, nil); err != nil {
		t.Fatal(err)
	}
	assertQueryEquals(t, d, `SELECT COUNT(*) FROM jd_areas`, nil, 2)
	assertQueryEquals(t, d, `SELECT COUNT(*) FROM jd_categories`, nil, 3)
}

func TestApplyMerge_UnresolvedCollisionErrors(t *testing.T) {
	d := openTestDB(t)
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))

	// Prime the DB with a category named "Bills" at code 11.
	first := smallPreset()
	first.ID = "first"
	if err := runMerge(d, log, first, nil); err != nil {
		t.Fatal(err)
	}

	// Second preset: same code 11, but named "Utilities".
	clash := &presetfile.PresetFile{
		Format:  presetfile.Format,
		ID:      "clash",
		Version: 1,
		Name:    "Clash",
		Story:   "test",
		Inbox:   49,
		Areas: []presetfile.Area{
			{Code: 10, Name: "Life", Categories: []presetfile.Category{
				{Code: 11, Name: "Utilities"},
			}},
			{Code: 40, Name: "System", Categories: []presetfile.Category{
				{Code: 49, Name: "Inbox"},
			}},
		},
	}
	err := runMerge(d, log, clash, nil)
	var uerr *importer.UnresolvedCollisionsError
	if !errors.As(err, &uerr) {
		t.Fatalf("expected UnresolvedCollisionsError, got: %v", err)
	}
	if len(uerr.Items) != 1 || uerr.Items[0].Code != 11 {
		t.Fatalf("collision list: %+v", uerr.Items)
	}
	if uerr.Items[0].Existing != "Bills" || uerr.Items[0].Incoming != "Utilities" {
		t.Fatalf("collision names wrong: %+v", uerr.Items[0])
	}
}

func TestApplyMerge_CollisionSkipDropsSeeds(t *testing.T) {
	d := openTestDB(t)
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))

	first := smallPreset()
	first.ID = "keep"
	if err := runMerge(d, log, first, nil); err != nil {
		t.Fatal(err)
	}

	clash := smallPreset()
	clash.ID = "clash"
	clash.Areas[0].Categories[0].Name = "Utilities"

	// Skip the collision. Keywords for that category shouldn't land.
	if err := runMerge(d, log, clash, map[int]int{11: 0}); err != nil {
		t.Fatalf("merge with skip: %v", err)
	}
	// jd_categories: still 3 (nothing added for the skipped one).
	assertQueryEquals(t, d, `SELECT COUNT(*) FROM jd_categories`, nil, 3)
	// The "clash" preset's own automation references jd_category_code=11
	// which was skipped — resolveActionParams returns an error, so no
	// automation lands. That's the expected honest failure.
	assertQueryEquals(t, d, `SELECT COUNT(*) FROM automations WHERE preset_slug = ?`, []any{"clash"}, 0)
}

func TestApplyMerge_CollisionRemapImportsAtNewCode(t *testing.T) {
	d := openTestDB(t)
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))

	first := smallPreset()
	first.ID = "keep"
	if err := runMerge(d, log, first, nil); err != nil {
		t.Fatal(err)
	}

	clash := smallPreset()
	clash.ID = "clash"
	clash.Areas[0].Categories[0].Name = "Utilities"

	// Remap 11 → 12 (free code in the 10-19 decade).
	if err := runMerge(d, log, clash, map[int]int{11: 12}); err != nil {
		t.Fatalf("merge with remap: %v", err)
	}

	// Category exists at code 12 under the "clash" name.
	var name string
	if err := d.Read.QueryRowContext(context.Background(),
		`SELECT name FROM jd_categories WHERE code = 12`).Scan(&name); err != nil {
		t.Fatal(err)
	}
	if name != "Utilities" {
		t.Fatalf("remapped category name: got %q, want Utilities", name)
	}
	// The keyword automation should reference the remapped category at code 12.
	assertQueryEquals(t, d,
		`SELECT COUNT(*) FROM automation_actions aa
		 JOIN automations a ON a.id = aa.automation_id
		 JOIN jd_categories c ON c.id = json_extract(aa.params_json, '$.jd_category_id')
		 WHERE a.preset_slug = ? AND c.code = ?
		   AND json_type(aa.params_json, '$._preset_keywords') = 'array'`,
		[]any{"clash", 12}, 1)
	// Original category at 11 (Bills) is untouched.
	assertQueryEquals(t, d,
		`SELECT COUNT(*) FROM jd_categories WHERE code = 11 AND name = ?`,
		[]any{"Bills"}, 1)
}

func TestApplyMerge_RemapOutOfDecadeErrors(t *testing.T) {
	d := openTestDB(t)
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	first := smallPreset()
	first.ID = "keep"
	if err := runMerge(d, log, first, nil); err != nil {
		t.Fatal(err)
	}
	clash := smallPreset()
	clash.ID = "clash"
	clash.Areas[0].Categories[0].Name = "Utilities"
	err := runMerge(d, log, clash, map[int]int{11: 21}) // 21 not in 10-19
	if err == nil || !containsSubstr(err.Error(), "out of decade") {
		t.Fatalf("expected out-of-decade error; got %v", err)
	}
}

func TestApplyMerge_ReSeedIdempotent_UserForksSurvive(t *testing.T) {
	d := openTestDB(t)
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	pf := smallPreset()
	pf.ID = "iter"
	if err := runMerge(d, log, pf, nil); err != nil {
		t.Fatal(err)
	}
	// User forks an automation into a user-owned copy (simulates CoW).
	if _, err := d.Write.ExecContext(context.Background(), `
		INSERT INTO automations(name, order_index, enabled, preset_slug, created_at, updated_at)
		VALUES ('user-fork', 100, 1, NULL, 0, 0)
	`); err != nil {
		t.Fatal(err)
	}
	// Re-apply the same preset (no collisions).
	if err := runMerge(d, log, pf, nil); err != nil {
		t.Fatal(err)
	}
	// User fork present; preset automations re-seeded (same count, not doubled).
	assertQueryEquals(t, d, `SELECT COUNT(*) FROM automations WHERE preset_slug IS NULL AND name = 'user-fork'`, nil, 1)
	assertQueryEquals(t, d, `SELECT COUNT(*) FROM automations WHERE preset_slug = ?`, []any{"iter"}, 2)
}

// runMerge is a small wrapper that opens a WriteTx and calls
// ApplyMerge inside it — mirrors the shape /api/admin/taxonomy/import
// uses on the merge branch.
func runMerge(d *db.DB, log *slog.Logger, pf *presetfile.PresetFile, remaps map[int]int) error {
	return d.WriteTx(context.Background(), func(tx *sql.Tx) error {
		_, err := importer.ApplyMerge(context.Background(), tx, log, pf,
			importer.Options{Remaps: remaps})
		return err
	})
}

func containsSubstr(hay, needle string) bool {
	return len(hay) >= len(needle) && (hay == needle ||
		(len(needle) > 0 && (indexOf(hay, needle) >= 0)))
}

// indexOf — tiny substring search to avoid importing strings just
// for one call site.
func indexOf(hay, needle string) int {
	n, m := len(hay), len(needle)
	if m == 0 {
		return 0
	}
	for i := 0; i+m <= n; i++ {
		if hay[i:i+m] == needle {
			return i
		}
	}
	return -1
}

// Suppress the "declared and not used" for fmt import Go might inline
// away if none of the test cases exercise the path — pin it in a
// sink so re-adds don't drag in a fresh import.
var _ = fmt.Sprintf
