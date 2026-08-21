// `suchi refile` — the operator-side kick that says "I changed the
// preset / template / automations, now make it stick." Iterates every live
// doc, re-runs document-added automations, enqueues a
// render/move job so the rendered-view symlinks converge on the
// current storage-path template.
//
// Safe to run against a live server. Uploads that arrive mid-sweep
// use the normal postingest chain and land under the new tree
// automatically (see core/refile/refile.go's concurrency invariants).
//
// Selling point: "you can always come back to change this."

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
	"github.com/johnnybravo-xyz/suchi/core/logx"
	"github.com/johnnybravo-xyz/suchi/core/refile"
)

func runRefile(args []string) int {
	fs := flag.NewFlagSet("suchi refile", flag.ContinueOnError)
	var (
		skipAutomations = fs.Bool("skip-automations", false, "don't re-run automations; enqueue render only")
		skipRender      = fs.Bool("skip-render", false, "don't enqueue render jobs — re-run classifier only")
		ownerID         = fs.Int64("owner-id", 0, "restrict to docs owned by this user id; 0 = every owner")
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
		fmt.Fprintf(os.Stderr, "open db: %v\n", err)
		return 1
	}
	defer func() { _ = d.Close() }()

	// Boot-time migrations — if the operator is running refile against
	// a fresh DATA_DIR the schema needs to be current or the query
	// under refile blows up.
	migs, err := db.LoadMigrations(migrations.FS, ".")
	if err != nil {
		fmt.Fprintf(os.Stderr, "load migrations: %v\n", err)
		return 1
	}
	if err := db.Migrate(ctx, d, migs, log); err != nil {
		fmt.Fprintf(os.Stderr, "migrate: %v\n", err)
		return 1
	}

	// CAS is not strictly needed by refile itself, but the render
	// handler dereferences it when the dispatcher picks up the
	// enqueued jobs. Nothing calls it here.
	_ = blob.CAS{}

	stats, err := refile.All(ctx, d, log, refile.Options{
		SkipAutomations: *skipAutomations,
		SkipRender:      *skipRender,
		OwnerID:         *ownerID,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "refile: %v\n", err)
		return 1
	}
	fmt.Printf("refile complete: docs=%d automations_applied=%d render_enqueued=%d errors=%d elapsed=%s\n",
		stats.DocsScanned, stats.AutomationsApplied, stats.RenderEnqueued,
		stats.Errors, stats.Elapsed)
	if stats.RenderEnqueued > 0 {
		fmt.Println("note: render jobs are enqueued; a running suchi serve will process them, or the next boot will pick them up.")
	}
	return 0
}
