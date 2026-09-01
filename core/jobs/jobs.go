// Package jobs is the durable outbox dispatcher.
//
// Every unit of asynchronous work in suchi is a row in the jobs table.
// The dispatcher polls that table, hands rows to registered subscribers,
// and updates state on success/failure. In-process nudges shorten the
// poll window; the table remains the source of truth.
//
// Handler contract:
//   - MUST be idempotent (backoff retries do not distinguish duplicates)
//   - MUST return quickly enough that ctx cancellation is honored
//   - Payload is per-kind JSON, documented alongside each subscriber
package jobs

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"math"
	"sync"
	"time"

	"github.com/johnnybravo-xyz/suchi/core/audit"
	"github.com/johnnybravo-xyz/suchi/core/db"
	pluginapi "github.com/johnnybravo-xyz/suchi/plugin-api"
)

// Default backoff/attempt policy. Handlers that need different values can
// PATCH next_run_at/attempts inside their own transaction; the dispatcher
// only owns the baseline.
const (
	MaxAttempts  = 5
	BaseBackoff  = 5 * time.Second
	MaxBackoff   = 30 * time.Minute
	PollInterval = 5 * time.Second
)

// ErrTerminal is the sentinel a Subscriber wraps around an error the
// dispatcher must NOT retry. Use with fmt.Errorf("%w: …", jobs.ErrTerminal,
// …) so the caller can still add context. Common case: an HTTP 4xx from an
// upstream — retrying a bad model name or a permission-denied request
// never turns into success, so short-circuit to dead on the first failure
// instead of burning MaxAttempts.
var ErrTerminal = errors.New("jobs: terminal error; do not retry")

// Dispatcher owns the polling loop and the subscriber registry. One per
// process. Zero value not useful; construct with New.
type Dispatcher struct {
	db      *db.DB
	log     *slog.Logger
	subs    map[string][]pluginapi.Subscriber
	nudge   chan struct{}
	stopCh  chan struct{}
	stopped sync.Once
	wg      sync.WaitGroup
}

// New builds a Dispatcher. Register subscribers before calling Run.
func New(d *db.DB, log *slog.Logger) *Dispatcher {
	return &Dispatcher{
		db:     d,
		log:    log.With("component", "jobs"),
		subs:   map[string][]pluginapi.Subscriber{},
		nudge:  make(chan struct{}, 1),
		stopCh: make(chan struct{}),
	}
}

// ReclaimOrphaned resets any dispatcher jobs left in state='running'
// from a previous process. Called once at boot, before Run — a
// process that crashed (or was SIGKILL'd) mid-handler leaves a
// running row with no worker; without this, that job orphans
// forever and the doc it belonged to sits with no OCR, no content,
// no error.
// Safe to call at boot: single-process by design, and the write pool
// pins one connection, so this and Run() never race.
//
// Returns the number of rows reset. Log at INFO as
// `jobs.boot_reclaimed` so operators see a non-zero count after a
// crash without grepping for it.
func (d *Dispatcher) ReclaimOrphaned(ctx context.Context) (int64, error) {
	var affected int64
	err := d.db.WriteTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `
			UPDATE jobs
			   SET state = 'pending',
			       next_run_at = ?,
			       updated_at = ?
				 WHERE state = 'running'
			`, time.Now().Unix(), time.Now().Unix())
		if err != nil {
			return err
		}
		affected, err = res.RowsAffected()
		return err
	})
	if err != nil {
		return 0, err
	}
	if affected > 0 {
		d.log.Info("jobs.boot_reclaimed",
			"count", affected,
			"reason", "prior process crashed mid-handler")
		// Persist the count so `suchi doctor` can surface it later
		// — the log line is transient; a crash-looping box benefits
		// from seeing the last boot's reap count without journalctl.
		audit.Log(ctx, d.db, d.log, audit.Event{
			Action: "jobs.reclaimed", ObjectKind: "server",
			After: map[string]any{"count": affected},
		})
	}
	return affected, nil
}

// Register wires a subscriber to every kind it declares. Safe to call
// only before Run; the registry is not lock-protected because the
// dispatcher loop reads it without a lock.
func (d *Dispatcher) Register(s pluginapi.Subscriber) {
	for _, k := range s.Kinds() {
		d.subs[k] = append(d.subs[k], s)
	}
}

// Enqueue inserts a job row inside tx. Caller controls the transaction
// so the doc row and its post-ingest job land in the same commit — the
// entire point of a durable outbox.
func Enqueue(ctx context.Context, tx *sql.Tx, kind string, docID int64, payload string) error {
	if payload == "" {
		payload = "{}"
	}
	now := time.Now().Unix()
	_, err := tx.ExecContext(ctx, `
		INSERT INTO jobs(kind, doc_id, payload, state, next_run_at, created_at, updated_at)
		VALUES (?, ?, ?, 'pending', ?, ?, ?)
	`, kind, nullInt64(docID), payload, now, now, now)
	return err
}

// Nudge asks the dispatcher to poll immediately instead of waiting for
// the next tick. Non-blocking; multiple nudges coalesce.
func (d *Dispatcher) Nudge() {
	select {
	case d.nudge <- struct{}{}:
	default:
	}
}

// Run blocks until ctx is done or Stop is called.
func (d *Dispatcher) Run(ctx context.Context) {
	d.wg.Add(1)
	defer d.wg.Done()

	t := time.NewTicker(PollInterval)
	defer t.Stop()

	d.log.Info("jobs.dispatcher.start", "kinds", d.kindList())
	for {
		if err := d.pollOnce(ctx); err != nil && !errors.Is(err, context.Canceled) {
			d.log.Error("jobs.poll.error", "err", err.Error())
		}
		select {
		case <-ctx.Done():
			d.log.Info("jobs.dispatcher.stop", "reason", "context")
			return
		case <-d.stopCh:
			d.log.Info("jobs.dispatcher.stop", "reason", "stop")
			return
		case <-t.C:
		case <-d.nudge:
		}
	}
}

// Stop signals the loop to exit and waits for it. Safe to call multiple
// times.
func (d *Dispatcher) Stop() {
	d.stopped.Do(func() { close(d.stopCh) })
	d.wg.Wait()
}

func (d *Dispatcher) kindList() []string {
	out := make([]string, 0, len(d.subs))
	for k := range d.subs {
		out = append(out, k)
	}
	return out
}

// pollOnce claims one batch of ready jobs and runs each. We claim with
// a single UPDATE...RETURNING so only one dispatcher instance could ever
// grab the same job — future-proofing for the E4 external-worker split.
func (d *Dispatcher) pollOnce(ctx context.Context) error {
	rows, err := d.claim(ctx)
	if err != nil {
		return err
	}
	if len(rows) == 0 {
		return nil
	}
	for _, r := range rows {
		d.runJob(ctx, r)
	}
	return nil
}

type row struct {
	ID       int64
	Kind     string
	DocID    sql.NullInt64
	Payload  string
	Attempts int
}

// claim atomically flips a batch of pending jobs to running and returns them.
// The small batch bounds how many rows can wait behind the serial dispatcher.
func (d *Dispatcher) claim(ctx context.Context) ([]row, error) {
	const batch = 8
	var out []row
	err := d.db.WriteTx(ctx, func(tx *sql.Tx) error {
		q, err := tx.QueryContext(ctx, `
			UPDATE jobs
			   SET state = 'running',
			       updated_at = unixepoch()
			 WHERE id IN (
			     SELECT id FROM jobs
			      WHERE state = 'pending'
				       AND next_run_at <= unixepoch()
			      ORDER BY next_run_at
			      LIMIT ?
			 )
			RETURNING id, kind, doc_id, payload, attempts
		`, batch)
		if err != nil {
			return err
		}
		defer q.Close()
		for q.Next() {
			var r row
			if err := q.Scan(&r.ID, &r.Kind, &r.DocID, &r.Payload, &r.Attempts); err != nil {
				return err
			}
			out = append(out, r)
		}
		return q.Err()
	})
	return out, err
}

func (d *Dispatcher) runJob(ctx context.Context, r row) {
	subs := d.subs[r.Kind]
	if len(subs) == 0 {
		// Unknown kind is a dead-letter — surface it, do not silently drop.
		d.markDead(ctx, r.ID, "no subscriber registered for kind "+r.Kind)
		d.log.Warn("jobs.no_subscriber", "job_id", r.ID, "kind", r.Kind)
		return
	}
	e := pluginapi.Event{
		Kind:  r.Kind,
		DocID: r.DocID.Int64,
		Time:  time.Now(),
	}
	// Payload parsing is per-subscriber; we hand it the raw string via
	// a documented convention (event.Payload["raw"]) so this file stays
	// out of the JSON business.
	e.Payload = map[string]any{"raw": r.Payload}

	var lastErr error
	for _, s := range subs {
		if err := s.Handle(ctx, e); err != nil {
			lastErr = err
			d.log.Error("jobs.handler.error",
				"job_id", r.ID, "kind", r.Kind, "attempts", r.Attempts+1, "err", err.Error())
		}
	}
	if lastErr == nil {
		d.markDone(ctx, r.ID)
		return
	}
	if errors.Is(lastErr, ErrTerminal) {
		d.markDead(ctx, r.ID, lastErr.Error())
		return
	}
	if r.Attempts+1 >= MaxAttempts {
		d.markDead(ctx, r.ID, lastErr.Error())
		return
	}
	d.markRetry(ctx, r.ID, r.Attempts+1, lastErr.Error())
}

func (d *Dispatcher) markDone(ctx context.Context, id int64) {
	if err := d.db.WriteTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
			UPDATE jobs SET state='done', updated_at=unixepoch() WHERE id=?
		`, id)
		return err
	}); err != nil {
		d.log.Error("jobs.mark_done.failed", "job_id", id, "err", err.Error())
	}
}

func (d *Dispatcher) markRetry(ctx context.Context, id int64, attempts int, msg string) {
	backoff := min(time.Duration(math.Pow(2, float64(attempts-1)))*BaseBackoff, MaxBackoff)
	next := time.Now().Add(backoff).Unix()
	if err := d.db.WriteTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
			UPDATE jobs
			   SET state='pending', attempts=?, next_run_at=?, last_error=?, updated_at=unixepoch()
			 WHERE id=?
		`, attempts, next, msg, id)
		return err
	}); err != nil {
		d.log.Error("jobs.mark_retry.failed", "job_id", id, "err", err.Error())
	}
}

func (d *Dispatcher) markDead(ctx context.Context, id int64, msg string) {
	// Snapshot the row so we can carry kind + doc_id + attempts into
	// the audit event. Read is cheap; the write follows in the same
	// tx.
	var (
		kind     string
		docID    sql.NullInt64
		attempts int64
	)
	_ = d.db.Read.QueryRowContext(ctx,
		`SELECT kind, doc_id, attempts FROM jobs WHERE id = ?`,
		id).Scan(&kind, &docID, &attempts)

	if err := d.db.WriteTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
			UPDATE jobs SET state='dead', last_error=?, updated_at=unixepoch() WHERE id=?
		`, msg, id)
		return err
	}); err != nil {
		d.log.Error("jobs.mark_dead.failed", "job_id", id, "err", err.Error())
		return
	}

	// Audit: operator-actionable signal that E-track SIEM exports
	// consume via audit_events. Truncate the error to keep the row
	// bounded — SIEMs choke on multi-KB fields. The full string
	// stays in jobs.last_error.
	truncated := msg
	if len(truncated) > 512 {
		truncated = truncated[:512] + "…(truncated)"
	}
	after := map[string]any{
		"job_id":   id,
		"kind":     kind,
		"attempts": attempts,
		"error":    truncated,
	}
	if docID.Valid {
		after["doc_id"] = docID.Int64
	}
	audit.Log(ctx, d.db, d.log, audit.Event{
		Action: "job.dead", ObjectKind: "job", ObjectID: id,
		After: after,
	})
}

func nullInt64(v int64) any {
	if v == 0 {
		return nil
	}
	return v
}
