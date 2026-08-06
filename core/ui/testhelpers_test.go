package ui

// Shared test constructors. Small helper file so multiple test
// files in this package can build a Server without duplicating the
// migration + i18n dance.

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/suchi-dms/suchi/core/db"
	migrations "github.com/suchi-dms/suchi/core/db/migrations"
	"github.com/suchi-dms/suchi/core/i18n"
)

// newUISrv returns a fresh Server against a temp-dir SQLite. Every
// migration applies at boot so the read pool sees the full schema.
// Blob CAS is nil — tests that exercise blob paths pass the real
// one via their own setup; the login/redirect tests don't need it.
func newUISrv(t *testing.T) *Server {
	t.Helper()
	ctx := context.Background()
	d, err := db.Open(ctx, filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	migs, err := db.LoadMigrations(migrations.FS, ".")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Migrate(ctx, d, migs, log); err != nil {
		t.Fatal(err)
	}

	cat, err := i18n.Load("en", log)
	if err != nil {
		t.Fatal(err)
	}
	s, err := New(d, nil, cat, log)
	if err != nil {
		t.Fatal(err)
	}
	return s
}
