package emailwatch

import (
	"context"
	"database/sql"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-imap"
	"github.com/johnnybravo-xyz/suchi/core/emailaccounts"
	"github.com/johnnybravo-xyz/suchi/core/jd"
)

func TestMailboxDedupAndMembershipAreSystemScoped(t *testing.T) {
	ctx := context.Background()
	first := newBatchWatcher(t, "127.0.0.1:993")
	if _, err := first.db.Write.ExecContext(ctx, `
		UPDATE jd_systems SET code = 'S01' WHERE id = 1;
		INSERT INTO jd_systems(id, code, name, taxonomy, created_at, updated_at) VALUES (2, 'S02', 'Second', 'jd', 0, 0);
		INSERT INTO jd_system_members(system_id, user_id, created_at) VALUES (1, 1, 0), (2, 1, 0);
		UPDATE users SET role = 'member' WHERE id = 1;
	`); err != nil {
		t.Fatal(err)
	}
	if err := jd.EnsureBootstrapTree(ctx, first.db, first.log, jd.ModeJD, 2); err != nil {
		t.Fatal(err)
	}
	account := *first.account
	account.ID = 0
	account.SystemID = 2
	var created *emailaccounts.Account
	if err := first.db.WriteTx(ctx, func(tx *sql.Tx) error {
		var err error
		created, err = emailaccounts.Create(ctx, tx, account, nil)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	second, err := New(ctx, created, Config{}, first.db, first.cas, nil, first.aead, nil, first.log)
	if err != nil || second == nil {
		t.Fatalf("second watcher: %v", err)
	}
	raw := []byte("From: sender@example.test\r\nMessage-Id: <shared@example.test>\r\nContent-Type: text/plain\r\n\r\nSame message bytes\r\n")
	message := &imap.Message{Envelope: &imap.Envelope{Subject: "Shared"}}
	for _, watcher := range []*Watcher{first, second} {
		outcome, err := watcher.importOne(ctx, raw, "<shared@example.test>", message)
		if err != nil || outcome != outcomeImported {
			t.Fatalf("first import: outcome=%v err=%v", outcome, err)
		}
		outcome, err = watcher.importOne(ctx, raw, "<shared@example.test>", message)
		if err != nil || outcome != outcomeDeduplicated {
			t.Fatalf("message retry: outcome=%v err=%v", outcome, err)
		}
		outcome, err = watcher.importOne(ctx, raw, "", message)
		if err != nil || outcome != outcomeDeduplicated {
			t.Fatalf("blob retry: outcome=%v err=%v", outcome, err)
		}
	}
	var count, blobs int
	if err := first.db.Read.QueryRowContext(ctx, `SELECT COUNT(*), COUNT(DISTINCT original_blob) FROM documents`).Scan(&count, &blobs); err != nil {
		t.Fatal(err)
	}
	if count != 2 || blobs != 1 {
		t.Fatalf("cross-system mailbox intake: docs=%d blobs=%d", count, blobs)
	}
	if _, err := first.db.Write.ExecContext(ctx, `DELETE FROM jd_system_members WHERE system_id = 2 AND user_id = 1`); err != nil {
		t.Fatal(err)
	}
	if _, err := second.importOne(ctx, raw, "<shared@example.test>", message); err == nil {
		t.Fatal("revoked owner replayed stale mailbox intake")
	}
}

func TestQueuedMailboxIntakeRechecksCurrentAdmission(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change string
		replay bool
		want   string
	}{
		{
			name:   "membership removed before insert",
			change: `DELETE FROM jd_system_members WHERE system_id = 1 AND user_id = 1`,
			want:   "owner cannot enter system",
		},
		{
			name:   "disabled owner cannot record another replay source",
			change: `UPDATE users SET disabled = 1 WHERE id = 1`,
			replay: true,
			want:   "owner cannot enter system",
		},
		{
			name:   "mailbox reassigned to another eligible owner",
			change: `UPDATE email_accounts SET owner_id = 2 WHERE id = ?`,
			want:   "account unavailable",
		},
		{
			name:   "mailbox disabled before insert",
			change: `UPDATE email_accounts SET enabled = 0 WHERE id = ?`,
			want:   "account unavailable",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			w := newBatchWatcher(t, "127.0.0.1:993")
			if _, err := w.db.Write.ExecContext(ctx, `
				UPDATE jd_systems SET code = 'S01' WHERE id = 1;
				INSERT INTO users(id, email, display_name, role, created_at, updated_at)
				VALUES (2, 'replacement@example.test', 'Replacement', 'member', 0, 0);
				INSERT INTO jd_system_members(system_id, user_id, created_at) VALUES (1, 1, 0), (1, 2, 0);
				UPDATE users SET role = 'member' WHERE id = 1;
			`); err != nil {
				t.Fatal(err)
			}
			raw := []byte("From: sender@example.test\r\nMessage-Id: <queued@example.test>\r\nContent-Type: text/plain\r\n\r\nQueued message\r\n")
			message := &imap.Message{Envelope: &imap.Envelope{Subject: "Queued"}}
			wantCount := 0
			if tc.replay {
				outcome, err := w.importOne(ctx, raw, "<queued@example.test>", message)
				if err != nil || outcome != outcomeImported {
					t.Fatalf("seed import: %v, %v", outcome, err)
				}
				wantCount = 1
				// A replay would add this distinct acquisition detail if its
				// writer turn trusted the earlier authenticated owner.
				w.account.Folder = "Another folder"
			}
			blocker, err := w.db.Write.BeginTx(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer blocker.Rollback()
			waitCount := w.db.Write.Stats().WaitCount
			done := make(chan error, 1)
			go func() {
				_, err := w.importOne(ctx, raw, "<queued@example.test>", message)
				done <- err
			}()
			// The single-connection writer pool is the barrier: wait until
			// this intake has finished CAS/preflight and actually queued.
			for w.db.Write.Stats().WaitCount == waitCount {
				select {
				case err := <-done:
					t.Fatalf("intake returned before writer turn: %v", err)
				case <-ctx.Done():
					t.Fatal("intake did not queue for writer")
				default:
					runtime.Gosched()
				}
			}
			var args []any
			if strings.Contains(tc.change, "?") {
				args = []any{w.account.ID}
			}
			if _, err := blocker.ExecContext(ctx, tc.change, args...); err != nil {
				t.Fatal(err)
			}
			if err := blocker.Commit(); err != nil {
				t.Fatal(err)
			}
			select {
			case err := <-done:
				if err == nil || !strings.Contains(err.Error(), tc.want) {
					t.Fatalf("queued stale intake: got %v, want %q", err, tc.want)
				}
			case <-ctx.Done():
				t.Fatal("queued intake did not finish")
			}
			var documents, sources, jobs int
			if err := w.db.Read.QueryRowContext(ctx, `
				SELECT (SELECT COUNT(*) FROM documents), (SELECT COUNT(*) FROM document_sources),
				       (SELECT COUNT(*) FROM jobs WHERE kind = 'post-ingest')
			`).Scan(&documents, &sources, &jobs); err != nil {
				t.Fatal(err)
			}
			if documents != wantCount || sources != wantCount || jobs != wantCount {
				t.Fatalf("stale intake changed committed state: documents=%d sources=%d jobs=%d, want each %d", documents, sources, jobs, wantCount)
			}
		})
	}
}
