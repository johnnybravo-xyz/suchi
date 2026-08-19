// Package backup owns the periodic VACUUM INTO snapshot loop. Runs on
// a boot-spawned goroutine, ticks on cfg.BackupInterval, writes
// `$DATA_DIR/backups/suchi-<ts>.db`, and prunes anything beyond
// cfg.BackupKeep so the backups dir can't grow unboundedly and fill
// the volume `suchi.db` itself lives on (which is how "we've had
// backups all along" turns into "the disk is full and nothing writes").
package backup

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/johnnybravo-xyz/suchi/core/audit"
	"github.com/johnnybravo-xyz/suchi/core/db"
	"github.com/johnnybravo-xyz/suchi/core/logx"
)

// Config carries the knobs a backup loop needs. Zero-value Interval
// disables the loop (matches BACKUP_INTERVAL=0 in docs/config.mdx).
type Config struct {
	// DataDir is the parent of the `backups/` directory. Missing dir
	// is created 0700 on first snapshot.
	DataDir string

	// Interval between snapshots. `0` disables the loop.
	Interval time.Duration

	// Keep is the retention window — after each successful snapshot,
	// prune all-but-the-latest N. Zero means "keep everything"
	// (dangerous; documented, not the default).
	Keep int

	// AuditRetentionDays is the sliding window for audit_events.
	// After each snapshot, rows older than N days get deleted.
	// 0 disables audit pruning. Same ticker as Snapshot, single-
	// writer conn, so no separate goroutine.
	AuditRetentionDays int
}

// Scheduler owns the periodic timer and accepts live configuration updates.
// A single Run goroutine performs snapshots, so changing the interval cannot
// overlap two VACUUM operations.
type Scheduler struct {
	mu      sync.RWMutex
	cfg     Config
	version uint64
	changed chan struct{}
}

func NewScheduler(cfg Config) *Scheduler {
	return &Scheduler{cfg: cfg, changed: make(chan struct{}, 1)}
}

// Update replaces the complete schedule and wakes Run. Repeated updates are
// coalesced; Run always reads the newest snapshot after waking.
func (s *Scheduler) Update(cfg Config) {
	s.mu.Lock()
	s.cfg = cfg
	s.version++
	s.mu.Unlock()
	select {
	case s.changed <- struct{}{}:
	default:
	}
}

func (s *Scheduler) config() (Config, uint64) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cfg, s.version
}

// Loop runs snapshots until ctx is cancelled. Intended to be spawned
// once at boot:
//
//	go backup.Loop(ctx, backup.Config{...}, database, log)
//
// The loop takes one snapshot per Interval tick. First snapshot fires
// one Interval after start so a boot storm doesn't slam the disk.
func Loop(ctx context.Context, cfg Config, database *db.DB, log *slog.Logger) {
	if cfg.Interval <= 0 {
		log.Info("backup.disabled", "reason", "backup interval <= 0")
		return
	}
	NewScheduler(cfg).Run(ctx, database, log)
}

// Run blocks until ctx is cancelled and applies Update calls without a process
// restart. The first snapshot under each configuration still occurs one full
// interval after activation.
func (s *Scheduler) Run(ctx context.Context, database *db.DB, log *slog.Logger) {
	log = log.With("component", "backup")
	for {
		cfg, version := s.config()
		var timer *time.Timer
		var tick <-chan time.Time
		if cfg.Interval > 0 {
			log.Info("backup.loop.active", "interval", cfg.Interval, "keep", cfg.Keep)
			timer = time.NewTimer(cfg.Interval)
			tick = timer.C
		} else {
			log.Info("backup.disabled", "reason", "backup interval <= 0")
		}
		select {
		case <-ctx.Done():
			stopTimer(timer)
			log.Info("backup.loop.stop")
			return
		case <-s.changed:
			stopTimer(timer)
			continue
		case <-tick:
			_, latestVersion := s.config()
			if latestVersion != version {
				continue
			}
			if err := Snapshot(ctx, cfg, database, log); err != nil {
				log.Warn("backup.snapshot.err", "err", err.Error())
			}
			// Audit retention piggy-backs on the backup tick — same
			// interval is fine (retention drift of BackupInterval is
			// well within a 20-day window), and reusing the ticker
			// keeps the boot topology one goroutine simpler.
			if cfg.AuditRetentionDays > 0 {
				if _, err := audit.Prune(ctx, database, log, cfg.AuditRetentionDays); err != nil {
					log.Warn("audit.prune.err", "err", err.Error())
				}
			}
		}
	}
}

func stopTimer(timer *time.Timer) {
	if timer == nil || timer.Stop() {
		return
	}
	select {
	case <-timer.C:
	default:
	}
}

// Snapshot runs one VACUUM INTO into DataDir/backups/suchi-<ts>.db,
// audit-logs the result, and prunes older snapshots per Keep. Exported
// so `suchi backup` (a future CLI) and `suchi doctor` can call it too;
// safe to invoke while the server is running (VACUUM INTO takes a
// read snapshot).
func Snapshot(ctx context.Context, cfg Config, database *db.DB, log *slog.Logger) error {
	if err := os.MkdirAll(backupDir(cfg.DataDir), 0o700); err != nil {
		return fmt.Errorf("mkdir backups: %w", err)
	}

	// Timestamped path — UTC + seconds resolution. Nothing else in the
	// tree collides at that granularity.
	stamp := time.Now().UTC().Format("20060102T150405Z")
	target := filepath.Join(backupDir(cfg.DataDir), "suchi-"+stamp+".db")

	// VACUUM INTO takes the read snapshot from the Read pool so the
	// single-writer conn stays available for real work while a large
	// vacuum streams to disk. modernc/sqlite exposes VACUUM INTO via
	// the standard Exec path.
	start := time.Now()
	if _, err := database.Read.ExecContext(ctx, `VACUUM INTO ?`, target); err != nil {
		// Clean up a half-written target — VACUUM INTO leaves nothing
		// on error per SQLite's docs, but the file may or may not
		// exist depending on where the failure hit. Best-effort.
		_ = os.Remove(target)
		return fmt.Errorf("vacuum: %w", err)
	}
	elapsed := time.Since(start)

	// Report the size for logs + audit.
	fi, err := os.Stat(target)
	if err != nil {
		return fmt.Errorf("stat backup: %w", err)
	}

	log.Info("backup.written",
		"path", target, "bytes", fi.Size(), "elapsed", elapsed)
	audit.Log(ctx, database, log, audit.Event{
		Action: "backup.written", ObjectKind: "server",
		After: map[string]any{
			"path": target, "bytes": fi.Size(), "elapsed_ms": elapsed.Milliseconds(),
		},
		RequestID: logx.RequestID(ctx),
	})

	// Retention: prune everything but the latest Keep files.
	if cfg.Keep > 0 {
		if pruned, err := pruneOld(cfg.DataDir, cfg.Keep); err != nil {
			log.Warn("backup.prune.err", "err", err.Error())
		} else if len(pruned) > 0 {
			log.Info("backup.pruned", "count", len(pruned))
		}
	}
	return nil
}

// LastSnapshotAge reports how long ago the newest snapshot in
// DataDir/backups was written. Returns (0, os.ErrNotExist) when the
// dir is empty. `suchi doctor` uses this to WARN when the age is
// substantially past the configured interval.
func LastSnapshotAge(dataDir string) (time.Duration, error) {
	entries, err := listSnapshots(dataDir)
	if err != nil {
		return 0, err
	}
	if len(entries) == 0 {
		return 0, os.ErrNotExist
	}
	newest := entries[len(entries)-1]
	fi, err := os.Stat(filepath.Join(backupDir(dataDir), newest))
	if err != nil {
		return 0, err
	}
	return time.Since(fi.ModTime()), nil
}

// --- helpers ---

func backupDir(dataDir string) string { return filepath.Join(dataDir, "backups") }

// listSnapshots returns filenames of suchi-*.db under $DataDir/backups
// sorted oldest → newest (lex sort works because the stamp format is
// zero-padded).
func listSnapshots(dataDir string) ([]string, error) {
	entries, err := os.ReadDir(backupDir(dataDir))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasPrefix(name, "suchi-") || !strings.HasSuffix(name, ".db") {
			continue
		}
		out = append(out, name)
	}
	sort.Strings(out)
	return out, nil
}

// pruneOld deletes suchi-*.db files older than the newest `keep`.
// Returns the pruned filenames for logging.
func pruneOld(dataDir string, keep int) ([]string, error) {
	all, err := listSnapshots(dataDir)
	if err != nil {
		return nil, err
	}
	if len(all) <= keep {
		return nil, nil
	}
	drop := all[:len(all)-keep]
	pruned := make([]string, 0, len(drop))
	for _, name := range drop {
		p := filepath.Join(backupDir(dataDir), name)
		if err := os.Remove(p); err != nil {
			return pruned, err
		}
		pruned = append(pruned, name)
	}
	return pruned, nil
}
