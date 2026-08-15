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

// runImport handles `suchi import [flags]`. Point --from at an export
// bundle produced by your current DMS — the importer speaks the widely-
// used JSON-manifest + originals/archive/ layout and lands docs +
// metadata verbatim.
func runImport(args []string) int {
	fs := flag.NewFlagSet("suchi import", flag.ContinueOnError)
	var (
		from       = fs.String("from", "", "path to the export bundle root (required)")
		ownerEmail = fs.String("owner-email", "", "email of the user that will own imported documents (required unless --dry-run)")
		dryRun     = fs.Bool("dry-run", false, "parse the bundle and report counts without writing")
		flat       = fs.Bool("flat", false, "force every imported doc to the inbox category — skip JD resolution")
		mapJD      = fs.String("map-jd", "", "path to a rules YAML mapping bundle metadata → JD code")
		autoJD     = fs.Bool("auto-jd", false, "apply the built-in JD heuristics (deterministic keyword matches against the starter tree). Off by default — inbox is the safe fallback.")
		verify     = fs.Bool("verify", false, "dry-diff the bundle against the live DB — no writes. Prints new/match/differ/orphan counts.")
		reportPath = fs.String("report", "./import-report.md", "write a FULL/PARTIAL/FAILED markdown report of the migration to this path")
	)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	// --verify is orthogonal to the write modes (--flat/--map-jd/--auto-jd)
	// because verify never writes and never resolves JD categories. It
	// answers "what would change" against current DB state.
	if *verify {
		return runImportVerify(*from)
	}

	mapping, err := bundle.LoadMapping(*mapJD)
	if err != nil {
		fmt.Fprintf(os.Stderr, "load --map-jd: %v\n", err)
		return 2
	}
	mrep := bundle.NewMigrationReport(*from, "")
	opts := bundle.Options{
		BundleRoot: *from,
		OwnerEmail: *ownerEmail,
		DryRun:     *dryRun,
		Flat:       *flat,
		MapJD:      mapping,
		AutoJD:     *autoJD,
		Report:     mrep,
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

	d, err := db.Open(ctx, cfg.DataDir+"/suchi.db")
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
	if *reportPath != "" && !*dryRun {
		f, err := os.Create(*reportPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "open --report %s: %v\n", *reportPath, err)
			return 1
		}
		if err := mrep.Emit(f); err != nil {
			f.Close()
			fmt.Fprintf(os.Stderr, "write --report %s: %v\n", *reportPath, err)
			return 1
		}
		f.Close()
		fmt.Fprintf(os.Stderr, "Migration report written to %s\n", *reportPath)
	}
	return 0
}

// runImportVerify is the --verify entry point. Reads-only: parses the
// bundle, diffs against the live DB, prints a partition. Never opens a
// write transaction and never resolves owner-email.
func runImportVerify(bundleRoot string) int {
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
		log.Error("verify.db.open", "err", err.Error())
		return 1
	}
	defer d.Close()

	rep, err := bundle.Verify(ctx, d, log, bundle.VerifyOptions{BundleRoot: bundleRoot})
	if err != nil {
		log.Error("verify.bundle", "err", err.Error())
		return 1
	}
	fmt.Fprintf(os.Stderr, `
Verify (no writes).
  New:     %d
  Match:   %d
  Differ:  %d
  Orphan:  %d
`, len(rep.New), len(rep.Match), len(rep.Differ), len(rep.Orphan))
	if len(rep.Differ) > 0 {
		fmt.Fprintln(os.Stderr, "\nDiffering docs:")
		for _, d := range rep.Differ {
			fmt.Fprintf(os.Stderr, "  pk=%d  fields=%v\n", d.LegacyID, d.Fields)
		}
	}
	if len(rep.Orphan) > 0 {
		fmt.Fprintln(os.Stderr, "\nOrphan docs (in suchi, not in bundle):")
		for _, pk := range rep.Orphan {
			fmt.Fprintf(os.Stderr, "  pk=%d\n", pk)
		}
	}
	return 0
}
