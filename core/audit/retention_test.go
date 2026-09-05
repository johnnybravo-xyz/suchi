package audit

// Sliding-window retention over audit_events. Verifies:
//   - rows older than the window are gone
//   - rows inside the window survive
//   - a Prune that finds nothing to delete doesn't emit an audit.pruned
//     row (else the log grows by one row per tick forever)
//   - Prune with days<=0 is a no-op

import (
	"context"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/johnnybravo-xyz/suchi/core/db"
)

func insertEventAt(t *testing.T, d *db.DB, ts int64, action string) {
	t.Helper()
	_, err := d.Write.ExecContext(context.Background(), `
		INSERT INTO audit_events(ts, actor_kind, action, object_kind)
		VALUES (?, 'system', ?, 'test')
	`, ts, action)
	if err != nil {
		t.Fatal(err)
	}
}

func countEvents(t *testing.T, d *db.DB) int64 {
	t.Helper()
	var n int64
	if err := d.Read.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM audit_events`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestPrune_WindowDropsOldKeepsNew(t *testing.T) {
	d := setupDB(t)
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))

	now := time.Now().Unix()
	old := now - int64(30*24*time.Hour/time.Second) // 30 days ago
	fresh := now - int64(5*24*time.Hour/time.Second)

	insertEventAt(t, d, old, "document.create")
	insertEventAt(t, d, old, "document.trash")
	insertEventAt(t, d, fresh, "document.update")

	n, err := Prune(context.Background(), d, log, 20)
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if n != 2 {
		t.Errorf("dropped %d, want 2", n)
	}

	// Expect: 1 fresh event + 1 audit.pruned entry recorded by the
	// prune itself = 2 rows.
	if got := countEvents(t, d); got != 2 {
		t.Errorf("post-prune row count = %d, want 2", got)
	}
}

func TestPrune_NoOpNoAuditRow(t *testing.T) {
	d := setupDB(t)
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))

	now := time.Now().Unix()
	insertEventAt(t, d, now, "document.create")

	before := countEvents(t, d)
	n, err := Prune(context.Background(), d, log, 20)
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("prune dropped %d, want 0", n)
	}
	after := countEvents(t, d)
	// No audit.pruned row: a Prune that touches nothing shouldn't
	// spam the log every tick.
	if after != before {
		t.Errorf("row count changed on no-op prune: %d → %d", before, after)
	}
}

func TestPrune_ZeroDaysDisabled(t *testing.T) {
	d := setupDB(t)
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))

	old := time.Now().Unix() - int64(365*24*time.Hour/time.Second)
	insertEventAt(t, d, old, "document.create")

	n, err := Prune(context.Background(), d, log, 0)
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("days=0 should be no-op, dropped %d", n)
	}
	if got := countEvents(t, d); got != 1 {
		t.Errorf("row survived? got %d, want 1", got)
	}
}
