package fswatch

import (
	"context"
	"fmt"
	"log/slog"
	"sync"

	"github.com/johnnybravo-xyz/suchi/core/blob"
	"github.com/johnnybravo-xyz/suchi/core/db"
	"github.com/johnnybravo-xyz/suchi/core/jobs"
)

// Supervisor owns the single filesystem watcher and replaces it when setup
// changes its directory or owner. New configurations validate before the old
// watcher is stopped.
type Supervisor struct {
	parent context.Context
	db     *db.DB
	cas    *blob.CAS
	disp   *jobs.Dispatcher
	log    *slog.Logger

	mu         sync.Mutex
	configured bool
	current    Config
	cancel     context.CancelFunc
	done       chan struct{}
}

func NewSupervisor(parent context.Context, d *db.DB, cas *blob.CAS, disp *jobs.Dispatcher, log *slog.Logger) *Supervisor {
	return &Supervisor{parent: parent, db: d, cas: cas, disp: disp, log: log}
}

// Reload validates cfg, waits for the previous watcher to stop, and starts the
// replacement. An unchanged live configuration is a no-op.
func (s *Supervisor) Reload(ctx context.Context, cfg Config) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.configured && cfg == s.current {
		if cfg.OwnerEmail == "" || (s.cancel != nil && !closed(s.done)) {
			return nil
		}
	}
	watcher, err := New(ctx, cfg, s.db, s.cas, s.disp, s.log)
	if err != nil {
		return err
	}
	if err := s.stopLocked(ctx); err != nil {
		return err
	}

	s.current = cfg
	s.configured = true
	if watcher == nil {
		return nil
	}
	runCtx, cancel := context.WithCancel(s.parent)
	done := make(chan struct{})
	s.cancel, s.done = cancel, done
	go func() {
		defer close(done)
		watcher.Run(runCtx)
	}()
	return nil
}

func (s *Supervisor) Stop() {
	s.mu.Lock()
	defer s.mu.Unlock()
	_ = s.stopLocked(context.Background())
	s.configured = false
}

func (s *Supervisor) stopLocked(ctx context.Context) error {
	if s.cancel == nil {
		s.done = nil
		return nil
	}
	s.cancel()
	done := s.done
	select {
	case <-done:
		s.cancel, s.done = nil, nil
		return nil
	case <-ctx.Done():
		return fmt.Errorf("stop filesystem watcher: %w", ctx.Err())
	}
}

func closed(done <-chan struct{}) bool {
	if done == nil {
		return false
	}
	select {
	case <-done:
		return true
	default:
		return false
	}
}
