package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/johnnybravo-xyz/suchi/core/blob"
	"github.com/johnnybravo-xyz/suchi/core/config"
	"github.com/johnnybravo-xyz/suchi/core/db"
	migrations "github.com/johnnybravo-xyz/suchi/core/db/migrations"
	"github.com/johnnybravo-xyz/suchi/core/gc"
	"github.com/johnnybravo-xyz/suchi/core/logx"
)

// runGC is the `suchi gc` subcommand. Reads-only against the DB;
// writes to the filesystem only when --apply is set.
func runGC(args []string) int {
	fs := flag.NewFlagSet("suchi gc", flag.ContinueOnError)
	var (
		grace   = fs.Duration("older-than", 30*24*time.Hour, "skip blobs newer than this; protects freshly-uploaded blobs from a race with pending doc-row inserts")
		apply   = fs.Bool("apply", false, "actually delete the candidate blobs. Default is dry-run.")
		verbose = fs.Bool("verbose", false, "log every candidate + skipped blob (default is summary only)")
	)
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
		log.Error("gc.db.open", "err", err.Error())
		return 1
	}
	defer d.Close()

	migs, err := db.LoadMigrations(migrations.FS, ".")
	if err != nil {
		log.Error("gc.migrations.load", "err", err.Error())
		return 1
	}
	if err := db.Migrate(ctx, d, migs, log); err != nil {
		log.Error("gc.migrate", "err", err.Error())
		return 1
	}

	cas, err := blob.New(cfg.DataDir)
	if err != nil {
		log.Error("gc.cas", "err", err.Error())
		return 1
	}

	rep, err := gc.Run(ctx, d, cas, cfg.DataDir, log, gc.Options{
		Grace:   *grace,
		Apply:   *apply,
		Verbose: *verbose,
	})
	if err != nil {
		log.Error("gc.run", "err", err.Error())
		return 1
	}
	fmt.Fprintf(os.Stderr, `
suchi gc (apply=%v, grace=%s).
  Referenced blobs:     %d
  Orphan candidates:    %d
  Skipped (grace):      %d
  Deleted:              %d
  Bytes reclaimable:    %d
  Failures:             %d
`, *apply, grace.String(),
		rep.Referenced, rep.OrphanCandidates, rep.Skipped,
		rep.Deleted, rep.BytesReclaimable, len(rep.Failures))
	if len(rep.Failures) > 0 {
		fmt.Fprintln(os.Stderr, "\nFailures:")
		for _, f := range rep.Failures {
			fmt.Fprintf(os.Stderr, "  %s: %s\n", f.SHA256, f.Err)
		}
	}
	if !*apply && rep.OrphanCandidates > 0 {
		fmt.Fprintln(os.Stderr, "\nDry-run — re-run with --apply to actually delete.")
	}
	return 0
}
