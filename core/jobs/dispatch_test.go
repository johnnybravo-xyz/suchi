package jobs

import (
	"context"
	"database/sql"
	"io"
	"log/slog"
	"testing"
	"time"

	pluginapi "github.com/johnnybravo-xyz/suchi/plugin-api"
)

type dispatchSubscriber struct {
	handle func(context.Context, pluginapi.Event) error
}

func (s dispatchSubscriber) Kinds() []string { return []string{"test-dispatch"} }
func (s dispatchSubscriber) Handle(ctx context.Context, event pluginapi.Event) error {
	return s.handle(ctx, event)
}

func TestRunDrainsReadyJobsBeforeWaiting(t *testing.T) {
	for _, test := range []struct {
		name     string
		initial  int
		expected int
	}{
		{name: "multiple batches", initial: 17, expected: 17},
		{name: "subscriber enqueues next stage", initial: 1, expected: 3},
	} {
		t.Run(test.name, func(t *testing.T) {
			d := openDB(t)
			ctx := t.Context()
			if err := d.WriteTx(ctx, func(tx *sql.Tx) error {
				for id := 1; id <= test.initial; id++ {
					if err := Enqueue(ctx, tx, "test-dispatch", int64(id), "{}"); err != nil {
						return err
					}
				}
				_, err := tx.ExecContext(ctx, `
					INSERT INTO jobs(kind, doc_id, payload, state, next_run_at, created_at, updated_at)
					VALUES ('test-dispatch', 99, '{}', 'pending', unixepoch() + 3600, 0, 0)`)
				return err
			}); err != nil {
				t.Fatal(err)
			}

			handled := make(chan int64, test.expected+1)
			disp := New(d, slog.New(slog.NewTextHandler(io.Discard, nil)))
			disp.Register(dispatchSubscriber{handle: func(ctx context.Context, event pluginapi.Event) error {
				if test.initial == 1 && event.DocID < int64(test.expected) {
					if err := d.WriteTx(ctx, func(tx *sql.Tx) error {
						return Enqueue(ctx, tx, "test-dispatch", event.DocID+1, "{}")
					}); err != nil {
						return err
					}
				}
				handled <- event.DocID
				return nil
			}})
			go disp.Run(ctx)
			t.Cleanup(disp.Stop)

			deadline := time.NewTimer(2 * time.Second)
			defer deadline.Stop()
			seen := make(map[int64]bool, test.expected)
			for len(seen) < test.expected {
				select {
				case id := <-handled:
					if id == 99 {
						t.Fatal("future job ran before next_run_at")
					}
					if seen[id] {
						t.Fatalf("job %d ran twice", id)
					}
					seen[id] = true
				case <-deadline.C:
					t.Fatalf("processed %d/%d ready jobs before polling delay", len(seen), test.expected)
				}
			}
			disp.Stop()

			var done, pending int
			if err := d.Read.QueryRowContext(ctx,
				`SELECT COUNT(*) FILTER (WHERE state = 'done'), COUNT(*) FILTER (WHERE state = 'pending') FROM jobs`).Scan(&done, &pending); err != nil {
				t.Fatal(err)
			}
			if done != test.expected || pending != 1 {
				t.Fatalf("done=%d pending=%d, want %d and one future job", done, pending, test.expected)
			}
		})
	}
}
