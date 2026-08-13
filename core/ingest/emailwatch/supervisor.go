package emailwatch

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"strconv"
	"strings"
	"sync"

	"github.com/johnnybravo-xyz/suchi/core/blob"
	"github.com/johnnybravo-xyz/suchi/core/crypto"
	"github.com/johnnybravo-xyz/suchi/core/db"
	"github.com/johnnybravo-xyz/suchi/core/emailaccounts"
	"github.com/johnnybravo-xyz/suchi/core/ingest/emailwatch/oauth"
	"github.com/johnnybravo-xyz/suchi/core/jobs"
)

// Supervisor manages one Watcher goroutine per enabled email_accounts
// row. Reload() is safe to call at any time — after every write to
// the accounts table, an API handler should invoke it so the running
// set matches the DB without a process restart.
//
// The supervisor owns the per-account context lifetime: each Watcher
// runs in a goroutine cancelled by its per-account context. On stop,
// the supervisor waits for each goroutine to drain via done channels.
type Supervisor struct {
	cfg  Config
	db   *db.DB
	cas  *blob.CAS
	disp *jobs.Dispatcher
	aead *crypto.AEADKey
	msal *oauth.Client
	log  *slog.Logger

	mu      sync.Mutex
	ctx     context.Context
	running map[int64]watcherHandle // account_id → handle
}

// watcherHandle is the supervisor's grip on one running Watcher — its
// fingerprint (for restart-on-change detection), its cancel func, and
// a done channel closed when the goroutine returns.
type watcherHandle struct {
	fingerprint string
	cancel      context.CancelFunc
	done        chan struct{}
}

// NewSupervisor constructs an idle supervisor. Call Run to seed and
// block; call Reload from API handlers after email_accounts writes.
//
// msal may be nil — password-only deployments don't need an MSAL
// client; XOAUTH2 accounts will surface a clear error at connect time.
func NewSupervisor(cfg Config, d *db.DB, cas *blob.CAS, disp *jobs.Dispatcher, aead *crypto.AEADKey, msal *oauth.Client, log *slog.Logger) *Supervisor {
	return &Supervisor{
		cfg:     cfg,
		db:      d,
		cas:     cas,
		disp:    disp,
		aead:    aead,
		msal:    msal,
		log:     log.With("component", "emailwatch.supervisor"),
		running: make(map[int64]watcherHandle),
	}
}

// Run seeds the initial watcher set and blocks until ctx is done,
// then stops every watcher and returns.
func (s *Supervisor) Run(ctx context.Context) {
	s.mu.Lock()
	s.ctx = ctx
	s.mu.Unlock()

	if err := s.Reload(ctx); err != nil {
		s.log.Warn("supervisor.reload_failed", "err", err.Error())
	}
	s.log.Info("supervisor.start")

	<-ctx.Done()
	s.log.Info("supervisor.stop", "reason", "context")
	s.stopAll()
}

// Reload diffs the current running set against ListEnabled and starts,
// stops, or restarts watchers accordingly. Safe to call concurrently
// with Run — the internal mutex serializes access to `running`.
func (s *Supervisor) Reload(ctx context.Context) error {
	accounts, err := emailaccounts.ListEnabled(ctx, s.db)
	if err != nil {
		s.log.Warn("supervisor.list_failed", "err", err.Error())
		return err
	}

	desired := make(map[int64]*emailaccounts.Account, len(accounts))
	for i := range accounts {
		a := accounts[i]
		desired[a.ID] = &a
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	// Prefer the supervisor's captured Run ctx as the parent for new
	// watchers so a Run-time cancellation drains everything. Fall back
	// to the caller's ctx (tests / pre-Run Reload).
	parent := s.ctx
	if parent == nil {
		parent = ctx
	}

	// 1. Stop watchers whose account row disappeared or was disabled.
	for id, h := range s.running {
		if _, ok := desired[id]; ok {
			continue
		}
		s.log.Info("supervisor.stop_watcher",
			"account_id", id, "reason", "disabled_or_deleted")
		h.cancel()
		<-h.done
		delete(s.running, id)
	}

	// 2. Restart watchers whose fingerprint changed; leave the rest.
	for id, h := range s.running {
		a := desired[id]
		fp := fingerprint(a)
		if fp == h.fingerprint {
			continue
		}
		s.log.Info("supervisor.restart_watcher", "account_id", id, "reason", "config_changed")
		h.cancel()
		<-h.done
		delete(s.running, id)
		s.startLocked(parent, a)
	}

	// 3. Start watchers we've never seen before.
	for id, a := range desired {
		if _, ok := s.running[id]; ok {
			continue
		}
		s.startLocked(parent, a)
	}
	return nil
}

// startLocked spawns a Watcher goroutine for one account. Caller must
// hold s.mu. A nil Watcher (owner missing / disabled slipped through)
// is silently skipped so the supervisor's map only tracks live rows.
func (s *Supervisor) startLocked(parent context.Context, a *emailaccounts.Account) {
	perCtx, cancel := context.WithCancel(parent)
	w, err := New(perCtx, a, s.cfg, s.db, s.cas, s.disp, s.aead, s.msal, s.log)
	if err != nil {
		cancel()
		s.log.Warn("supervisor.new_watcher_failed",
			"account_id", a.ID, "err", err.Error())
		return
	}
	if w == nil {
		cancel()
		s.log.Info("supervisor.skip_watcher",
			"account_id", a.ID, "reason", "owner_missing_or_disabled")
		return
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		w.Run(perCtx)
	}()
	s.running[a.ID] = watcherHandle{
		fingerprint: fingerprint(a),
		cancel:      cancel,
		done:        done,
	}
	s.log.Info("supervisor.start_watcher", "account_id", a.ID, "account", a.Name)
}

// stopAll cancels every running watcher and waits for their goroutines
// to exit. Called by Run on ctx cancellation.
func (s *Supervisor) stopAll() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, h := range s.running {
		h.cancel()
		<-h.done
		delete(s.running, id)
	}
}

// fingerprint hashes the runtime-relevant fields of an account row so
// Reload can detect changes without a field-by-field diff. UpdatedAt
// alone would work, but including the wire-affecting columns makes
// the intent obvious and survives an accidental touch-only PATCH that
// bumps updated_at without changing behaviour (bump would still cause
// a benign restart in that case — fine).
func fingerprint(a *emailaccounts.Account) string {
	var b strings.Builder
	b.WriteString(a.Host)
	b.WriteByte('\x1f')
	b.WriteString(strconv.Itoa(a.Port))
	b.WriteByte('\x1f')
	if a.UseTLS {
		b.WriteByte('1')
	} else {
		b.WriteByte('0')
	}
	b.WriteByte('\x1f')
	b.WriteString(a.Folder)
	b.WriteByte('\x1f')
	b.WriteString(strconv.Itoa(a.PollIntervalMin))
	b.WriteByte('\x1f')
	b.WriteString(string(a.AuthMethod))
	b.WriteByte('\x1f')
	b.WriteString(a.Username)
	b.WriteByte('\x1f')
	b.WriteString(strconv.FormatInt(a.UpdatedAt, 10))
	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:])
}
