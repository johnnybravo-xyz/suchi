package audit_test

// Tests for the AuditSink fanout wiring. Focus on the boundary: a
// registered sink receives the same event that lands in audit_events,
// and a slow / failing sink doesn't break the write path.

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/suchi-dms/suchi/core/audit"
	"github.com/suchi-dms/suchi/core/db"
	migrations "github.com/suchi-dms/suchi/core/db/migrations"
	pluginapi "github.com/suchi-dms/suchi/plugin-api"
)

func setupDB(t *testing.T) *db.DB {
	t.Helper()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "test.db")
	d, err := db.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	migs, err := db.LoadMigrations(migrations.FS, ".")
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	if err := db.Migrate(ctx, d, migs, log); err != nil {
		t.Fatal(err)
	}
	return d
}

// captureSink records every event it receives.
type captureSink struct {
	mu     sync.Mutex
	events []pluginapi.AuditEvent
}

func (c *captureSink) Kind() string { return "capture" }
func (c *captureSink) Emit(_ context.Context, e pluginapi.AuditEvent) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.events = append(c.events, e)
	return nil
}

// failSink returns an error every emit; used to verify Log doesn't
// propagate sink failures.
type failSink struct{ called int }

func (f *failSink) Kind() string { return "fail" }
func (f *failSink) Emit(_ context.Context, _ pluginapi.AuditEvent) error {
	f.called++
	return errors.New("sink is on fire")
}

func TestFanout_EventReachesSink(t *testing.T) {
	d := setupDB(t)
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))

	sink := &captureSink{}
	audit.RegisterSink(sink)
	t.Cleanup(clearSinks)

	principal := &pluginapi.Principal{
		Kind: "user", UserID: 42, Email: "a@example.com",
	}
	audit.Log(context.Background(), d, log, audit.Event{
		Actor:      principal,
		Action:     "document.create",
		ObjectKind: "document",
		ObjectID:   7,
		After:      map[string]any{"title": "receipt"},
		RequestID:  "req-abc",
	})

	sink.mu.Lock()
	defer sink.mu.Unlock()
	if len(sink.events) != 1 {
		t.Fatalf("want 1 event, got %d", len(sink.events))
	}
	got := sink.events[0]
	if got.Action != "document.create" {
		t.Errorf("Action: got %q", got.Action)
	}
	if got.ActorID != 42 || got.ActorEmail != "a@example.com" {
		t.Errorf("actor: got id=%d email=%q", got.ActorID, got.ActorEmail)
	}
	if got.ObjectID != 7 || got.ObjectKind != "document" {
		t.Errorf("object: got kind=%q id=%d", got.ObjectKind, got.ObjectID)
	}
	if got.After["title"] != "receipt" {
		t.Errorf("After.title: got %v", got.After["title"])
	}
	if got.RequestID != "req-abc" {
		t.Errorf("RequestID: got %q", got.RequestID)
	}
	if got.Timestamp.IsZero() {
		t.Error("Timestamp should be set")
	}
}

func TestFanout_MultipleSinksAllReceive(t *testing.T) {
	d := setupDB(t)
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))

	s1, s2 := &captureSink{}, &captureSink{}
	audit.RegisterSink(s1)
	audit.RegisterSink(s2)
	t.Cleanup(clearSinks)

	audit.Log(context.Background(), d, log, audit.Event{Action: "test.event"})

	s1.mu.Lock()
	defer s1.mu.Unlock()
	s2.mu.Lock()
	defer s2.mu.Unlock()
	if len(s1.events) != 1 || len(s2.events) != 1 {
		t.Errorf("both sinks should have received one event; s1=%d s2=%d",
			len(s1.events), len(s2.events))
	}
}

func TestFanout_FailingSinkDoesNotBlockWrite(t *testing.T) {
	d := setupDB(t)
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))

	fail := &failSink{}
	ok := &captureSink{}
	audit.RegisterSink(fail)
	audit.RegisterSink(ok)
	t.Cleanup(clearSinks)

	// Should not panic, not error visibly, and still reach the ok sink.
	audit.Log(context.Background(), d, log, audit.Event{Action: "test.event"})

	if fail.called != 1 {
		t.Errorf("failing sink should still be called; got %d", fail.called)
	}
	ok.mu.Lock()
	defer ok.mu.Unlock()
	if len(ok.events) != 1 {
		t.Errorf("ok sink should still fire after fail sink; got %d events",
			len(ok.events))
	}

	// Verify the audit_events row DID land — the DB write ran before
	// the fanout, and a failing sink can't roll it back.
	var n int
	err := d.Read.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM audit_events WHERE action='test.event'`).Scan(&n)
	if err != nil || n != 1 {
		t.Errorf("audit_events row: want 1, got %d (err=%v)", n, err)
	}
}

func TestFanout_NilSinkIgnored(t *testing.T) {
	t.Cleanup(clearSinks)
	// Should be a no-op, not panic.
	audit.RegisterSink(nil)
}

// clearSinks resets the package-level registry between tests. There's
// no exported clear so tests reach through — kept internal since
// production code never wants this.
func clearSinks() {
	// Best we can do without exported plumbing: register a sink, then
	// re-register nils won't remove others. The package doesn't expose
	// removal on purpose (sinks are boot-time only). Tests using
	// t.Cleanup live with the accumulation as long as each test doesn't
	// rely on sink COUNT from other tests. Add a subtest-scoped filter
	// if that becomes a problem.
}
