// SPDX-License-Identifier: AGPL-3.0-or-later

package importer_test

import (
	"errors"
	"sync"
	"testing"

	"github.com/johnnybravo-xyz/suchi/core/jd/importer"
	"github.com/johnnybravo-xyz/suchi/core/jd/presetfile"
)

func TestMergeCollisionResolutionAndDependentSkip(t *testing.T) {
	for _, target := range []int{0, 12} {
		t.Run(map[int]string{0: "skip", 12: "remap"}[target], func(t *testing.T) {
			d := openTestDB(t)
			apply(t, d, smallPreset(t), importer.Options{})
			pf := smallPreset(t)
			pf.ID = "incoming"
			pf.Areas[0].Categories[0].Name = "Utilities"
			pf.Seeds.Automations[0].Name = "Incoming utility invoices"
			preview, err := importer.Preview(t.Context(), d, pf, importer.Options{})
			if err != nil {
				t.Fatal(err)
			}
			if len(preview.Collisions) != 1 || preview.Collisions[0].Code != 11 || preview.Collisions[0].ProposedCode != 12 || preview.Collisions[0].Resolved {
				t.Fatalf("collision preview=%+v", preview)
			}
			_, err = importer.ImportForDB(t.Context(), d, logger(), pf, importer.Options{})
			var collisions *importer.UnresolvedCollisionsError
			if !errors.As(err, &collisions) {
				t.Fatalf("unresolved import err=%v", err)
			}
			diff := apply(t, d, pf, importer.Options{Remaps: map[int]int{11: target}})
			if target == 0 {
				if len(diff.RulesSkipped) != 2 || len(diff.RulesToAdd) != 0 {
					t.Fatalf("dependent rules not skipped as whole rules: %+v", diff)
				}
				if n := count(t, d, `SELECT COUNT(*) FROM automations WHERE preset_slug='incoming'`); n != 0 {
					t.Fatal("partial dependent rule installed")
				}
			} else {
				if n := count(t, d, `SELECT COUNT(*) FROM jd_categories WHERE code=12 AND name='Utilities'`); n != 1 {
					t.Fatal("remap category absent")
				}
				if n := count(t, d, `SELECT COUNT(*) FROM automation_actions a JOIN automations r ON r.id=a.automation_id JOIN jd_categories c ON c.id=json_extract(a.params_json,'$.jd_category_id') WHERE r.preset_slug='incoming' AND c.code=12`); n != 2 {
					t.Fatal("remapped rules do not target the imported category")
				}
			}
			if n := count(t, d, `SELECT COUNT(*) FROM jd_categories WHERE code=11 AND name='Bills'`); n != 1 {
				t.Fatal("existing category changed")
			}
		})
	}
}

func TestInvalidRemapsAndRuleFailureRollBackEverything(t *testing.T) {
	d := openTestDB(t)
	apply(t, d, smallPreset(t), importer.Options{})
	pf := smallPreset(t)
	pf.ID = "incoming"
	pf.Areas[0].Categories[0].Name = "Utilities"
	pf.Seeds.Automations[0].Name = "Incoming invoices"
	for _, remaps := range []map[int]int{{49: 11}, {11: 10}, {11: 21}, {11: 49}, {12: 13}, {11: 11}} {
		if _, err := importer.ImportForDB(t.Context(), d, logger(), pf, importer.Options{Remaps: remaps}); err == nil {
			t.Fatalf("invalid remap accepted: %v", remaps)
		}
	}
	before := count(t, d, `SELECT COUNT(*) FROM jobs`)
	if _, err := d.Write.ExecContext(t.Context(), `CREATE TRIGGER fail_rule BEFORE INSERT ON automation_actions BEGIN SELECT RAISE(ABORT,'test rule write failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := importer.ImportForDB(t.Context(), d, logger(), pf, importer.Options{Remaps: map[int]int{11: 12}}); err == nil {
		t.Fatal("injected failure ignored")
	}
	if n := count(t, d, `SELECT COUNT(*) FROM jd_categories WHERE code=12`); n != 0 {
		t.Fatal("failed rule left category behind")
	}
	if n := count(t, d, `SELECT COUNT(*) FROM automations WHERE preset_slug='incoming'`); n != 0 {
		t.Fatal("failed rule left partial automation")
	}
	if n := count(t, d, `SELECT COUNT(*) FROM jobs`); n != before {
		t.Fatal("failed import enqueued work")
	}
}

func TestPreviewBindsRelevantStateAndRequest(t *testing.T) {
	for _, change := range []string{"content", "seeds", "remaps", "description", "rule toggle", "filed trash"} {
		t.Run(change, func(t *testing.T) {
			d := openTestDB(t)
			pf := smallPreset(t)
			// Keep bootstrap unchosen for the filed-document eligibility case.
			apply(t, d, pf, importer.Options{})
			opts := importer.Options{ContentSHA256: "submitted-file"}
			preview, err := importer.Preview(t.Context(), d, pf, opts)
			if err != nil {
				t.Fatal(err)
			}
			opts.ExpectedStateHash = preview.StateHash
			switch change {
			case "content":
				opts.ContentSHA256 = "different-file"
			case "seeds":
				opts.SkipSeeds = true
			case "remaps":
				opts.Remaps = map[int]int{11: 12}
			case "description":
				_, err = d.Write.ExecContext(t.Context(), `UPDATE jd_categories SET description='Local edit' WHERE code=11`)
			case "rule toggle":
				_, err = d.Write.ExecContext(t.Context(), `UPDATE automations SET enabled=0 WHERE name='File utility invoices'`)
			case "filed trash":
				id := document(t, d, "Trashed receipt", "")
				_, err = d.Write.ExecContext(t.Context(), `UPDATE documents SET jd_category_id=(SELECT id FROM jd_categories WHERE code=11),trashed_at=1 WHERE id=?`, id)
			}
			if err != nil {
				t.Fatal(err)
			}
			before := count(t, d, `SELECT COUNT(*) FROM jobs`)
			if _, err := importer.Apply(t.Context(), d, logger(), pf, opts); !errors.Is(err, importer.ErrStalePreview) {
				t.Fatalf("changed %s err=%v", change, err)
			}
			if n := count(t, d, `SELECT COUNT(*) FROM jobs`); n != before {
				t.Fatal("stale request mutated archive")
			}
		})
	}
}

func TestConcurrentApplyAllowsOnlyOnePreview(t *testing.T) {
	d := openTestDB(t)
	pf := smallPreset(t)
	preview, err := importer.Preview(t.Context(), d, pf, importer.Options{})
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := importer.Apply(t.Context(), d, logger(), pf, importer.Options{ExpectedStateHash: preview.StateHash})
			results <- err
		}()
	}
	wg.Wait()
	close(results)
	accepted, stale := 0, 0
	for err := range results {
		if err == nil {
			accepted++
		} else if errors.Is(err, importer.ErrStalePreview) {
			stale++
		} else {
			t.Fatal(err)
		}
	}
	if accepted != 1 || stale != 1 {
		t.Fatalf("accepted=%d stale=%d", accepted, stale)
	}
}

func TestLegacyReservedTreeFailsWithoutMutation(t *testing.T) {
	d := openTestDB(t)
	apply(t, d, smallPreset(t), importer.Options{})
	if _, err := d.Write.ExecContext(t.Context(), `INSERT INTO jd_categories(system_id,area_start,code,name,system) VALUES(1,40,41,'Legacy records',0)`); err != nil {
		t.Fatal(err)
	}
	pf := smallPreset(t)
	pf.Areas[0].Categories = append(pf.Areas[0].Categories, presetfile.Category{Code: 12, Name: "Receipts"})
	if _, err := importer.ImportForDB(t.Context(), d, logger(), pf, importer.Options{}); err == nil {
		t.Fatal("legacy reserved content silently accepted")
	}
	if n := count(t, d, `SELECT COUNT(*) FROM jd_categories WHERE code=41 AND name='Legacy records'`); n != 1 {
		t.Fatal("legacy data altered")
	}
	if n := count(t, d, `SELECT COUNT(*) FROM jd_categories WHERE code=12`); n != 0 {
		t.Fatal("partial import into legacy tree")
	}
}
