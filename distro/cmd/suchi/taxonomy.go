package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"

	"github.com/johnnybravo-xyz/suchi/core/config"
	"github.com/johnnybravo-xyz/suchi/core/db"
	migrations "github.com/johnnybravo-xyz/suchi/core/db/migrations"
	"github.com/johnnybravo-xyz/suchi/core/logx"
	"github.com/johnnybravo-xyz/suchi/core/taxonomy"
)

// runTaxonomy is `suchi taxonomy <subcommand>`. Ships one subcommand
// today (merge); grows with rename, tree-shape ops as needs surface.
func runTaxonomy(args []string) int {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "usage: suchi taxonomy <subcommand> [flags]")
		fmt.Fprintln(os.Stderr, "subcommands: merge")
		return 2
	}
	switch args[0] {
	case "merge":
		return runTaxonomyMerge(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "unknown taxonomy subcommand %q\n", args[0])
		return 2
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
