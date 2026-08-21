package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/BurntSushi/toml"
	huml "github.com/huml-lang/go-huml"

	"github.com/johnnybravo-xyz/suchi/core/config"
	"github.com/johnnybravo-xyz/suchi/core/db"
	migrations "github.com/johnnybravo-xyz/suchi/core/db/migrations"
	"github.com/johnnybravo-xyz/suchi/core/jd/importer"
	"github.com/johnnybravo-xyz/suchi/core/jd/presetfile"
	"github.com/johnnybravo-xyz/suchi/core/logx"
	"github.com/johnnybravo-xyz/suchi/core/taxonomy"
)

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

func runTaxonomyImport(args []string) int {
	fs := flag.NewFlagSet("suchi taxonomy import", flag.ContinueOnError)
	remaps := taxonomyRemaps{}
	var (
		apply     = fs.Bool("apply", false, "actually write. Default is dry-run.")
		skipSeeds = fs.Bool("skip-seeds", false, "only touch the JD tree, without starter automations")
		format    = fs.String("format", "", "override auto-detect: huml|toml")
	)
	fs.Var(&remaps, "remap", "merge collision as incoming:target or incoming:skip; repeatable")
	path, err := parseTaxonomyImportArgs(fs, args)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	b, err := os.ReadFile(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", path, err)
		return 1
	}
	var pf *presetfile.PresetFile
	if *format != "" {
		f, formatErr := preferredTaxonomyFormat(*format)
		if formatErr != nil {
			fmt.Fprintln(os.Stderr, formatErr)
			return 2
		}
		pf, err = presetfile.Parse(b, f)
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

	mode, err := taxonomy.ImportMode(ctx, d)
	if err != nil {
		fmt.Fprintf(os.Stderr, "choose import mode: %v\n", err)
		return 1
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
		fmt.Printf("filing keywords: %d\n", seedKw)
		fmt.Printf("automations:   %d\n", seedAuto)
	}
	if !*apply {
		fmt.Fprintln(os.Stderr, "\nDry-run — pass --apply to write.")
		return 0
	}
	hash := sha256.Sum256(b)
	contentSHA := hex.EncodeToString(hash[:])
	err = d.WriteTx(ctx, func(tx *sql.Tx) error {
		if mode == "merge" {
			if _, err := importer.ApplyMerge(ctx, tx, log, pf, importer.Options{
				SkipSeeds: *skipSeeds,
				Remaps:    remaps,
			}); err != nil {
				return err
			}
			return importer.WriteImportProvenance(ctx, tx, pf.ID, pf.Version, contentSHA)
		}
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
		if _, err = tx.ExecContext(ctx, `
			UPDATE documents SET jd_category_id = ? WHERE trashed_at IS NULL
		`, newInbox); err != nil {
			return err
		}
		return importer.WriteImportProvenance(ctx, tx, pf.ID, pf.Version, contentSHA)
	})
	if err != nil {
		var unresolved *importer.UnresolvedCollisionsError
		if errors.As(err, &unresolved) {
			for _, collision := range unresolved.Items {
				fmt.Fprintf(os.Stderr,
					"collision %d: existing %q, incoming %q; use --remap %d:skip or --remap %d:<free-code>\n",
					collision.Code, collision.Existing, collision.Incoming, collision.Code, collision.Code)
			}
			return 1
		}
		fmt.Fprintf(os.Stderr, "apply: %v\n", err)
		return 1
	}
	fmt.Fprintln(os.Stderr, "\nApplied.")
	return 0
}

func runTaxonomyExport(args []string) int {
	fs := flag.NewFlagSet("suchi taxonomy export", flag.ContinueOnError)
	format := fs.String("format", "huml", "output format: huml|toml")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	outputFormat, err := preferredTaxonomyFormat(*format)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
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

	pf, err := taxonomy.BuildExport(ctx, d)
	if err != nil {
		fmt.Fprintf(os.Stderr, "read: %v\n", err)
		return 1
	}

	switch outputFormat {
	case presetfile.FormatHuML:
		b, err := huml.Marshal(pf)
		if err != nil {
			fmt.Fprintf(os.Stderr, "encode huml: %v\n", err)
			return 1
		}
		_, _ = io.Copy(os.Stdout, bytes.NewReader(b))
	case presetfile.FormatTOML:
		if err := toml.NewEncoder(os.Stdout).Encode(pf); err != nil {
			fmt.Fprintf(os.Stderr, "encode toml: %v\n", err)
			return 1
		}
	}
	return 0
}

func preferredTaxonomyFormat(raw string) (presetfile.SerFormat, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "huml":
		return presetfile.FormatHuML, nil
	case "toml":
		return presetfile.FormatTOML, nil
	default:
		return "", fmt.Errorf("--format must be huml or toml, got %q", raw)
	}
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

type taxonomyRemaps map[int]int

func (r *taxonomyRemaps) String() string { return "" }

func (r *taxonomyRemaps) Set(value string) error {
	incoming, target, ok := strings.Cut(value, ":")
	if !ok {
		return fmt.Errorf("remap %q must be incoming:target or incoming:skip", value)
	}
	from, err := strconv.Atoi(incoming)
	if err != nil || from <= 0 {
		return fmt.Errorf("remap source %q must be a positive category code", incoming)
	}
	to := 0
	if target != "skip" {
		to, err = strconv.Atoi(target)
		if err != nil || to <= 0 {
			return fmt.Errorf("remap target %q must be a positive category code or skip", target)
		}
	}
	(*r)[from] = to
	return nil
}

func parseTaxonomyImportArgs(fs *flag.FlagSet, args []string) (string, error) {
	usage := "usage: suchi taxonomy import <file> [--apply] [--skip-seeds] [--format huml|toml] [--remap incoming:target]"
	path := ""
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		path, args = args[0], args[1:]
	}
	if err := fs.Parse(args); err != nil {
		return "", err
	}
	if path == "" && fs.NArg() == 1 {
		path = fs.Arg(0)
	} else if fs.NArg() != 0 {
		return "", errors.New(usage)
	}
	if path == "" {
		return "", errors.New(usage)
	}
	return path, nil
}
