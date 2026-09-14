package audit

// Audit sink payloads preserve the plugin wire contract and reach every sink.
// Deferred records must not escape before commit or after rollback/write failure.

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/johnnybravo-xyz/suchi/core/db"
	migrations "github.com/johnnybravo-xyz/suchi/core/db/migrations"
	pluginapi "github.com/johnnybravo-xyz/suchi/plugin-api"
)

func setupDB(t *testing.T) *db.DB {
	t.Helper()
	clearSinks()
	t.Cleanup(clearSinks)
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
	events []pluginapi.AuditEvent
	check  func(context.Context, pluginapi.AuditEvent) error
}

func (c *captureSink) Kind() string { return "capture" }
func (c *captureSink) Emit(ctx context.Context, e pluginapi.AuditEvent) error {
	c.events = append(c.events, e)
	if c.check != nil {
		return c.check(ctx, e)
	}
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
	RegisterSink(sink)

	principal := &pluginapi.Principal{
		Kind: "user", UserID: 42, Email: "a@example.com",
	}
	Log(context.Background(), d, log, Event{
		Actor:      principal,
		Action:     "document.update",
		ObjectKind: "document",
		ObjectID:   7,
		Before: struct {
			Title string `json:"title"`
		}{Title: "draft"},
		After:     map[string]any{"title": "receipt"},
		RequestID: "req-abc",
	})

	if len(sink.events) != 1 {
		t.Fatalf("want 1 event, got %d", len(sink.events))
	}
	got := sink.events[0]
	if got.Action != "document.update" {
		t.Errorf("Action: got %q", got.Action)
	}
	if got.ActorID != 42 || got.ActorEmail != "a@example.com" {
		t.Errorf("actor: got id=%d email=%q", got.ActorID, got.ActorEmail)
	}
	if got.ObjectID != 7 || got.ObjectKind != "document" {
		t.Errorf("object: got kind=%q id=%d", got.ObjectKind, got.ObjectID)
	}
	if got.Before["title"] != "draft" || got.After["title"] != "receipt" {
		t.Errorf("title projection: before=%v after=%v", got.Before["title"], got.After["title"])
	}
	if got.RequestID != "req-abc" {
		t.Errorf("RequestID: got %q", got.RequestID)
	}
	var timestamp int64
	if err := d.Read.QueryRow(`SELECT ts FROM audit_events WHERE action='document.update'`).Scan(&timestamp); err != nil {
		t.Fatal(err)
	}
	if !got.Timestamp.Equal(time.Unix(timestamp, 0)) {
		t.Errorf("sink timestamp %v differs from persisted timestamp %d", got.Timestamp, timestamp)
	}
}

func TestFanout_MultipleSinksAllReceive(t *testing.T) {
	d := setupDB(t)
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))

	s1, s2 := &captureSink{}, &captureSink{}
	RegisterSink(s1)
	RegisterSink(s2)

	Log(context.Background(), d, log, Event{Action: "test.event"})

	if len(s1.events) != 1 || len(s2.events) != 1 {
		t.Errorf("both sinks should have received one event; s1=%d s2=%d",
			len(s1.events), len(s2.events))
	}
}

func TestRecordInTxDefersSinksUntilCommit(t *testing.T) {
	for _, outcome := range []string{"commit", "rollback", "audit_failure"} {
		t.Run(outcome, func(t *testing.T) {
			d := setupDB(t)
			if outcome == "audit_failure" {
				if _, err := d.Write.Exec(`CREATE TRIGGER fail_audit BEFORE INSERT ON audit_events
					BEGIN SELECT RAISE(ABORT, 'forced audit failure'); END`); err != nil {
					t.Fatal(err)
				}
			}
			ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
			defer cancel()
			log := slog.New(slog.NewTextHandler(os.Stderr, nil))
			var observedErr error
			sink := &captureSink{check: func(ctx context.Context, event pluginapi.AuditEvent) error {
				// A sink can take the writer and observe the committed record.
				// Calling it inside RecordInTx would block behind that transaction.
				observedErr = d.WriteTx(ctx, func(tx *sql.Tx) error {
					var n int
					if err := tx.QueryRowContext(ctx,
						`SELECT count(*) FROM audit_events WHERE action=?`, event.Action).Scan(&n); err != nil {
						return err
					}
					if n != 1 {
						return errors.New("sink observed an uncommitted event")
					}
					return nil
				})
				return observedErr
			}}
			RegisterSink(sink)
			rejected := errors.New("reject state change")
			var record Record
			err := d.WriteTx(ctx, func(tx *sql.Tx) error {
				record = RecordInTx(ctx, tx, Event{Action: "state.transition"})
				if outcome == "rollback" {
					return rejected
				}
				return nil
			})
			if outcome == "rollback" {
				if !errors.Is(err, rejected) {
					t.Fatalf("rollback: %v", err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			if len(sink.events) != 0 {
				t.Fatal("recording emitted an event before the commit decision")
			}
			want := 0
			if outcome != "rollback" {
				record.Emit(ctx, log)
			}
			if outcome == "commit" {
				want = 1
			}
			var stored int
			if err := d.Read.QueryRowContext(ctx,
				`SELECT count(*) FROM audit_events WHERE action='state.transition'`).Scan(&stored); err != nil {
				t.Fatal(err)
			}
			if stored != want || len(sink.events) != want || observedErr != nil {
				t.Fatalf("stored=%d delivered=%d want=%d observer=%v",
					stored, len(sink.events), want, observedErr)
			}
		})
	}
}

func TestFanout_FailingSinkDoesNotBlockWrite(t *testing.T) {
	d := setupDB(t)
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))

	fail := &failSink{}
	ok := &captureSink{}
	RegisterSink(fail)
	RegisterSink(ok)

	// Should not panic, not error visibly, and still reach the ok sink.
	Log(context.Background(), d, log, Event{Action: "test.event"})

	if fail.called != 1 {
		t.Errorf("failing sink should still be called; got %d", fail.called)
	}
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

// Tests run serially because the sink registry is process-wide.
func clearSinks() {
	sinkMu.Lock()
	sinks = nil
	sinkMu.Unlock()
}
