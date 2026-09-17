package api

import (
	"fmt"
	"io"
	"log/slog"
	"testing"

	"github.com/johnnybravo-xyz/suchi/core/automations"
	"github.com/johnnybravo-xyz/suchi/core/jd/importer"
	"github.com/johnnybravo-xyz/suchi/core/jd/presetfile"
	"github.com/johnnybravo-xyz/suchi/core/taxonomy/exporter"
)

const portableTaxonomy = `format = "suchi-taxonomy/v1"
id = "utility-files"
version = 7
name = "Utility files"
market = "global"
language = "en"
maintainer = "Source author"
license = "CC0-1.0"
story = "File utility invoices with an explicit named-filter starter."
[[areas]]
code = 10
name = "Life admin"
[[areas.categories]]
code = 11
name = "Bills"
description = "Invoices for household utilities."
[[seeds.automations]]
name = "Label utility invoice"
[seeds.automations.trigger]
type = 2
filter_title_matching = "Invoice"
filter_content_matching = "electricity"
filter_has_tag = "To file"
filter_has_correspondent = "Utility company"
filter_has_document_type = "Bill"
[[seeds.automations.actions]]
kind = "assign_title"
[seeds.automations.actions.params]
template = "Filed utility invoice"
[[seeds.automations.actions]]
kind = "assign_jd_category"
[seeds.automations.actions.params]
jd_category_code = 11
`

func TestTaxonomyExportPreservesNamedAndTitleFilters(t *testing.T) {
	ctx := t.Context()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	source := openTestDB(t)
	pf, err := presetfile.Parse([]byte(portableTaxonomy), presetfile.FormatTOML)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := importer.ImportForDB(ctx, source, log, pf, importer.Options{}); err != nil {
		t.Fatal(err)
	}
	exported, err := exporter.BuildExport(ctx, source, 1, false)
	if err != nil {
		t.Fatal(err)
	}
	if exported.ID != "archive" || exported.Version != 1 || exported.Maintainer != "" || exported.License != "" {
		t.Fatalf("snapshot falsely claims source identity: %+v", exported)
	}
	for _, format := range []presetfile.SerFormat{presetfile.FormatHuML, presetfile.FormatTOML} {
		t.Run(string(format), func(t *testing.T) {
			data, err := presetfile.Marshal(exported, format)
			if err != nil {
				t.Fatal(err)
			}
			roundTrip, err := presetfile.Parse(data, format)
			if err != nil {
				t.Fatal(err)
			}
			dest := openTestDB(t)
			if _, err := importer.ImportForDB(ctx, dest, log, roundTrip, importer.Options{}); err != nil {
				t.Fatal(err)
			}
			if _, err := dest.Write.Exec(`INSERT INTO users(id,email,display_name,role,created_at,updated_at) VALUES(1,'test@example.test','Test','admin',0,0)`); err != nil {
				t.Fatal(err)
			}
			for i, tc := range []struct {
				title string
				tag   bool
				want  string
			}{
				{"Invoice", true, "Filed utility invoice"},
				{"Statement", true, "Statement"},
				{"Invoice", false, "Invoice"},
			} {
				res, err := dest.Write.Exec(`INSERT INTO documents(system_id,owner_id,title,content,original_blob,original_size,jd_category_id,correspondent_id,document_type_id,created_at,added_at,updated_at) VALUES(1,1,?,'electricity',?,1,(SELECT id FROM jd_categories WHERE code=49),(SELECT id FROM correspondents WHERE name='Utility company'),(SELECT id FROM document_types WHERE name='Bill'),0,0,0)`, tc.title, fmt.Sprintf("source-%d", i))
				if err != nil {
					t.Fatal(err)
				}
				id, err := res.LastInsertId()
				if err != nil {
					t.Fatal(err)
				}
				if tc.tag {
					if _, err := dest.Write.Exec(`INSERT INTO document_tags(document_id,tag_id) VALUES(?,(SELECT id FROM tags WHERE name='To file'))`, id); err != nil {
						t.Fatal(err)
					}
				}
				if err := automations.ApplyOnDocumentAdded(ctx, dest, log, id); err != nil {
					t.Fatal(err)
				}
				var title string
				if err := dest.Read.QueryRow(`SELECT title FROM documents WHERE id=?`, id).Scan(&title); err != nil {
					t.Fatal(err)
				}
				if title != tc.want {
					t.Fatalf("round-trip rule changed matching: title=%q tagged=%v got=%q want=%q", tc.title, tc.tag, title, tc.want)
				}
			}
		})
	}
}

func TestTaxonomyExportRejectsDisabledAndUnsupportedRules(t *testing.T) {
	d := openTestDB(t)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	pf, err := presetfile.Parse([]byte(portableTaxonomy), presetfile.FormatTOML)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := importer.ImportForDB(t.Context(), d, log, pf, importer.Options{}); err != nil {
		t.Fatal(err)
	}
	for _, mutation := range []string{
		`UPDATE automations SET enabled=0`,
		`UPDATE automations SET enabled=1; UPDATE automation_actions SET kind='assign_metadata' WHERE kind='assign_title'`,
	} {
		if _, err := d.Write.Exec(mutation); err != nil {
			t.Fatal(err)
		}
		if _, err := exporter.BuildExport(t.Context(), d, 1, false); err == nil {
			t.Fatal("unrepresentable behavior silently exported")
		}
		tree, err := exporter.BuildExport(t.Context(), d, 1, true)
		if err != nil {
			t.Fatal(err)
		}
		if tree.Seeds != nil {
			t.Fatal("tree-only snapshot includes executable behavior")
		}
		if _, err := presetfile.Marshal(tree, presetfile.FormatHuML); err != nil {
			t.Fatal(err)
		}
	}
}

func TestTaxonomyAuthoringFormat(t *testing.T) {
	for _, value := range []string{"huml", "HuML", "toml", " TOML "} {
		if _, ok := taxonomyAuthoringFormat(value, ""); !ok {
			t.Errorf("taxonomyAuthoringFormat(%q) rejected", value)
		}
	}
	if got, ok := taxonomyAuthoringFormat("", "huml"); !ok || got != "huml" {
		t.Fatalf("fallback = %q, %v", got, ok)
	}
	for _, value := range []string{"yaml", "json"} {
		if _, ok := taxonomyAuthoringFormat(value, ""); ok {
			t.Errorf("taxonomyAuthoringFormat(%q) accepted", value)
		}
	}
}
