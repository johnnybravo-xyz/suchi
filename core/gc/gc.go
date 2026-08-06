// Package gc is the blob mark-and-sweep for the CAS.
//
// A blob is **live** if any documents row (including trashed ones)
// references it. Everything else is a candidate. gc is deliberately
// simple:
//
//  1. Collect every referenced sha from documents.original_blob +
//     documents.archive_blob.
//  2. Walk the CAS via CAS.List().
//  3. Anything in the CAS that isn't in the reference set is a
//     candidate for deletion.
//  4. Optional grace period — skip blobs whose file mtime is newer
//     than time.Now().Add(-grace). Protects freshly-uploaded blobs
//     that haven't been referenced YET because their doc row is still
//     in a pending transaction (should be a millisecond, but a
//     minutes-to-days grace is cheap defense).
//
// gc never touches the DB — it only reads. Actual deletions are on
// the filesystem via CAS.Delete.
//
// Contract: safe against a concurrent uploader as long as the grace
// is longer than the maximum ingest wall-clock. Default grace is
// 30 days — an inbox mailbox that lands a doc at t=0 and gets
// classified at t=29d29h stays safe.
package gc

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/johnnybravo-xyz/suchi/core/blob"
	"github.com/johnnybravo-xyz/suchi/core/db"
	pluginapi "github.com/johnnybravo-xyz/suchi/plugin-api"
)

// Options carries the gc knobs. Zero value is a runnable dry-run.
type Options struct {
	// Grace skips blobs whose mtime is newer than (now - Grace).
	// Zero disables the grace check entirely. Default: 30 days.
	Grace time.Duration

	// Apply actually deletes candidate blobs. Default is dry-run.
	Apply bool

	// Verbose logs every kept + every candidate at Info. Otherwise
	// only summary counters land.
	Verbose bool
}

// Report is what Run returns. Every count refers to blobs (not bytes).
// BytesReclaimable is what --apply would free (or actually freed when
// Apply was true).
type Report struct {
	Referenced       int
	OrphanCandidates int
	Skipped          int // filtered out by Grace
	Deleted          int
	BytesReclaimable int64
	// Failures accumulates per-blob errors (delete failures). Empty
	// in the happy path.
	Failures []Failure
}

// Failure describes a blob we couldn't process.
type Failure struct {
	SHA256 string
	Err    string
}

// Run performs the mark-and-sweep. Idempotent; safe to run on any
// schedule.
func Run(ctx context.Context, d *db.DB, cas *blob.CAS, casRoot string, log *slog.Logger, opts Options) (*Report, error) {
	if opts.Grace == 0 {
		opts.Grace = 30 * 24 * time.Hour
	}
	log = log.With("component", "gc", "apply", opts.Apply, "grace", opts.Grace.String())

	ref, err := collectReferences(ctx, d)
	if err != nil {
		return nil, fmt.Errorf("collect references: %w", err)
	}
	rep := &Report{Referenced: len(ref)}

	cutoff := time.Now().Add(-opts.Grace)

	err = cas.List(func(b pluginapi.BlobRef) error {
		if ref[b.SHA256] {
			return nil
		}
		// Under grace? Skip. Same-device stat is cheap.
		info, statErr := os.Stat(blobPath(casRoot, b.SHA256))
		if statErr != nil {
			// Race with a concurrent delete or scan glitch. Log at debug + skip.
			log.Debug("gc.stat_failed", "sha", b.SHA256, "err", statErr.Error())
			return nil
		}
		if info.ModTime().After(cutoff) {
			rep.Skipped++
			if opts.Verbose {
				log.Info("gc.skip.grace", "sha", b.SHA256, "size", b.Size, "mtime", info.ModTime())
			}
			return nil
		}
		rep.OrphanCandidates++
		rep.BytesReclaimable += b.Size

		if !opts.Apply {
			if opts.Verbose {
				log.Info("gc.candidate", "sha", b.SHA256, "size", b.Size)
			}
			return nil
		}
		if err := cas.Delete(b.SHA256); err != nil {
			rep.Failures = append(rep.Failures, Failure{SHA256: b.SHA256, Err: err.Error()})
			log.Warn("gc.delete_failed", "sha", b.SHA256, "err", err.Error())
			return nil
		}
		rep.Deleted++
		log.Info("gc.deleted", "sha", b.SHA256, "size", b.Size)
		return nil
	})
	if err != nil {
		return rep, fmt.Errorf("walk cas: %w", err)
	}

	log.Info("gc.done",
		"referenced", rep.Referenced,
		"orphan_candidates", rep.OrphanCandidates,
		"skipped_grace", rep.Skipped,
		"deleted", rep.Deleted,
		"bytes_reclaimable", rep.BytesReclaimable,
		"failures", len(rep.Failures),
	)
	return rep, nil
}

// collectReferences walks the documents table (including trashed rows)
// and returns the set of every sha still referenced. Two queries — one
// per column — because SQLite doesn't like UNION with distinct-across-
// nullables as much as separate scans.
func collectReferences(ctx context.Context, d *db.DB) (map[string]bool, error) {
	ref := map[string]bool{}
	scan := func(q string) error {
		rows, err := d.Read.QueryContext(ctx, q)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var s sql.NullString
			if err := rows.Scan(&s); err != nil {
				return err
			}
			if s.Valid && s.String != "" {
				ref[s.String] = true
			}
		}
		return rows.Err()
	}
	if err := scan(`SELECT original_blob FROM documents`); err != nil {
		return nil, err
	}
	if err := scan(`SELECT archive_blob FROM documents WHERE archive_blob IS NOT NULL`); err != nil {
		return nil, err
	}
	return ref, nil
}

// blobPath re-derives the on-disk path for a hash. Duplicates the
// sharding rule from core/blob but we don't want to grow the CAS
// interface just for gc's mtime read. Keep in sync with CAS.path().
func blobPath(casRoot, sum string) string {
	return filepath.Join(casRoot, "blobs", "sha256", sum[0:2], sum[2:4], sum[4:6], sum)
}
