// Demo-mode reset ticker. Sweeps expired scratch users (their docs, CAS
// blobs, sessions, tokens — whatever is FK-cascaded from users.id) so a
// public showcase instance cannot accumulate visitor state.
//
// Scratch users are identified by email pattern: `visitor-*@demo.local`.
// The pattern is load-bearing — the ticker will not touch a real user,
// even one whose created_at is well past the TTL. The scratch-user mint
// endpoint (POST /api/demo/session, follow-up commit) must use the same
// pattern.
//
// Seed rows (documents.original_blob LIKE 'demo:%', users seeded by
// `suchi demo`) are ignored by construction — they don't match the
// scratch-user pattern.

package demo

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"time"

	"github.com/johnnybravo-xyz/suchi/core/blob"
	"github.com/johnnybravo-xyz/suchi/core/db"
)

// ScratchEmailLike is the SQL LIKE pattern that identifies per-visitor
// scratch users. Any user whose email matches this is fair game for the
// reset sweep once they age past the TTL.
const ScratchEmailLike = "visitor-%@demo.local"

// TickerOptions configures the reset loop. Zero DB/CAS is a programmer
// error; zero TTL falls back to 30 minutes.
type TickerOptions struct {
	DB  *db.DB
	CAS *blob.CAS
	Log *slog.Logger
	// TTL is how long a scratch user (and its uploads) survive after
	// creation before being swept. Recommended: 30m for a public demo.
	TTL time.Duration
	// Interval is how often the sweep runs. Recommended: TTL/2 so a
	// row is never more than TTL/2 past its deadline before being
	// deleted. If zero, defaults to TTL/2.
	Interval time.Duration
}

// Loop runs Sweep on a ticker until ctx is cancelled. Spawn once at boot:
//
//	go demo.Loop(ctx, demo.TickerOptions{DB: d, CAS: cas, TTL: 30*time.Minute, Log: log})
//
// The first sweep fires one Interval after start; a boot storm never
// races the seed step.
func Loop(ctx context.Context, opts TickerOptions) {
	if opts.DB == nil || opts.CAS == nil {
		panic("demo.Loop: DB and CAS are required")
	}
	log := opts.Log
	if log == nil {
		log = slog.Default()
	}
	log = log.With("component", "demo.ticker")

	ttl := opts.TTL
	if ttl <= 0 {
		ttl = 30 * time.Minute
	}
	interval := opts.Interval
	if interval <= 0 {
		interval = ttl / 2
		if interval < time.Minute {
			interval = time.Minute
		}
	}

	log.Info("demo.ticker.start", "ttl", ttl, "interval", interval)
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			log.Info("demo.ticker.stop")
			return
		case <-t.C:
			stats, err := Sweep(ctx, opts.DB, opts.CAS, ttl, log)
			if err != nil {
				log.Warn("demo.ticker.err", "err", err.Error())
				continue
			}
			if stats.Users > 0 || stats.Docs > 0 || stats.Blobs > 0 {
				log.Info("demo.ticker.sweep",
					"users", stats.Users,
					"docs", stats.Docs,
					"blobs_deleted", stats.Blobs,
					"blobs_kept", stats.BlobsKept)
			}
		}
	}
}

// SweepStats summarises one sweep pass.
type SweepStats struct {
	Users     int // scratch users deleted
	Docs      int // documents deleted (across all swept users)
	Blobs     int // blobs removed from the CAS
	BlobsKept int // blob hashes referenced by other rows and therefore kept
}

// Sweep runs a single reset pass. Deletes:
//   - documents owned by any scratch user older than TTL,
//   - their blobs from the CAS, IF no other document still references
//     the same hash (dedup-safe),
//   - the scratch users themselves (cascades sessions, tokens, saved
//     views, etc. — every existing FK to users.id).
//
// Runs inside a single write transaction to keep the doc-delete and
// user-delete atomic; the CAS work happens after the txn commits so a
// filesystem hiccup can't leave the DB half-updated. If the CAS delete
// fails, the blob is left in place — `suchi gc` will pick it up.
func Sweep(ctx context.Context, database *db.DB, cas *blob.CAS, ttl time.Duration, log *slog.Logger) (SweepStats, error) {
	var stats SweepStats
	cutoff := time.Now().Add(-ttl).Unix()

	// Two-phase: (1) inside the txn, collect blob hashes owned only by
	// expired scratch users, delete their docs, delete the users. (2)
	// outside the txn, CAS.Delete each collected hash — filesystem work
	// stays off the write lock.
	var toDelete []string
	err := database.WriteTx(ctx, func(tx *sql.Tx) error {
		// User IDs first — one query, small result.
		//
		// disabled=0 filter: an operator who manually quarantines a
		// specific visitor row (rare, but a valid response to abuse)
		// gets to hold onto the row + its docs for forensics. Without
		// this filter, the next tick would silently reap the evidence.
		rows, err := tx.QueryContext(ctx, `
			SELECT id FROM users
			 WHERE email LIKE ? AND created_at < ? AND disabled = 0
		`, ScratchEmailLike, cutoff)
		if err != nil {
			return fmt.Errorf("select scratch users: %w", err)
		}
		var userIDs []int64
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				_ = rows.Close()
				return fmt.Errorf("scan user id: %w", err)
			}
			userIDs = append(userIDs, id)
		}
		if err := rows.Close(); err != nil {
			return err
		}
		if len(userIDs) == 0 {
			return nil
		}

		for _, uid := range userIDs {
			hashes, docCount, err := collectExclusiveBlobs(ctx, tx, uid)
			if err != nil {
				return fmt.Errorf("collect blobs for user %d: %w", uid, err)
			}
			toDelete = append(toDelete, hashes...)
			stats.Docs += docCount

			// Delete docs first — the FK from documents.owner_id is
			// NOT ON DELETE CASCADE, so we can't drop the user with
			// docs still hanging off it.
			if _, err := tx.ExecContext(ctx,
				`DELETE FROM documents WHERE owner_id = ?`, uid); err != nil {
				return fmt.Errorf("delete docs for user %d: %w", uid, err)
			}
			if _, err := tx.ExecContext(ctx,
				`DELETE FROM users WHERE id = ?`, uid); err != nil {
				return fmt.Errorf("delete user %d: %w", uid, err)
			}
			stats.Users++
		}
		return nil
	})
	if err != nil {
		return stats, err
	}

	// CAS work happens after commit — the DB no longer references
	// these hashes; deleting them is safe even if we crash mid-loop.
	for _, sum := range toDelete {
		if !isSHA256Hex(sum) {
			// Skip sentinel keys (demo:* seed docs never survive to
			// this branch because scratch users never own them, but
			// guard anyway).
			stats.BlobsKept++
			continue
		}
		if err := cas.Delete(sum); err != nil {
			log.Warn("demo.ticker.cas.delete_err", "sha256", sum, "err", err.Error())
			stats.BlobsKept++
			continue
		}
		stats.Blobs++
	}
	return stats, nil
}

// collectExclusiveBlobs returns the set of CAS hashes referenced by this
// user's docs AND no others'. Those are safe to CAS.Delete after the doc
// row goes away. Shared hashes are excluded — deleting them would break
// unrelated docs. Also returns the doc count for stats.
func collectExclusiveBlobs(ctx context.Context, tx *sql.Tx, userID int64) ([]string, int, error) {
	// original_blob is NOT NULL; archive_blob may be NULL. UNION over
	// both columns in one pass; filter out NULLs and any that another
	// user's docs also reference.
	rows, err := tx.QueryContext(ctx, `
		SELECT COALESCE(original_blob, ''), COALESCE(archive_blob, '')
		  FROM documents WHERE owner_id = ?
	`, userID)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = rows.Close() }()

	candidates := map[string]struct{}{}
	docCount := 0
	for rows.Next() {
		var orig, arch string
		if err := rows.Scan(&orig, &arch); err != nil {
			return nil, 0, err
		}
		docCount++
		if orig != "" {
			candidates[orig] = struct{}{}
		}
		if arch != "" {
			candidates[arch] = struct{}{}
		}
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	if len(candidates) == 0 {
		return nil, docCount, nil
	}

	// For each candidate, check whether ANY doc owned by a different
	// user references it. Two queries per hash is fine for demo scale
	// (a handful of scratch users per sweep, each with a handful of
	// uploads); no need for a temp table.
	exclusive := make([]string, 0, len(candidates))
	for hash := range candidates {
		var otherRefs int
		if err := tx.QueryRowContext(ctx, `
			SELECT COUNT(*) FROM documents
			 WHERE owner_id != ?
			   AND (original_blob = ? OR archive_blob = ?)
		`, userID, hash, hash).Scan(&otherRefs); err != nil {
			return nil, docCount, err
		}
		if otherRefs == 0 {
			exclusive = append(exclusive, hash)
		}
	}
	return exclusive, docCount, nil
}

// isSHA256Hex — 64 lower-case hex chars. Matches core/blob.validHash
// but duplicated here so this package doesn't reach into that private
// helper. The set is small; the duplication is cheap.
func isSHA256Hex(s string) bool {
	if len(s) != 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}
