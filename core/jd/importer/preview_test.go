package importer_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/johnnybravo-xyz/suchi/core/jd/importer"
	"github.com/johnnybravo-xyz/suchi/core/jd/presetfile"
)

func TestPreviewBindsTrimmedExactMetadataReferences(t *testing.T) {
	for _, tc := range []struct {
		name, table, kind, param string
		trigger                  bool
	}{
		{name: "tag action", table: "tags", kind: "assign_tags", param: "tag"},
		{name: "tag list action", table: "tags", kind: "assign_tags", param: "tags"},
		{name: "correspondent action", table: "correspondents", kind: "assign_correspondent", param: "correspondent"},
		{name: "document type action", table: "document_types", kind: "assign_document_type", param: "document_type"},
		{name: "tag trigger", table: "tags", trigger: true},
		{name: "correspondent trigger", table: "correspondents", trigger: true},
		{name: "document type trigger", table: "document_types", trigger: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := openTestDB(t)
			pf := smallPreset(t)
			apply(t, d, pf, importer.Options{SkipSeeds: true})
			pf.Areas[0].Categories[0].Keywords = nil
			const reference = " \tTax\u2003 "
			rule := presetfile.SeedAutomation{
				Name:    "Reference local metadata",
				Trigger: presetfile.Trigger{Type: 2},
				Actions: []presetfile.Action{{Kind: "assign_title", Params: map[string]any{"template": "Reviewed"}}},
			}
			if tc.trigger {
				switch tc.table {
				case "tags":
					rule.Trigger.FilterTag = reference
				case "correspondents":
					rule.Trigger.FilterCorrespondent = reference
				case "document_types":
					rule.Trigger.FilterDocumentType = reference
				}
			} else {
				var value any = reference
				if tc.param == "tags" {
					value = []string{reference}
				}
				rule.Actions = []presetfile.Action{{Kind: tc.kind, Params: map[string]any{tc.param: value}}}
			}
			pf.Seeds.Automations = []presetfile.SeedAutomation{rule}
			if _, err := d.ExecWrite(t.Context(), "INSERT INTO "+tc.table+"(system_id,name,slug,created_at,updated_at) VALUES(1,'Tax','custom-tax',0,0)"); err != nil {
				t.Fatal(err)
			}
			before, err := importer.Preview(t.Context(), d, pf, importer.Options{})
			if err != nil {
				t.Fatal(err)
			}
			jobs := count(t, d, `SELECT COUNT(*) FROM jobs`)
			if _, err := d.ExecWrite(t.Context(), "UPDATE "+tc.table+" SET name='Former Tax' WHERE system_id=1 AND name='Tax'"); err != nil {
				t.Fatal(err)
			}
			after, err := importer.Preview(t.Context(), d, pf, importer.Options{})
			if err != nil {
				t.Fatal(err)
			}
			if after.StateHash == before.StateHash {
				t.Fatal("renaming the trimmed exact match did not invalidate its preview")
			}
			if _, err := importer.Apply(t.Context(), d, logger(), pf, importer.Options{ExpectedStateHash: before.StateHash}); !errors.Is(err, importer.ErrStalePreview) {
				t.Fatalf("stale reference Apply = %v", err)
			}
			if count(t, d, "SELECT COUNT(*) FROM "+tc.table) != 1 || count(t, d, `SELECT COUNT(*) FROM automations`) != 0 || count(t, d, `SELECT COUNT(*) FROM jobs`) != jobs {
				t.Fatal("stale Apply created metadata, rules, or jobs")
			}
		})
	}
}

func TestPreviewRejectsDuplicatePlannedStarterNames(t *testing.T) {
	for _, remapped := range []bool{false, true} {
		name := "new archive"
		if remapped {
			name = "remapped category"
		}
		t.Run(name, func(t *testing.T) {
			d := openTestDB(t)
			pf := smallPreset(t)
			opts := importer.Options{}
			duplicate := "smalltest: file 11 Bills"
			if remapped {
				apply(t, d, pf, importer.Options{SkipSeeds: true})
				pf.Areas[0].Categories[0].Name = "Utilities"
				opts.Remaps = map[int]int{11: 12}
				duplicate = "smalltest: file 12 Utilities"
			}
			pf.System = "S01"
			pf.Seeds.Automations[0].Name = duplicate
			categories := count(t, d, `SELECT COUNT(*) FROM jd_categories`)
			jobs := count(t, d, `SELECT COUNT(*) FROM jobs`)
			_, err := importer.Preview(t.Context(), d, pf, opts)
			if err == nil || !strings.Contains(err.Error(), duplicate) || !strings.Contains(err.Error(), "rename the explicit starter") {
				t.Fatalf("duplicate starter Preview = %v; want conflicting name and resolution", err)
			}
			if _, err := importer.ImportForDB(t.Context(), d, logger(), pf, opts); err == nil {
				t.Fatal("CLI import accepted duplicate planned rules")
			}
			if count(t, d, `SELECT COUNT(*) FROM jd_systems WHERE code<>''`) != 0 || count(t, d, `SELECT COUNT(*) FROM jd_categories`) != categories || count(t, d, `SELECT COUNT(*) FROM automations`) != 0 || count(t, d, `SELECT COUNT(*) FROM jobs`) != jobs {
				t.Fatal("rejected proposal changed system identity, tree, rules, or jobs")
			}
			pf.Seeds.Automations[0].Name = "Explicit utility filing"
			diff := apply(t, d, pf, opts)
			if len(diff.RulesToAdd) != 2 {
				t.Fatalf("renaming the explicit starter did not resolve collision: %+v", diff)
			}
		})
	}
}

func TestDuplicateStarterNamesDoNotBlockPreservedOrSkippedRules(t *testing.T) {
	for _, preserve := range []bool{false, true} {
		name := "skip starters"
		if preserve {
			name = "preserve existing rule"
		}
		t.Run(name, func(t *testing.T) {
			d := openTestDB(t)
			pf := smallPreset(t)
			if preserve {
				apply(t, d, pf, importer.Options{})
			}
			pf.Seeds.Automations[0].Name = "smalltest: file 11 Bills"
			diff := apply(t, d, pf, importer.Options{SkipSeeds: !preserve})
			if len(diff.RulesToAdd) != 0 {
				t.Fatalf("skipped or preserved candidates planned additions: %+v", diff)
			}
			if preserve && count(t, d, `SELECT COUNT(*) FROM automations`) != 2 {
				t.Fatal("existing rules changed")
			}
			if !preserve && count(t, d, `SELECT COUNT(*) FROM automations`) != 0 {
				t.Fatal("skipped rules were added")
			}
		})
	}
}
