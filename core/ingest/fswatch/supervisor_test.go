package fswatch

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"testing"

	"github.com/johnnybravo-xyz/suchi/core/blob"
	"github.com/johnnybravo-xyz/suchi/core/db"
	migrations "github.com/johnnybravo-xyz/suchi/core/db/migrations"
	"github.com/johnnybravo-xyz/suchi/core/jobs"
)

func TestSupervisorReloadsWatcherWithoutRestart(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d, err := db.Open(ctx, filepath.Join(t.TempDir(), "suchi.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	migs, err := db.LoadMigrations(migrations.FS, ".")
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	if err := db.Migrate(ctx, d, migs, log); err != nil {
		t.Fatal(err)
	}
	if _, err := d.ExecWrite(ctx, `
		INSERT INTO users(id, email, display_name, role, created_at, updated_at)
		VALUES (1, 'owner@example.test', 'Owner', 'admin', 0, 0)
	`); err != nil {
		t.Fatal(err)
	}
	cas, err := blob.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s := NewSupervisor(ctx, d, cas, jobs.New(d, log), log)
	t.Cleanup(s.Stop)

	first := Config{Dir: filepath.Join(t.TempDir(), "first"), OwnerEmail: "owner@example.test"}
	if err := s.Reload(ctx, first); err != nil {
		t.Fatal(err)
	}
	firstDone := s.done
	if firstDone == nil {
		t.Fatal("first watcher did not start")
	}
	if err := s.Reload(ctx, first); err != nil {
		t.Fatal(err)
	}
	if s.done != firstDone {
		t.Fatal("unchanged configuration restarted the watcher")
	}

	second := Config{Dir: filepath.Join(t.TempDir(), "second"), OwnerEmail: "owner@example.test"}
	if err := s.Reload(ctx, second); err != nil {
		t.Fatal(err)
	}
	select {
	case <-firstDone:
	default:
		t.Fatal("old watcher was still running after reload returned")
	}
	if s.done == nil || s.done == firstDone {
		t.Fatal("replacement watcher did not start")
	}

	bad := Config{Dir: filepath.Join(t.TempDir(), "missing"), OwnerEmail: "missing@example.test"}
	if err := s.Reload(ctx, bad); !errors.Is(err, ErrOwnerNotFound) {
		t.Fatalf("missing owner error = %v", err)
	}
	if s.current != second {
		t.Fatal("invalid reload replaced the working configuration")
	}
}
