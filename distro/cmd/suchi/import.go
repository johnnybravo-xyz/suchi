package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"

	"github.com/johnnybravo-xyz/suchi/core/blob"
	"github.com/johnnybravo-xyz/suchi/core/config"
	"github.com/johnnybravo-xyz/suchi/core/db"
	migrations "github.com/johnnybravo-xyz/suchi/core/db/migrations"
	"github.com/johnnybravo-xyz/suchi/core/importer/bundle"
	"github.com/johnnybravo-xyz/suchi/core/jd"
	"github.com/johnnybravo-xyz/suchi/core/logx"
)

// runImport dispatches `suchi import <source>`. Phase-1 ships one source:
// bundle. Others (fs, mail-sidecar, etc.) can slot in here alongside.
func runImport(args []string) int {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "usage: suchi import <source> [flags]")
		fmt.Fprintln(os.Stderr, "sources: bundle")
		return 2
	}
	switch args[0] {
	case "bundle":
		return runImportBundle(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "unknown source %q\n", args[0])
		return 2
	}
}

func runImportBundle(args []string) int {
	fs := flag.NewFlagSet("suchi import bundle", flag.ContinueOnError)
	var (
		from       = fs.String("from", "", "path to the Bundle export bundle root (required)")
		ownerEmail = fs.String("owner-email", "", "email of the user that will own imported documents (required unless --dry-run)")
		dryRun     = fs.Bool("dry-run", false, "parse the bundle and report counts without writing")
		flat       = fs.Bool("flat", false, "force every imported doc to the inbox category — skip JD resolution")
		mapJD      = fs.String("map-jd", "", "path to a rules YAML mapping bundle metadata → JD code")
		autoJD     = fs.Bool("auto-jd", false, "apply the built-in JD heuristics (deterministic keyword matches against the starter tree). Off by default — inbox is the safe fallback.")
	)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	mapping, err := bundle.LoadMapping(*mapJD)
	if err != nil {
		fmt.Fprintf(os.Stderr, "load --map-jd: %v\n", err)
		return 2
	}
	opts := bundle.Options{
		BundleRoot: *from,
		OwnerEmail: *ownerEmail,
		DryRun:     *dryRun,
		Flat:       *flat,
		MapJD:      mapping,
		AutoJD:     *autoJD,
	}
	// Delegate cross-field validation to Options.Validate() so the CLI
	// and every programmatic caller share one rulebook.
	if err := opts.Validate(); err != nil {
		fmt.Fprintf(os.Stderr, "invalid options: %v\n", err)
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

	if err := os.MkdirAll(cfg.DataDir, 0o750); err != nil {
		log.Error("import.datadir", "err", err.Error())
		return 1
	}

	ctx := context.Background()

	d, err := db.Open(ctx, cfg.DataDir+"/dms.db")
	if err != nil {
		log.Error("import.db.open", "err", err.Error())
		return 1
	}
	defer d.Close()

	migs, err := db.LoadMigrations(migrations.FS, ".")
	if err != nil {
		log.Error("import.migrations.load", "err", err.Error())
		return 1
	}
	if err := db.Migrate(ctx, d, migs, log); err != nil {
		log.Error("import.migrate", "err", err.Error())
		return 1
	}
	mode, err := jd.Mode(ctx, d)
	if err != nil {
		log.Error("import.jd.mode", "err", err.Error())
		return 1
	}
	if err := jd.EnsureTree(ctx, d, log, mode); err != nil {
		log.Error("import.jd.ensure", "err", err.Error())
		return 1
	}

	cas, err := blob.New(cfg.DataDir)
	if err != nil {
		log.Error("import.cas", "err", err.Error())
		return 1
	}

	rep, err := bundle.Run(ctx, d, cas, log, opts)
	if err != nil {
		log.Error("import.bundle", "err", err.Error())
		return 1
	}
	// Human-readable summary alongside the structured log.
	fmt.Fprintf(os.Stderr, `
Import complete (dry_run=%v).
  Tags:            %d
  Correspondents:  %d
  Document types:  %d
  Storage paths:   %d
  Custom fields:   %d
  Documents:       %d
  Skipped (dupes): %d
  Mapped by rule:  %d
  Notes:           %d
  Blobs written:   %d
  Warnings:        %d
`,
		*dryRun,
		rep.Tags, rep.Correspondents, rep.DocumentTypes, rep.StoragePaths,
		rep.CustomFields, rep.Documents, rep.DocumentsSkipped, rep.MappedByRule,
		rep.Notes, rep.Blobs, len(rep.Warnings))
	if len(rep.Warnings) > 0 {
		for _, w := range rep.Warnings {
			fmt.Fprintf(os.Stderr, "  ! %s\n", w)
		}
	}
	return 0
}
