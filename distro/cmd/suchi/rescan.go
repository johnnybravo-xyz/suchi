// `suchi rescan` — selective, signature-driven re-run of the
// content-extraction pipeline against originals. Thin wrapper over
// core/rescan; the same enqueue path also fires from the approvals-
// engine handler that surfaces stale-doc proposals in the Tasks
// inbox (see core/rescan/handler.go).

package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/johnnybravo-xyz/suchi/core/config"
	"github.com/johnnybravo-xyz/suchi/core/db"
	migrations "github.com/johnnybravo-xyz/suchi/core/db/migrations"
	"github.com/johnnybravo-xyz/suchi/core/logx"
	"github.com/johnnybravo-xyz/suchi/core/pipeline/postingest"
	"github.com/johnnybravo-xyz/suchi/core/rescan"
	llmclassifier "github.com/johnnybravo-xyz/suchi/plugins/llm-classifier"
)

// rescanConfirmThreshold is the count above which rescan prompts the
// operator to confirm. Below this we just run — small selections are
// probably intentional. Above, ask once so a stray filter doesn't
// silently kick off an hours-long OCR run.
const rescanConfirmThreshold = 100

func runRescan(args []string) int {
	fs := flag.NewFlagSet("suchi rescan", flag.ContinueOnError)
	var (
		stale         = fs.String("stale", "", "target docs whose pipeline_version_<kind> < current; kind: ocr | llm | content")
		jdCategory    = fs.Int64("jd", 0, "restrict to a single JD category id")
		tag           = fs.String("tag", "", "restrict to docs carrying this tag (by name)")
		correspondent = fs.String("correspondent", "", "restrict to docs whose primary correspondent is this (by name)")
		olderThan     = fs.Duration("older-than", 0, "restrict to docs created before now-DURATION (e.g. 720h for 30d)")
		newerThan     = fs.Duration("newer-than", 0, "restrict to docs created after now-DURATION")
		onlyFailed    = fs.Bool("only-failed", false, "restrict to docs whose last post-ingest job hit state='dead'")
		onlyNoOCR     = fs.Bool("only-no-ocr", false, "restrict to docs with empty content (never OCR'd or OCR silently failed)")
		sample        = fs.Int("sample", 0, "randomize + cap to N docs from the matching set (for testing)")

		dryRun   = fs.Bool("dry-run", false, "print the affected count + a sample of IDs, then stop — no enqueues")
		estimate = fs.Bool("estimate", false, "in addition to the count, print rough wall-clock + LLM-cost estimates")
		yes      = fs.Bool("yes", false, fmt.Sprintf("skip the confirmation prompt when the affected count exceeds %d", rescanConfirmThreshold))
	)
	if err := fs.Parse(args); err != nil {
		return 2
	}

	opts := rescan.Options{
		Stale:          *stale,
		JDCategory:     *jdCategory,
		Tag:            *tag,
		Correspondent:  *correspondent,
		OlderThan:      *olderThan,
		NewerThan:      *newerThan,
		OnlyFailed:     *onlyFailed,
		OnlyNoOCR:      *onlyNoOCR,
		SampleSize:     *sample,
		OCRVersion:     postingest.PipelineVersionOCR,
		LLMVersion:     llmclassifier.PipelineVersionLLM,
		ContentVersion: postingest.PipelineVersionContent,
	}
	if err := opts.Validate(); err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
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
		fmt.Fprintf(os.Stderr, "open db: %v\n", err)
		return 1
	}
	defer func() { _ = d.Close() }()

	migs, err := db.LoadMigrations(migrations.FS, ".")
	if err != nil {
		fmt.Fprintf(os.Stderr, "load migrations: %v\n", err)
		return 1
	}
	if err := db.Migrate(ctx, d, migs, log); err != nil {
		fmt.Fprintf(os.Stderr, "migrate: %v\n", err)
		return 1
	}

	picks, err := rescan.Select(ctx, d, opts)
	if err != nil {
		fmt.Fprintf(os.Stderr, "select: %v\n", err)
		return 1
	}
	count := len(picks)
	fmt.Printf("rescan: %d documents match the current filter.\n", count)

	if count > 0 {
		limit := 5
		if limit > count {
			limit = count
		}
		ids := make([]string, 0, limit)
		for _, r := range picks[:limit] {
			ids = append(ids, fmt.Sprint(r.ID))
		}
		trail := ""
		if count > limit {
			trail = ", …"
		}
		fmt.Printf("       first %d: %s%s\n", limit, strings.Join(ids, ", "), trail)
	}

	if *estimate {
		printRescanEstimate(picks)
	}
	if *dryRun {
		fmt.Println("dry-run: no jobs enqueued.")
		return 0
	}
	if count == 0 {
		return 0
	}

	if count >= rescanConfirmThreshold && !*yes {
		fmt.Printf("proceed? this will enqueue %d post-ingest jobs. [y/N] ", count)
		var reply string
		_, _ = fmt.Scanln(&reply)
		if !strings.EqualFold(strings.TrimSpace(reply), "y") {
			fmt.Println("aborted.")
			return 0
		}
	}

	// Delegate to the shared helper. It re-runs Select internally so
	// the sample-shuffle we did above for preview doesn't
	// double-shuffle; we discard `picks` for the enqueue path. Small
	// cost (one extra query) for a single source of truth.
	enqueued, err := rescan.Enqueue(ctx, d, opts)
	if err != nil {
		fmt.Fprintf(os.Stderr, "enqueue: %v\n", err)
		return 1
	}
	fmt.Printf("rescan: enqueued %d post-ingest jobs. run `suchi serve` (or the current one will pick them up) to process.\n", enqueued)
	return 0
}

// printRescanEstimate prints coarse wall-clock and LLM-cost
// estimates. Numbers are order-of-magnitude — the point is to warn
// when the operator asked for 5000 docs, not to be precise:
//
//   - "Needs OCR" = HasContent == false. Assume 60s per doc on a
//     mid-range CPU (tesseract single-pass, single-thread) — the
//     dispatcher runs one at a time by default.
//   - "Text-native / metadata-only" (HasContent already set) —
//     roughly 5s per doc (rules + render + thumb).
//   - LLM cost: if the operator has cloud LLM configured, ~$0.005
//     per doc at present-day pricing for a small-model classify
//     call. Skipped when LLM is disabled — we can't easily tell
//     from here, so we print the figure conditionally with a
//     caveat.
func printRescanEstimate(picks []rescan.Row) {
	var needOCR, textNative int
	for _, r := range picks {
		if r.HasContent {
			textNative++
		} else {
			needOCR++
		}
	}
	const ocrSecPerDoc = 60
	const metaSecPerDoc = 5
	wallSeconds := needOCR*ocrSecPerDoc + textNative*metaSecPerDoc
	wall := time.Duration(wallSeconds) * time.Second

	fmt.Println()
	fmt.Println("estimate (coarse — single-threaded dispatcher, no concurrency):")
	fmt.Printf("  needs OCR:       %d docs × ~%ds = %s\n",
		needOCR, ocrSecPerDoc, (time.Duration(needOCR) * ocrSecPerDoc * time.Second).Round(time.Second))
	fmt.Printf("  metadata-only:   %d docs × ~%ds = %s\n",
		textNative, metaSecPerDoc, (time.Duration(textNative) * metaSecPerDoc * time.Second).Round(time.Second))
	fmt.Printf("  wall-clock:      ~%s (single-worker)\n", wall.Round(time.Second))
	fmt.Println("  llm cost:        ~$0.005 × N if the cloud LLM classifier is configured; $0 for a local classifier or when LLM is off")
	fmt.Println()
}
