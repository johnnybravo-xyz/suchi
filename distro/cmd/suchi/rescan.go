// `suchi rescan` — selective, signature-driven re-run of the
// content-extraction pipeline against originals.
//
// The mental model: filters select a set of live docs; for each, we
// enqueue a fresh `post-ingest` job carrying the doc's original-blob
// SHA + size + mime. When `suchi serve` is running (or the next boot
// wakes the dispatcher up) those jobs drain through the same chain a
// fresh upload runs: qpdf → text-native / OCR → content write →
// rules → automations → render → thumb → post-classify (if the LLM
// classifier is wired). All idempotent — re-running a rescan against
// the same doc is a no-op except for updated `pipeline_version_*`
// columns.
//
// This is the tool the operator reaches for after:
//   - swapping OCR engine (`--stale ocr`)
//   - upgrading the LLM classifier model or prompt (`--stale llm`)
//   - fixing a broken pre-consume script (`--only-failed`)
//   - discovering docs with empty content (`--only-no-ocr`)
//   - onboarding an archive that predates a new custom-field / rule
//     (composed filters, e.g. `--jd 22 --older-than 90d`)
//
// Explicit + previewable + resumable. No auto-rescan.

package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"math/rand"
	"os"
	"strings"
	"time"

	"github.com/johnnybravo-xyz/suchi/core/config"
	"github.com/johnnybravo-xyz/suchi/core/db"
	migrations "github.com/johnnybravo-xyz/suchi/core/db/migrations"
	"github.com/johnnybravo-xyz/suchi/core/jobs"
	"github.com/johnnybravo-xyz/suchi/core/logx"
	"github.com/johnnybravo-xyz/suchi/core/pipeline/postingest"
)

// rescanConfirmThreshold is the count above which rescan prompts the
// operator to confirm. Below this we just run — small selections are
// probably intentional. Above, ask once so a stray filter doesn't
// silently kick off an hours-long OCR run.
const rescanConfirmThreshold = 100

// rescanRow is the projection the selection query reads for each
// affected doc. Only enough to reconstruct a postIngestPayload plus a
// few fields the estimate math needs.
type rescanRow struct {
	ID     int64
	SHA256 string
	Size   int64
	MIME   string
	// HasContent — used by the estimate to guess "will this doc need
	// OCR (empty content today) or is it just re-applying rules?"
	HasContent bool
}

func runRescan(args []string) int {
	fs := flag.NewFlagSet("suchi rescan", flag.ContinueOnError)
	var (
		// Selection filters (compose with AND).
		stale         = fs.String("stale", "", "target docs whose pipeline_version_<kind> < current; kind: ocr | llm | content")
		jdCategory    = fs.Int64("jd", 0, "restrict to a single JD category id")
		tag           = fs.String("tag", "", "restrict to docs carrying this tag (by name)")
		correspondent = fs.String("correspondent", "", "restrict to docs whose primary correspondent is this (by name)")
		olderThan     = fs.Duration("older-than", 0, "restrict to docs created before now-DURATION (e.g. 720h for 30d)")
		newerThan     = fs.Duration("newer-than", 0, "restrict to docs created after now-DURATION")
		onlyFailed    = fs.Bool("only-failed", false, "restrict to docs whose last post-ingest job hit state='dead'")
		onlyNoOCR     = fs.Bool("only-no-ocr", false, "restrict to docs with empty content (never OCR'd or OCR silently failed)")
		sample        = fs.Int("sample", 0, "randomize + cap to N docs from the matching set (for testing)")

		// Pre-flight.
		dryRun   = fs.Bool("dry-run", false, "print the affected count + a sample of IDs, then stop — no enqueues")
		estimate = fs.Bool("estimate", false, "in addition to the count, print rough wall-clock + LLM-cost estimates")
		yes      = fs.Bool("yes", false, fmt.Sprintf("skip the confirmation prompt when the affected count exceeds %d", rescanConfirmThreshold))
	)
	if err := fs.Parse(args); err != nil {
		return 2
	}

	if err := validateStale(*stale); err != nil {
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

	where, whereArgs := buildRescanFilters(*stale, *jdCategory, *tag, *correspondent,
		*olderThan, *newerThan, *onlyFailed, *onlyNoOCR)

	picks, err := fetchRescanPicks(ctx, d, where, whereArgs)
	if err != nil {
		fmt.Fprintf(os.Stderr, "select: %v\n", err)
		return 1
	}

	// --sample: shuffle + truncate. math/rand's default is fine — this
	// is a preview convenience, not cryptography.
	if *sample > 0 && len(picks) > *sample {
		rand.Shuffle(len(picks), func(i, j int) { picks[i], picks[j] = picks[j], picks[i] })
		picks = picks[:*sample]
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

	// Enqueue in batches of 200. One tx per batch — a crash mid-run
	// leaves the outbox consistent (partial progress is fine, jobs are
	// resumable across reboots).
	const batchSize = 200
	enqueued := 0
	for i := 0; i < len(picks); i += batchSize {
		end := i + batchSize
		if end > len(picks) {
			end = len(picks)
		}
		batch := picks[i:end]
		if err := d.WriteTx(ctx, func(tx *sql.Tx) error {
			for _, r := range batch {
				payload, err := json.Marshal(map[string]any{
					"sha256":    r.SHA256,
					"size":      r.Size,
					"mime_type": r.MIME,
				})
				if err != nil {
					return err
				}
				if err := jobs.Enqueue(ctx, tx, postingest.Kind, r.ID, string(payload)); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			fmt.Fprintf(os.Stderr, "enqueue batch %d: %v\n", i/batchSize, err)
			return 1
		}
		enqueued += len(batch)
		fmt.Printf("       enqueued %d / %d\n", enqueued, count)
	}

	fmt.Printf("rescan: enqueued %d post-ingest jobs. run `suchi serve` (or the current one will pick them up) to process.\n", enqueued)
	return 0
}

// validateStale accepts empty (no --stale filter) or one of the
// enumerated kinds. Anything else is a user error.
func validateStale(kind string) error {
	switch kind {
	case "", "ocr", "llm", "content":
		return nil
	}
	return fmt.Errorf("--stale must be one of ocr, llm, content (got %q)", kind)
}

// buildRescanFilters composes an SQL WHERE fragment plus its bind
// args. Every filter is optional. Empty filter set → no fragment,
// which selects every live doc.
func buildRescanFilters(
	stale string, jdCat int64, tag, correspondent string,
	olderThan, newerThan time.Duration,
	onlyFailed, onlyNoOCR bool,
) (string, []any) {
	var b strings.Builder
	var args []any

	if stale != "" {
		col, cur := staleColumnAndVersion(stale)
		if col != "" {
			b.WriteString(" AND d." + col + " < ?")
			args = append(args, cur)
		}
	}
	if jdCat > 0 {
		b.WriteString(" AND d.jd_category_id = ?")
		args = append(args, jdCat)
	}
	if tag != "" {
		b.WriteString(` AND EXISTS (SELECT 1 FROM document_tags dt JOIN tags t ON t.id = dt.tag_id
			WHERE dt.document_id = d.id AND t.name = ?)`)
		args = append(args, tag)
	}
	if correspondent != "" {
		b.WriteString(` AND EXISTS (SELECT 1 FROM correspondents c
			WHERE c.id = d.correspondent_id AND c.name = ?)`)
		args = append(args, correspondent)
	}
	if olderThan > 0 {
		b.WriteString(" AND d.created_at < ?")
		args = append(args, time.Now().Add(-olderThan).Unix())
	}
	if newerThan > 0 {
		b.WriteString(" AND d.created_at > ?")
		args = append(args, time.Now().Add(-newerThan).Unix())
	}
	if onlyFailed {
		b.WriteString(` AND EXISTS (SELECT 1 FROM jobs j
			WHERE j.doc_id = d.id AND j.kind = 'post-ingest' AND j.state = 'dead')`)
	}
	if onlyNoOCR {
		b.WriteString(" AND COALESCE(d.content, '') = ''")
	}
	return b.String(), args
}

// staleColumnAndVersion maps a `--stale <kind>` value to the
// documents column + the current binary's version constant.
func staleColumnAndVersion(kind string) (string, int) {
	switch kind {
	case "ocr":
		return "pipeline_version_ocr", postingest.PipelineVersionOCR
	case "content":
		return "pipeline_version_content", postingest.PipelineVersionContent
	case "llm":
		// The LLM version constant lives in the plugin (kept out of
		// core to keep the dep graph flat). Hard-code the match here
		// — if the plugin bumps, bump this too. Cross-checked in the
		// plugin's package constant.
		return "pipeline_version_llm", 1
	}
	return "", 0
}

func fetchRescanPicks(ctx context.Context, d *db.DB, where string, args []any) ([]rescanRow, error) {
	rows, err := d.Read.QueryContext(ctx, `
		SELECT d.id, COALESCE(d.original_blob, ''), COALESCE(d.original_size, 0),
		       COALESCE(d.mime_type, ''),
		       CASE WHEN COALESCE(d.content, '') = '' THEN 0 ELSE 1 END
		FROM documents d
		WHERE d.trashed_at IS NULL`+where+`
		ORDER BY d.id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []rescanRow
	for rows.Next() {
		var r rescanRow
		var hasContent int
		if err := rows.Scan(&r.ID, &r.SHA256, &r.Size, &r.MIME, &hasContent); err != nil {
			return nil, err
		}
		r.HasContent = hasContent == 1
		out = append(out, r)
	}
	return out, rows.Err()
}

// printRescanEstimate prints coarse wall-clock and cost estimates.
// Numbers are order-of-magnitude — the point is to warn the operator
// when they asked for 5000 docs, not to be precise:
//
//   - "Needs OCR" = HasContent == false. Assume 60s per doc on a
//     mid-range CPU (tesseract single-pass, single-thread) — the
//     dispatcher runs one at a time by default.
//   - "Text-native / metadata-only" (HasContent already set) —
//     roughly 5s per doc (rules + render + thumb).
//   - LLM cost: if the operator has cloud LLM configured, ~$0.005
//     per doc at present-day pricing for a small-model classify
//     call. Skipped when LLM is disabled — we can't easily tell from
//     here, so we print the figure conditionally with a caveat.
func printRescanEstimate(picks []rescanRow) {
	var needOCR, textNative int
	for _, r := range picks {
		if r.HasContent {
			textNative++
		} else {
			needOCR++
		}
	}
	// Coarse numbers — override in the printout below by adjusting
	// the two constants.
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
