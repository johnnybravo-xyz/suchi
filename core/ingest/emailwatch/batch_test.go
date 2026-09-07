package emailwatch

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-imap"

	"github.com/johnnybravo-xyz/suchi/core/blob"
	"github.com/johnnybravo-xyz/suchi/core/crypto"
	"github.com/johnnybravo-xyz/suchi/core/db"
	"github.com/johnnybravo-xyz/suchi/core/db/migrations"
	"github.com/johnnybravo-xyz/suchi/core/emailaccounts"
	"github.com/johnnybravo-xyz/suchi/core/jd"
)

func TestCycleBatchesAndResumes(t *testing.T) {
	const total = 105
	uid := func(index int) uint32 { return 1_000_000_000 + uint32(index)*3 }
	for _, failure := range []string{"", "fetch", "message", "move", "missing_still_present", "vanished"} {
		name := failure
		if name == "" {
			name = "success"
		}
		t.Run(name, func(t *testing.T) {
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = listener.Close() })
			uids := make([]uint32, total)
			for i := range uids {
				uids[i] = uid(i)
			}
			type result struct {
				batches [][]uint32
				err     error
			}
			results := make(chan result, 2)
			go func() {
				failed := false
				for range 2 {
					conn, err := listener.Accept()
					if err != nil {
						results <- result{err: err}
						return
					}
					batches, err := serveBatchMailbox(conn, uids, failure, &failed)
					_ = conn.Close()
					results <- result{batches: batches, err: err}
					if err != nil {
						return
					}
				}
			}()

			w := newBatchWatcher(t, listener.Addr().String())
			if failure == "move" {
				w.account.ProcessedFolder = "Processed"
			}
			for round := range 2 {
				w.runCycle(context.Background())
				var observed result
				select {
				case observed = <-results:
				case <-time.After(5 * time.Second):
					t.Fatal("IMAP server did not complete")
				}
				if observed.err != nil {
					t.Fatal(observed.err)
				}
				account, err := emailaccounts.Get(context.Background(), w.db, w.account.ID)
				if err != nil {
					t.Fatal(err)
				}
				wantCursor, wantDocuments := uid(total-1), total
				if failure == "vanished" {
					wantDocuments--
				}
				wantError := round == 0 && failure != "" && failure != "vanished"
				if wantError {
					wantCursor = uid(imapFetchBatchSize - 1)
					switch failure {
					case "fetch":
						wantDocuments = imapFetchBatchSize + 1
					case "move":
						wantDocuments = imapFetchBatchSize * 2
					case "message", "missing_still_present":
						wantCursor = uid(imapFetchBatchSize) - 1
						wantDocuments = imapFetchBatchSize*2 - 1
					}
					if len(observed.batches) != 2 {
						t.Fatalf("requested %d batches after failure, want 2", len(observed.batches))
					}
				}
				if account.LastUIDSeen != wantCursor || (account.LastError != "") != wantError {
					t.Fatalf("round %d: cursor=%d error=%q; want cursor=%d error=%v", round, account.LastUIDSeen, account.LastError, wantCursor, wantError)
				}
				if (account.LastSyncAt != 0) == wantError {
					t.Fatalf("round %d: wrong successful sync timestamp %d", round, account.LastSyncAt)
				}
				var documents, jobs int
				if err := w.db.Read.QueryRow("SELECT count(*) FROM documents").Scan(&documents); err != nil {
					t.Fatal(err)
				}
				if err := w.db.Read.QueryRow("SELECT count(*) FROM jobs WHERE kind = 'post-ingest'").Scan(&jobs); err != nil {
					t.Fatal(err)
				}
				if documents != wantDocuments || jobs != wantDocuments {
					t.Fatalf("round %d: documents=%d jobs=%d; want %d", round, documents, jobs, wantDocuments)
				}
				if round == 1 && wantDocuments == total && failure != "" {
					if len(observed.batches) == 0 || observed.batches[0][0] != uid(imapFetchBatchSize) {
						t.Fatalf("retry did not resume at the failed batch: %v", observed.batches)
					}
				}
				if round == 1 && (failure == "" || failure == "vanished") && len(observed.batches) != 0 {
					t.Fatal("completed history was fetched again")
				}
				// Reload persisted state as a watcher restart would, not just in-memory progress.
				w.account = account
				if failure == "move" {
					w.account.ProcessedFolder = "Processed"
				}
			}
		})
	}
}

func newBatchWatcher(t *testing.T, address string) *Watcher {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	database, err := db.Open(ctx, filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	migs, err := db.LoadMigrations(migrations.FS, ".")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Migrate(ctx, database, migs, log); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Write.Exec("INSERT INTO users(id, email, display_name, role, created_at, updated_at) VALUES (1, 'test@example.com', 'Test', 'admin', 0, 0)"); err != nil {
		t.Fatal(err)
	}
	if err := jd.EnsureBootstrapTree(ctx, database, log, jd.ModeJD); err != nil {
		t.Fatal(err)
	}
	cas, err := blob.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	key, err := crypto.LoadOrCreateKey(filepath.Join(dir, "key"))
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := emailaccounts.SealPassword(key, "test-password")
	if err != nil {
		t.Fatal(err)
	}
	host, portText, err := net.SplitHostPort(address)
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		t.Fatal(err)
	}
	account, err := emailaccounts.Create(ctx, database, emailaccounts.Account{
		Name: "test", OwnerID: 1, Provider: emailaccounts.ProviderCustom,
		Host: host, Port: port, Folder: "INBOX", PollIntervalMin: 10,
		AuthMethod: emailaccounts.AuthPassword, Username: "test", SealedSecret: sealed,
		Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	w, err := New(ctx, account, Config{}, database, cas, nil, key, nil, log)
	if err != nil {
		t.Fatal(err)
	}
	return w
}

// A real IMAP wire exchange catches oversized UID commands and checkpoint ordering.
// SEARCH deliberately returns descending UIDs; FETCH uses normal ascending order.
func serveBatchMailbox(conn net.Conn, uids []uint32, failure string, failed *bool) ([][]uint32, error) {
	_ = conn.SetDeadline(time.Now().Add(15 * time.Second))
	if _, err := fmt.Fprint(conn, "* OK [CAPABILITY IMAP4rev1 MOVE] ready\r\n"); err != nil {
		return nil, err
	}
	var batches [][]uint32
	scanner := bufio.NewScanner(conn)
	for scanner.Scan() {
		line := scanner.Text()
		fields := strings.Fields(line)
		if len(fields) < 2 {
			return batches, fmt.Errorf("invalid command %q", line)
		}
		tag, command := fields[0], fields[1]
		var err error
		switch command {
		case "CAPABILITY":
			_, err = fmt.Fprint(conn, "* CAPABILITY IMAP4rev1 MOVE\r\n")
		case "LOGIN":
		case "SELECT":
			_, err = fmt.Fprintf(conn, "* FLAGS (\\Seen)\r\n* %d EXISTS\r\n* OK [UIDVALIDITY 14] valid\r\n", len(uids))
		case "UID":
			if len(fields) < 4 {
				return batches, fmt.Errorf("invalid UID command %q", line)
			}
			setText := fields[3]
			if fields[2] == "SEARCH" {
				for i, field := range fields {
					if field == "UID" && i+1 < len(fields) {
						setText = fields[i+1]
					}
				}
			}
			set, parseErr := imap.ParseSeqSet(setText)
			if parseErr != nil {
				return batches, parseErr
			}
			var matched []uint32
			for _, uid := range uids {
				if set.Contains(uid) && !(failure == "vanished" && *failed && uid == uids[imapFetchBatchSize]) {
					matched = append(matched, uid)
				}
			}
			switch fields[2] {
			case "SEARCH":
				if len(matched) == 0 && strings.Contains(setText, "*") {
					matched = []uint32{uids[len(uids)-1]} // IMAP N:* can include the old maximum.
				}
				slices.Reverse(matched)
				var response strings.Builder
				response.WriteString("* SEARCH")
				for _, uid := range matched {
					fmt.Fprintf(&response, " %d", uid)
				}
				_, err = fmt.Fprintf(conn, "%s\r\n", response.String())
			case "FETCH":
				if len(line) > 1024 || len(matched) > imapFetchBatchSize {
					return batches, fmt.Errorf("oversized FETCH: %d bytes, %d messages", len(line), len(matched))
				}
				batches = append(batches, matched)
				inject := len(batches) == 2 && failure != "" && !*failed
				if inject {
					*failed = true
				}
				for i, uid := range matched {
					if inject && i == 0 {
						switch failure {
						case "message":
							_, err = fmt.Fprintf(conn, "* 1 FETCH (UID %d)\r\n", uid)
							continue
						case "missing_still_present", "vanished":
							continue
						}
					}
					body := fmt.Sprintf("From: sender@example.com\r\nSubject: Test %d\r\n\r\nSynthetic mail %d\r\n", uid, uid)
					_, err = fmt.Fprintf(conn, "* %d FETCH (UID %d BODY[] {%d}\r\n%s)\r\n", i+1, uid, len(body), body)
					if err != nil || inject && failure == "fetch" {
						break
					}
				}
				if err == nil && inject && failure == "fetch" {
					_, err = fmt.Fprintf(conn, "%s BAD Command Error. 10\r\n", tag)
					if err != nil {
						return batches, err
					}
					continue
				}
				if err == nil && inject && (failure == "missing_still_present" || failure == "vanished") {
					_, err = fmt.Fprintf(conn, "%s NO Some of the requested messages no longer exist.\r\n", tag)
					if err != nil {
						return batches, err
					}
					continue
				}
			case "MOVE":
				if len(batches) == 2 && failure == "move" && len(matched) > 0 && matched[0] == uids[imapFetchBatchSize] {
					// Fail only the first attempt; the retry's first batch is the failed batch.
					_, err = fmt.Fprintf(conn, "%s NO move failed\r\n", tag)
					if err != nil {
						return batches, err
					}
					continue
				}
			default:
				return batches, fmt.Errorf("unexpected UID command %q", line)
			}
		case "LOGOUT":
			_, err = fmt.Fprintf(conn, "* BYE closing\r\n%s OK logout\r\n", tag)
			return batches, err
		default:
			return batches, fmt.Errorf("unexpected command %q", line)
		}
		if err != nil {
			return batches, err
		}
		if _, err := fmt.Fprintf(conn, "%s OK completed\r\n", tag); err != nil {
			return batches, err
		}
	}
	return batches, scanner.Err()
}
