// Demo-mode reset ticker removes expired visitor records, never CAS bytes.
// The reserved email pattern excludes real users and seeded corpus owners.
// Offline GC or the deployment's stopped-volume reset reclaims physical storage.
package demo

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"time"

	"github.com/johnnybravo-xyz/suchi/core/db"
)

const ScratchEmailLike = "visitor-%@demo.local"

type TickerOptions struct {
	DB  *db.DB
	Log *slog.Logger
	// TTL bounds visitor records and access, not physical blob retention.
	TTL time.Duration
	// Interval defaults to TTL/2, with a one-minute minimum.
	Interval time.Duration
}

// Loop starts after the seed step and runs until cancellation.
func Loop(ctx context.Context, opts TickerOptions) {
	if opts.DB == nil {
		panic("demo.Loop: DB is required")
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
			stats, err := Sweep(ctx, opts.DB, ttl)
			if err != nil {
				log.Warn("demo.ticker.err", "err", err.Error())
				continue
			}
			if stats.Users > 0 || stats.Docs > 0 {
				log.Info("demo.ticker.sweep", "users", stats.Users, "docs", stats.Docs)
			}
		}
	}
}

type SweepStats struct {
	Users int
	Docs  int
}

// Sweep deletes expired visitors and their documents in one transaction.
// Sessions and tokens cascade from the user. Disabled visitors remain available
// for investigation. No CAS reference snapshot can prove that a concurrent
// publisher is not reusing those bytes, so only offline reclamation deletes them.
func Sweep(ctx context.Context, database *db.DB, ttl time.Duration) (SweepStats, error) {
	var stats SweepStats
	cutoff := time.Now().Add(-ttl).Unix()
	err := database.WriteTx(ctx, func(tx *sql.Tx) error {
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
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return fmt.Errorf("iterate user ids: %w", err)
		}
		if err := rows.Close(); err != nil {
			return err
		}

		for _, uid := range userIDs {
			// documents.owner_id does not cascade, so documents go first.
			deleted, err := tx.ExecContext(ctx, "DELETE FROM documents WHERE owner_id = ?", uid)
			if err != nil {
				return fmt.Errorf("delete docs for user %d: %w", uid, err)
			}
			count, err := deleted.RowsAffected()
			if err != nil {
				return fmt.Errorf("count deleted docs for user %d: %w", uid, err)
			}
			stats.Docs += int(count)
			if _, err := tx.ExecContext(ctx, "DELETE FROM users WHERE id = ?", uid); err != nil {
				return fmt.Errorf("delete user %d: %w", uid, err)
			}
			stats.Users++
		}
		return nil
	})
	return stats, err
}
