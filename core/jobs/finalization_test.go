// SPDX-License-Identifier: AGPL-3.0-or-later

package jobs

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	pluginapi "github.com/johnnybravo-xyz/suchi/plugin-api"
)

type finalizationSubscriber struct {
	kind  string
	err   error
	calls atomic.Int32
}

func (s *finalizationSubscriber) Kinds() []string { return []string{s.kind} }
func (s *finalizationSubscriber) Handle(context.Context, pluginapi.Event) error {
	s.calls.Add(1)
	return s.err
}

type finalizationLog chan string

func (w finalizationLog) Write(p []byte) (int, error) {
	line := string(p)
	select {
	case w <- line:
	default:
	}
	return len(p), nil
}

func waitForFinalizationFailure(t *testing.T, logs finalizationLog, message string) string {
	t.Helper()
	timer := time.NewTimer(3 * time.Second)
	defer timer.Stop()
	for {
		select {
		case line := <-logs:
			if strings.Contains(line, message) {
				return line
			}
		case <-timer.C:
			t.Fatalf("dispatcher did not log %s", message)
		}
	}
}

func installFinalizationFailure(t *testing.T, d interface {
	ExecWrite(context.Context, string, ...any) (sql.Result, error)
}, name string) {
	t.Helper()
	if _, err := d.ExecWrite(t.Context(), fmt.Sprintf(`
		CREATE TRIGGER %s
		BEFORE UPDATE OF state ON jobs
		WHEN OLD.state = 'running' AND NEW.state != 'running'
		BEGIN
			SELECT RAISE(ABORT, 'injected finalization failure');
		END`, name)); err != nil {
		t.Fatal(err)
	}
}

func TestDispatcherRetriesFinalizationWithoutRerunningHandler(t *testing.T) {
	tests := []struct {
		name       string
		handlerErr error
		logMessage string
		wantState  string
		wantTries  int
	}{
		{name: "done", logMessage: "jobs.mark_done.failed", wantState: "done"},
		{name: "retry", handlerErr: errors.New("temporary handler failure"), logMessage: "jobs.mark_retry.failed", wantState: "pending", wantTries: 1},
		{name: "dead", handlerErr: fmt.Errorf("%w: permanent handler failure", ErrTerminal), logMessage: "jobs.mark_dead.failed", wantState: "dead"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			d := openDB(t)
			seedJobDocuments(t, d, 1)
			kind := "finalization-" + test.name
			if err := d.WriteTx(t.Context(), func(tx *sql.Tx) error {
				return Enqueue(t.Context(), tx, kind, 1, 1, "{}")
			}); err != nil {
				t.Fatal(err)
			}
			trigger := "fail_" + test.name + "_finalization"
			installFinalizationFailure(t, d, trigger)

			logs := make(finalizationLog, 16)
			disp := New(d, slog.New(slog.NewTextHandler(logs, nil)))
			subscriber := &finalizationSubscriber{kind: kind, err: test.handlerErr}
			disp.Register(subscriber)
			go disp.Run(t.Context())
			t.Cleanup(disp.Stop)

			waitForFinalizationFailure(t, logs, test.logMessage)
			var blockedState string
			if err := d.Read.QueryRowContext(t.Context(), `SELECT state FROM jobs WHERE kind=?`, kind).Scan(&blockedState); err != nil {
				t.Fatal(err)
			}
			if blockedState != "running" {
				t.Fatalf("state during injected failure = %q, want running", blockedState)
			}
			if _, err := d.ExecWrite(t.Context(), `DROP TRIGGER `+trigger); err != nil {
				t.Fatal(err)
			}
			disp.Nudge()

			deadline := time.NewTimer(2 * time.Second)
			defer deadline.Stop()
			for {
				var state string
				var attempts int
				if err := d.Read.QueryRowContext(t.Context(),
					`SELECT state, attempts FROM jobs WHERE kind=?`, kind).Scan(&state, &attempts); err != nil {
					t.Fatal(err)
				}
				if state == test.wantState {
					if attempts != test.wantTries {
						t.Fatalf("attempts = %d, want %d", attempts, test.wantTries)
					}
					break
				}
				select {
				case <-deadline.C:
					t.Fatalf("state = %q, want %q after removing fault", state, test.wantState)
				case <-time.After(10 * time.Millisecond):
				}
			}
			disp.Stop()
			if got := subscriber.calls.Load(); got != 1 {
				t.Fatalf("handler calls = %d, want 1", got)
			}
		})
	}
}

func TestDispatcherCancelsFinalizationBackoff(t *testing.T) {
	d := openDB(t)
	seedJobDocuments(t, d, 1)
	const kind = "finalization-cancel"
	if err := d.WriteTx(t.Context(), func(tx *sql.Tx) error {
		return Enqueue(t.Context(), tx, kind, 1, 1, "{}")
	}); err != nil {
		t.Fatal(err)
	}
	installFinalizationFailure(t, d, "fail_cancel_finalization")

	logs := make(finalizationLog, 4)
	disp := New(d, slog.New(slog.NewTextHandler(logs, nil)))
	subscriber := &finalizationSubscriber{kind: kind}
	disp.Register(subscriber)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		disp.Run(ctx)
		close(done)
	}()
	waitForFinalizationFailure(t, logs, "jobs.mark_done.failed")
	cancel()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("dispatcher did not stop during finalization backoff")
	}
	var state string
	if err := d.Read.QueryRowContext(t.Context(), `SELECT state FROM jobs WHERE kind=?`, kind).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != "running" {
		t.Fatalf("state after shutdown = %q, want running for boot reclamation", state)
	}
	if got := subscriber.calls.Load(); got != 1 {
		t.Fatalf("handler calls = %d, want 1", got)
	}
}

func TestDispatcherStopAbandonsFinalizationBackoff(t *testing.T) {
	d := openDB(t)
	seedJobDocuments(t, d, 1)
	const kind = "finalization-stop"
	if err := d.WriteTx(t.Context(), func(tx *sql.Tx) error {
		return Enqueue(t.Context(), tx, kind, 1, 1, "{}")
	}); err != nil {
		t.Fatal(err)
	}
	installFinalizationFailure(t, d, "fail_stop_finalization")

	logs := make(finalizationLog, 16)
	disp := New(d, slog.New(slog.NewTextHandler(logs, nil)))
	subscriber := &finalizationSubscriber{kind: kind}
	disp.Register(subscriber)
	go disp.Run(t.Context())
	waitForFinalizationFailure(t, logs, "jobs.mark_done.failed")

	stopped := make(chan struct{})
	go func() {
		disp.Stop()
		close(stopped)
	}()
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("Stop did not interrupt finalization backoff")
	}
	line := waitForFinalizationFailure(t, logs, "jobs.finalize.abandoned")
	if !strings.Contains(line, "reason=stop") || !strings.Contains(line, "intended_state=done") {
		t.Fatalf("abandon log = %q, want reason=stop and intended_state=done", line)
	}
	var state string
	if err := d.Read.QueryRowContext(t.Context(), `SELECT state FROM jobs WHERE kind=?`, kind).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != "running" {
		t.Fatalf("state after stop = %q, want running for boot reclamation", state)
	}
	if got := subscriber.calls.Load(); got != 1 {
		t.Fatalf("handler calls = %d, want 1", got)
	}
}

func TestDispatcherBoundsFinalizationRetries(t *testing.T) {
	d := openDB(t)
	seedJobDocuments(t, d, 1, 2)
	const stuckKind, nextKind = "finalization-stuck", "finalization-next"
	if err := d.WriteTx(t.Context(), func(tx *sql.Tx) error {
		if err := Enqueue(t.Context(), tx, stuckKind, 1, 1, "{}"); err != nil {
			return err
		}
		if err := Enqueue(t.Context(), tx, nextKind, 2, 1, "{}"); err != nil {
			return err
		}
		// Claim order follows next_run_at; run the failing job first.
		_, err := tx.ExecContext(t.Context(),
			`UPDATE jobs SET next_run_at = next_run_at - 10 WHERE kind = ?`, stuckKind)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := d.ExecWrite(t.Context(), `
		CREATE TRIGGER fail_stuck_finalization
		BEFORE UPDATE OF state ON jobs
		WHEN OLD.kind = 'finalization-stuck' AND OLD.state = 'running' AND NEW.state != 'running'
		BEGIN
			SELECT RAISE(ABORT, 'injected finalization failure');
		END`); err != nil {
		t.Fatal(err)
	}

	logs := make(finalizationLog, 64)
	disp := New(d, slog.New(slog.NewTextHandler(logs, nil)))
	stuck := &finalizationSubscriber{kind: stuckKind}
	next := &finalizationSubscriber{kind: nextKind}
	disp.Register(stuck)
	disp.Register(next)
	go disp.Run(t.Context())
	t.Cleanup(disp.Stop)

	line := waitForFinalizationFailure(t, logs, "jobs.finalize.abandoned")
	if !strings.Contains(line, `reason="retries exhausted"`) || !strings.Contains(line, "kind="+stuckKind) {
		t.Fatalf("abandon log = %q, want exhausted retries for %s", line, stuckKind)
	}

	deadline := time.NewTimer(2 * time.Second)
	defer deadline.Stop()
	for {
		var state string
		if err := d.Read.QueryRowContext(t.Context(), `SELECT state FROM jobs WHERE kind=?`, nextKind).Scan(&state); err != nil {
			t.Fatal(err)
		}
		if state == "done" {
			break
		}
		select {
		case <-deadline.C:
			t.Fatalf("next job state = %q, want done after abandoned finalization", state)
		case <-time.After(10 * time.Millisecond):
		}
	}
	disp.Stop()

	var stuckState string
	if err := d.Read.QueryRowContext(t.Context(), `SELECT state FROM jobs WHERE kind=?`, stuckKind).Scan(&stuckState); err != nil {
		t.Fatal(err)
	}
	if stuckState != "running" {
		t.Fatalf("abandoned job state = %q, want running for boot reclamation", stuckState)
	}
	if stuck.calls.Load() != 1 || next.calls.Load() != 1 {
		t.Fatalf("handler calls = %d/%d, want 1/1", stuck.calls.Load(), next.calls.Load())
	}
}

type cancelingSubscriber struct {
	kind   string
	cancel context.CancelFunc
}

func (s *cancelingSubscriber) Kinds() []string { return []string{s.kind} }
func (s *cancelingSubscriber) Handle(context.Context, pluginapi.Event) error {
	s.cancel()
	return nil
}

func TestDispatcherPersistsResultFinishedDuringShutdown(t *testing.T) {
	d := openDB(t)
	seedJobDocuments(t, d, 1)
	const kind = "finalization-shutdown"
	if err := d.WriteTx(t.Context(), func(tx *sql.Tx) error {
		return Enqueue(t.Context(), tx, kind, 1, 1, "{}")
	}); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	disp := New(d, slog.New(slog.DiscardHandler))
	disp.Register(&cancelingSubscriber{kind: kind, cancel: cancel})
	done := make(chan struct{})
	go func() {
		disp.Run(ctx)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("dispatcher did not stop after context cancellation")
	}

	var state string
	if err := d.Read.QueryRowContext(t.Context(), `SELECT state FROM jobs WHERE kind=?`, kind).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != "done" {
		t.Fatalf("state = %q, want done persisted despite shutdown", state)
	}
}
