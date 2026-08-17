package jobs

// Terminal-error short-circuit: a subscriber that wraps ErrTerminal must
// take the job straight to state=dead on the first failure — no retry
// budget spent. Everything else (network errors, 5xx, transient DB blips)
// stays on the MaxAttempts retry ladder.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"testing"

	pluginapi "github.com/johnnybravo-xyz/suchi/plugin-api"
)

type failingSubscriber struct {
	kind string
	err  error
}

func (f *failingSubscriber) Kinds() []string { return []string{f.kind} }
func (f *failingSubscriber) Handle(_ context.Context, _ pluginapi.Event) error {
	return f.err
}

func TestRunJob_ErrTerminalGoesStraightToDead(t *testing.T) {
	d := openDB(t)
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	disp := New(d, log)
	disp.Register(&failingSubscriber{
		kind: "test-terminal",
		err:  fmt.Errorf("%w: HTTP 404", ErrTerminal),
	})

	ctx := context.Background()
	if err := d.WriteTx(ctx, func(tx *sql.Tx) error {
		return Enqueue(ctx, tx, "test-terminal", 42, `{}`)
	}); err != nil {
		t.Fatal(err)
	}

	rows, err := disp.claim(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("claim returned %d rows, want 1", len(rows))
	}
	disp.runJob(ctx, rows[0])

	var state string
	var attempts int
	if err := d.Read.QueryRow(
		`SELECT state, attempts FROM jobs WHERE id = ?`, rows[0].ID).Scan(&state, &attempts); err != nil {
		t.Fatal(err)
	}
	if state != "dead" {
		t.Errorf("state = %q, want dead", state)
	}
	if attempts != 0 {
		// runJob never bumps attempts on the terminal path — it goes
		// straight to markDead without incrementing.
		t.Errorf("attempts = %d, want 0 (no retry spent)", attempts)
	}
}

// A plain (non-terminal) error still takes the normal retry path.
// Sanity check that the short-circuit is scoped to ErrTerminal only.
func TestRunJob_RetryableErrorTakesNormalPath(t *testing.T) {
	d := openDB(t)
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	disp := New(d, log)
	disp.Register(&failingSubscriber{
		kind: "test-retryable",
		err:  errors.New("network blip"),
	})

	ctx := context.Background()
	if err := d.WriteTx(ctx, func(tx *sql.Tx) error {
		return Enqueue(ctx, tx, "test-retryable", 43, `{}`)
	}); err != nil {
		t.Fatal(err)
	}
	rows, _ := disp.claim(ctx)
	if len(rows) != 1 {
		t.Fatalf("claim returned %d rows, want 1", len(rows))
	}
	disp.runJob(ctx, rows[0])

	var state string
	var attempts int
	if err := d.Read.QueryRow(
		`SELECT state, attempts FROM jobs WHERE id = ?`, rows[0].ID).Scan(&state, &attempts); err != nil {
		t.Fatal(err)
	}
	if state != "pending" {
		t.Errorf("state = %q, want pending (retry)", state)
	}
	if attempts != 1 {
		t.Errorf("attempts = %d, want 1", attempts)
	}
}
