package main

import (
	"bytes"
	"context"
	"database/sql"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"
	huml "github.com/huml-lang/go-huml"
	"gopkg.in/yaml.v3"

	"github.com/johnnybravo-xyz/suchi/core/config"
	"github.com/johnnybravo-xyz/suchi/core/db"
	migrations "github.com/johnnybravo-xyz/suchi/core/db/migrations"
	"github.com/johnnybravo-xyz/suchi/core/jd/importer"
	"github.com/johnnybravo-xyz/suchi/core/jd/presetfile"
	"github.com/johnnybravo-xyz/suchi/core/logx"
	"github.com/johnnybravo-xyz/suchi/core/taxonomy"
)

// runTaxonomy is `suchi taxonomy <subcommand>`.
//
// Subcommands:
//   - validate <file>                  Silent success (rc=0); positioned errors on stderr (rc=1)
//   - import   <file> [--apply] [--skip-seeds] [--format huml|toml|yaml]
//   - export   [--format huml|toml|yaml]     Dumps to stdout
//   - merge    --kind ... --from-name ... --into-name ... [--apply]
//     Entity merges (tag/correspondent/document_type)
func runTaxonomy(args []string) int {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "usage: suchi taxonomy <subcommand> [flags]")
		fmt.Fprintln(os.Stderr, "subcommands: validate | import | export | merge")
		return 2
	}
	switch args[0] {
	case "validate":
		return runTaxonomyValidate(args[1:])
	case "import":
		return runTaxonomyImport(args[1:])
	case "export":
		return runTaxonomyExport(args[1:])
	case "merge":
		return runTaxonomyMerge(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "unknown taxonomy subcommand %q\n", args[0])
		return 2
	}
}

// runTaxonomyValidate is the presets-repo CI entry point. Silent on
// success; positioned errors on stderr; rc=1 on any failure.
func runTaxonomyValidate(args []string) int {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "usage: suchi taxonomy validate <file>")
		return 2
	}
	path := args[0]
	b, err := os.ReadFile(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", path, err)
		return 1
	}
	if _, err := presetfile.ParseFromExt(b, filepath.Ext(path)); err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", path, err)
		return 1
	}
	return 0
}

// runTaxonomyImport applies a taxonomy file to the local instance.
// Dry-run by default; --apply writes.
func runTaxonomyImport(args []string) int {
	fs := flag.NewFlagSet("suchi taxonomy import", flag.ContinueOnError)
	var (
		apply     = fs.Bool("apply", false, "actually write. Default is dry-run.")
		skipSeeds = fs.Bool("skip-seeds", false, "only touch the JD tree, no rules or automations")
		format    = fs.String("format", "", "override auto-detect: huml|toml|yaml")
	)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() < 1 {
		fmt.Fprintln(os.Stderr, "usage: suchi taxonomy import <file> [--apply] [--skip-seeds] [--format huml|toml|yaml]")
		return 2
	}
	path := fs.Arg(0)
	b, err := os.ReadFile(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", path, err)
		return 1
	}
	var pf *presetfile.PresetFile
	if *format != "" {
		pf, err = presetfile.Parse(b, presetfile.SerFormat(*format))
	} else {
		pf, err = presetfile.ParseFromExt(b, filepath.Ext(path))
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "parse: %v\n", err)
		return 1
	}

	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "config: %v\n", err)
		return 1
	}
	log := logx.Setup(os.Stdout, cfg.LogLevel)
	slog.SetDefault(log)
	ctx := context.Background()
	d, err := db.Open(ctx, cfg.DataDir+"/suchi.db")
	if err != nil {
		fmt.Fprintf(os.Stderr, "db open: %v\n", err)
		return 1
	}
	defer d.Close()
	migs, _ := db.LoadMigrations(migrations.FS, ".")
	if err := db.Migrate(ctx, d, migs, log); err != nil {
		fmt.Fprintf(os.Stderr, "migrate: %v\n", err)
		return 1
	}

	// Diff: count-only summary for both dry-run and apply.
	var stray int
	if err := d.Read.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM documents d
		JOIN jd_categories c ON c.id = d.jd_category_id
		WHERE d.trashed_at IS NULL AND c.system = 0
	`).Scan(&stray); err != nil {
		fmt.Fprintf(os.Stderr, "count non-inbox docs: %v\n", err)
		return 1
	}
	mode := "replace"
	if stray > 0 {
		mode = "merge"
	}

	var seedKw, seedAuto int
	for _, a := range pf.Areas {
		for _, c := range a.Categories {
			seedKw += len(c.Keywords)
		}
	}
	if pf.Seeds != nil {
		seedAuto = len(pf.Seeds.Automations)
	}
	fmt.Printf("preset:      %s v%d\n", pf.ID, pf.Version)
	fmt.Printf("mode:        %s\n", mode)
	fmt.Printf("areas:       %d\n", len(pf.Areas))
	catCount := 0
	for _, a := range pf.Areas {
		catCount += len(a.Categories)
	}
	fmt.Printf("categories:  %d\n", catCount)
	if !*skipSeeds {
		fmt.Printf("keyword rules: %d\n", seedKw)
		fmt.Printf("automations:   %d\n", seedAuto)
	}
	if !*apply {
		fmt.Fprintln(os.Stderr, "\nDry-run — pass --apply to write.")
		return 0
	}
	if mode == "merge" {
		fmt.Fprintln(os.Stderr, "merge apply lands with the admin taxonomy screen; not wired in CLI yet")
		return 1
	}
	err = d.WriteTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `
			UPDATE documents SET jd_category_id = (
				SELECT id FROM jd_categories WHERE system = 1 LIMIT 1
			) WHERE trashed_at IS NULL
		`); err != nil {
			return fmt.Errorf("park docs: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM jd_categories`); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM jd_areas`); err != nil {
			return err
		}
		_, err := importer.ApplyReplace(ctx, tx, log, pf, importer.Options{SkipSeeds: *skipSeeds})
		if err != nil {
			return err
		}
		var newInbox int64
		if err := tx.QueryRowContext(ctx,
			`SELECT id FROM jd_categories WHERE system = 1 LIMIT 1`).Scan(&newInbox); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `
			UPDATE documents SET jd_category_id = ? WHERE trashed_at IS NULL
		`, newInbox)
		return err
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "apply: %v\n", err)
		return 1
	}
	fmt.Fprintln(os.Stderr, "\nApplied.")
	return 0
}

// runTaxonomyExport dumps the current tree to stdout in the requested
// format.
func runTaxonomyExport(args []string) int {
	fs := flag.NewFlagSet("suchi taxonomy export", flag.ContinueOnError)
	format := fs.String("format", "huml", "output format: huml|toml|yaml")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "config: %v\n", err)
		return 1
	}
	log := logx.Setup(os.Stdout, cfg.LogLevel)
	slog.SetDefault(log)
	ctx := context.Background()
	d, err := db.Open(ctx, cfg.DataDir+"/suchi.db")
	if err != nil {
		fmt.Fprintf(os.Stderr, "db open: %v\n", err)
		return 1
	}
	defer d.Close()

	pf, err := readTaxonomyTree(ctx, d)
	if err != nil {
		fmt.Fprintf(os.Stderr, "read: %v\n", err)
		return 1
	}

	switch strings.ToLower(*format) {
	case "huml":
		b, err := huml.Marshal(pf)
		if err != nil {
			fmt.Fprintf(os.Stderr, "encode huml: %v\n", err)
			return 1
		}
		_, _ = io.Copy(os.Stdout, bytes.NewReader(b))
	case "toml":
		if err := toml.NewEncoder(os.Stdout).Encode(pf); err != nil {
			fmt.Fprintf(os.Stderr, "encode toml: %v\n", err)
			return 1
		}
	case "yaml":
		enc := yaml.NewEncoder(os.Stdout)
		enc.SetIndent(2)
		if err := enc.Encode(pf); err != nil {
			fmt.Fprintf(os.Stderr, "encode yaml: %v\n", err)
			return 1
		}
		_ = enc.Close()
	default:
		fmt.Fprintf(os.Stderr, "--format must be huml/toml/yaml, got %q\n", *format)
		return 2
	}
	return 0
}

// readTaxonomyTree reads jd_areas + jd_categories + preset-owned
// keyword rules into a PresetFile ready for encoding. Mirrors the
// server-side buildExportPresetFile, kept local to avoid dragging
// the api pkg into the CLI import graph.
func readTaxonomyTree(ctx context.Context, d *db.DB) (*presetfile.PresetFile, error) {
	pf := &presetfile.PresetFile{
		Format:  presetfile.Format,
		ID:      "exported",
		Version: 1,
		Name:    "Exported taxonomy",
		Story:   "Round-tripped from the running instance.",
	}
	rows, err := d.Read.QueryContext(ctx,
		`SELECT code_start, name FROM jd_areas ORDER BY position, code_start`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var a presetfile.Area
		if err := rows.Scan(&a.Code, &a.Name); err != nil {
			return nil, err
		}
		pf.Areas = append(pf.Areas, a)
	}
	for i := range pf.Areas {
		a := &pf.Areas[i]
		crows, err := d.Read.QueryContext(ctx, `
			SELECT code, name, COALESCE(description, ''), system
			FROM jd_categories WHERE area_start = ? ORDER BY code
		`, a.Code)
		if err != nil {
			return nil, err
		}
		for crows.Next() {
			var c presetfile.Category
			var sys int
			if err := crows.Scan(&c.Code, &c.Name, &c.Description, &sys); err != nil {
				crows.Close()
				return nil, err
			}
			if sys == 1 {
				pf.Inbox = c.Code
			}
			a.Categories = append(a.Categories, c)
		}
		crows.Close()
	}
	return pf, nil
}

func runTaxonomyMerge(args []string) int {
	fs := flag.NewFlagSet("suchi taxonomy merge", flag.ContinueOnError)
	var (
		kind  = fs.String("kind", "", "tag | correspondent | document_type (required)")
		from  = fs.String("from-name", "", "source row name (required; will be deleted)")
		into  = fs.String("into-name", "", "target row name (required; will absorb every reference)")
		apply = fs.Bool("apply", false, "actually merge. Default is dry-run.")
	)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *kind == "" || *from == "" || *into == "" {
		fmt.Fprintln(os.Stderr, "--kind, --from-name, --into-name are all required")
		fs.Usage()
		return 2
	}

	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "config: %v\n", err)
		return 1
	}
	log := logx.Setup(os.Stdout, cfg.LogLevel)
	slog.SetDefault(log)

	ctx := context.Background()

	d, err := db.Open(ctx, cfg.DataDir+"/suchi.db")
	if err != nil {
		log.Error("taxonomy.db.open", "err", err.Error())
		return 1
	}
	defer d.Close()

	migs, _ := db.LoadMigrations(migrations.FS, ".")
	if err := db.Migrate(ctx, d, migs, log); err != nil {
		log.Error("taxonomy.migrate", "err", err.Error())
		return 1
	}

	res, err := taxonomy.Merge(ctx, d, taxonomy.Options{
		Kind: *kind, FromName: *from, IntoName: *into, Apply: *apply,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "merge failed: %v\n", err)
		return 1
	}
	fmt.Fprintf(os.Stderr, `
taxonomy merge (kind=%s, apply=%v).
  From:        %s (id=%d)
  Into:        %s (id=%d)
  Docs moved:  %d
`, res.Kind, res.Applied, res.FromName, res.FromID, res.IntoName, res.IntoID, res.DocsMoved)
	if !res.Applied {
		fmt.Fprintln(os.Stderr, "\nDry-run — pass --apply to actually merge.")
	}
	return 0
}
