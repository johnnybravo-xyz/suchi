package index

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/johnnybravo-xyz/suchi/core/db"
	"github.com/johnnybravo-xyz/suchi/core/db/migrations"
	"github.com/johnnybravo-xyz/suchi/core/jd/presetfile"
	"github.com/johnnybravo-xyz/suchi/core/jd/systems"
	"github.com/johnnybravo-xyz/suchi/core/jobs"
	"github.com/johnnybravo-xyz/suchi/core/render/paths"
	pluginapi "github.com/johnnybravo-xyz/suchi/plugin-api"
)

func TestIndexJobUsesCurrentTreeAndPreservesCompleteFileOnFailure(t *testing.T) {
	d, log := indexDB(t)
	ctx := t.Context()
	root := t.TempDir()
	h := New(d, log, root)
	if err := d.WriteTx(ctx, func(tx *sql.Tx) error { return Enqueue(ctx, tx, 1) }); err != nil {
		t.Fatal(err)
	}
	// A queued job never captures a stale source tree.
	if _, err := d.ExecWrite(ctx, `UPDATE jd_categories SET description='Latest description' WHERE code=11`); err != nil {
		t.Fatal(err)
	}
	dispatcher := jobs.New(d, log)
	dispatcher.Register(h)
	runContext, cancel := context.WithTimeout(ctx, 10*time.Second)
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		dispatcher.Run(runContext)
	}()
	t.Cleanup(func() { cancel(); <-finished })
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		var state string
		if err := d.Read.QueryRowContext(ctx, `SELECT state FROM jobs WHERE kind=?`, Kind).Scan(&state); err != nil {
			t.Fatal(err)
		}
		if state == "done" {
			break
		}
		select {
		case <-runContext.Done():
			t.Fatalf("queued index never completed: state=%s", state)
		case <-tick.C:
		}
	}
	cancel()
	<-finished
	path := filepath.Join(root, paths.IndexDirectory, Filename)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	pf, err := presetfile.Parse(before, presetfile.FormatHuML)
	if err != nil {
		t.Fatal(err)
	}
	if pf.ID != "archive" || pf.Seeds != nil || len(pf.Areas[0].Categories[0].Keywords) != 0 || pf.Areas[0].Categories[0].Description != "Latest description" {
		t.Fatalf("index contents: %+v", pf)
	}
	var docs, archiveJobs int
	if err := d.Read.QueryRow(`SELECT COUNT(*) FROM documents`).Scan(&docs); err != nil {
		t.Fatal(err)
	}
	if err := d.Read.QueryRow(`SELECT COUNT(*) FROM jobs WHERE kind=? AND doc_id IS NULL`, Kind).Scan(&archiveJobs); err != nil {
		t.Fatal(err)
	}
	if docs != 0 || archiveJobs != 1 {
		t.Fatalf("document rows=%d archive jobs=%d", docs, archiveJobs)
	}
	if _, err := d.ExecWrite(ctx, `UPDATE jd_categories SET name='Legacy inbox' WHERE code=49`); err != nil {
		t.Fatal(err)
	}
	if err := h.Handle(ctx, pluginapi.Event{SystemID: 1, Payload: map[string]any{"raw": `{"system_id":1}`}}); err == nil {
		t.Fatal("legacy tree exported")
	}
	after, err := os.ReadFile(path)
	if err != nil || string(after) != string(before) {
		t.Fatalf("failed refresh changed last complete file: %v", err)
	}
	if _, err := d.ExecWrite(ctx, `UPDATE jd_categories SET name='Inbox' WHERE code=49`); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := h.Handle(ctx, pluginapi.Event{SystemID: 1, Payload: map[string]any{"raw": `{"system_id":1}`}}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal("missing index not recovered", err)
	}
}

func TestIndexEnqueueRollsBackWithMutation(t *testing.T) {
	d, _ := indexDB(t)
	abort := errors.New("abort")
	err := d.WriteTx(t.Context(), func(tx *sql.Tx) error {
		if err := Enqueue(t.Context(), tx, 1); err != nil {
			return err
		}
		return abort
	})
	if !errors.Is(err, abort) {
		t.Fatal(err)
	}
	var count int
	if err := d.Read.QueryRow(`SELECT COUNT(*) FROM jobs WHERE kind=?`, Kind).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatal("rolled-back import left projection work")
	}
}

func TestIndexRefusesSymlinksAndUnexpectedFiles(t *testing.T) {
	for _, kind := range []string{"parent", "target", "user file", "target directory", "root"} {
		t.Run(kind, func(t *testing.T) {
			root := t.TempDir()
			outside := t.TempDir()
			sentinel := filepath.Join(outside, "sentinel")
			if err := os.WriteFile(sentinel, []byte("private"), 0o600); err != nil {
				t.Fatal(err)
			}
			dir := filepath.Join(root, paths.IndexDirectory)
			if kind == "root" {
				alias := filepath.Join(t.TempDir(), "root")
				if err := os.Symlink(root, alias); err != nil {
					t.Fatal(err)
				}
				root = alias
			} else if kind == "parent" {
				if err := os.Symlink(outside, dir); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := os.Mkdir(dir, 0o750); err != nil {
					t.Fatal(err)
				}
				target := filepath.Join(dir, Filename)
				switch kind {
				case "target":
					if err := os.Symlink(sentinel, target); err != nil {
						t.Fatal(err)
					}
				case "user file":
					if err := os.WriteFile(target, []byte("user content"), 0o600); err != nil {
						t.Fatal(err)
					}
				case "target directory":
					if err := os.Mkdir(target, 0o750); err != nil {
						t.Fatal(err)
					}
				}
			}
			if err := writeIndex(t.Context(), root, "", []byte(generatedHeader+"new")); err == nil {
				t.Fatal("unsafe destination accepted")
			}
			if data, err := os.ReadFile(sentinel); err != nil || string(data) != "private" {
				t.Fatalf("outside file changed: %q %v", data, err)
			}
			if kind == "user file" {
				data, err := os.ReadFile(filepath.Join(dir, Filename))
				if err != nil || string(data) != "user content" {
					t.Fatal("user file overwritten")
				}
			}
		})
	}
}

func TestIndexAtomicReplacementAndCancellation(t *testing.T) {
	root := t.TempDir()
	old := []byte(generatedHeader + "old")
	if err := writeIndex(t.Context(), root, "", old); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, paths.IndexDirectory)
	path := filepath.Join(dir, Filename)
	// Unrelated files and abandoned temporaries are not cleanup targets.
	stray := filepath.Join(dir, ".taxonomy-index-abandoned.tmp")
	if err := os.WriteFile(stray, []byte("preserve"), 0o600); err != nil {
		t.Fatal(err)
	}
	if os.Geteuid() != 0 {
		if err := os.Chmod(dir, 0o500); err != nil {
			t.Fatal(err)
		}
		defer os.Chmod(dir, 0o750)
		if err := writeIndex(t.Context(), root, "", []byte(generatedHeader+"new")); err == nil {
			t.Fatal("unwritable directory reported a successful refresh")
		}
		if data, err := os.ReadFile(path); err != nil || string(data) != string(old) {
			t.Fatal("write failure lost the previous complete index")
		}
		if err := os.Chmod(dir, 0o750); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := writeIndex(ctx, root, "", []byte(generatedHeader+"new")); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(path); err != nil || string(data) != string(old) {
		t.Fatal("cancellation lost complete index")
	}
	if err := writeIndex(t.Context(), root, "", []byte(generatedHeader+"new")); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(path); err != nil || !strings.HasSuffix(string(data), "new") {
		t.Fatal("refresh did not replace file")
	}
	if data, err := os.ReadFile(stray); err != nil || string(data) != "preserve" {
		t.Fatal("refresh touched unrelated file")
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o027 != 0 {
		t.Fatalf("permissive index mode: %v", info.Mode())
	}
}

func indexDB(t *testing.T) (*db.DB, *slog.Logger) {
	t.Helper()
	ctx := t.Context()
	d, err := db.Open(ctx, filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	m, err := db.LoadMigrations(migrations.FS, ".")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Migrate(ctx, d, m, log); err != nil {
		t.Fatal(err)
	}
	if _, err := d.ExecWrite(ctx, `INSERT INTO jd_areas(system_id,code_start,code_end,name,position) VALUES(1,10,19,'Records',0),(1,40,49,'System',1);
	INSERT INTO jd_categories(system_id,id,area_start,code,name,system,description) VALUES(1,1,10,11,'Identity',0,'Original description'),(1,2,40,49,'Inbox',1,'');
	UPDATE jd_systems SET inbox_category_id=2 WHERE id=1;`); err != nil {
		t.Fatal(err)
	}
	return d, log
}

func TestNamedIndexesAreIndependentAndOnlyOriginalCleansLegacy(t *testing.T) {
	d, log := indexDB(t)
	ctx := t.Context()
	root := t.TempDir()
	h := New(d, log, root)
	event := func(id int64) pluginapi.Event {
		return pluginapi.Event{SystemID: id, Payload: map[string]any{"raw": fmt.Sprintf(`{"system_id":%d}`, id)}}
	}
	if err := h.Handle(ctx, event(1)); err != nil {
		t.Fatal(err)
	}
	legacy := filepath.Join(root, paths.IndexDirectory, Filename)
	var other int64
	if err := d.WriteTx(ctx, func(tx *sql.Tx) error {
		if err := systems.SetCode(ctx, tx, 1, "S01", 1); err != nil {
			return err
		}
		if err := systems.Rename(ctx, tx, 1, "Audit firm", 1); err != nil {
			return err
		}
		var err error
		other, err = systems.Create(ctx, tx, "S02", "Advisory firm", "jd", 1)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO jd_areas(system_id,code_start,code_end,name,position) SELECT ?,code_start,code_end,name,position FROM jd_areas WHERE system_id=1`, other); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO jd_categories(system_id,area_start,code,name,system,description) SELECT ?,area_start,code,name,system,description FROM jd_categories WHERE system_id=1`, other); err != nil {
			return err
		}
		var inbox int64
		if err := tx.QueryRowContext(ctx, `SELECT id FROM jd_categories WHERE system_id=? AND code=49`, other).Scan(&inbox); err != nil {
			return err
		}
		return systems.SetInbox(ctx, tx, other, inbox, 1)
	}); err != nil {
		t.Fatal(err)
	}
	if err := h.Handle(ctx, event(other)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(legacy); err != nil {
		t.Fatal("non-original system removed legacy index", err)
	}
	otherPath := filepath.Join(root, "S02", paths.IndexDirectory, Filename)
	otherBefore, err := os.ReadFile(otherPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.Handle(ctx, event(1)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(legacy); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("managed old-root index remains: %v", err)
	}
	for code, name := range map[string]string{"S01": "Audit firm", "S02": "Advisory firm"} {
		data, err := os.ReadFile(filepath.Join(root, code, paths.IndexDirectory, Filename))
		if err != nil {
			t.Fatal(err)
		}
		pf, err := presetfile.Parse(data, presetfile.FormatHuML)
		if err != nil {
			t.Fatal(err)
		}
		if pf.System != code || pf.Name != name || pf.ID != "archive" || pf.Version != 1 || pf.Seeds != nil {
			t.Fatalf("wrong system snapshot: %+v", pf)
		}
	}
	if _, err := d.ExecWrite(ctx, `UPDATE jd_categories SET description='Only audit changed' WHERE system_id=1 AND code=11`); err != nil {
		t.Fatal(err)
	}
	if err := h.Handle(ctx, event(1)); err != nil {
		t.Fatal(err)
	}
	otherAfter, err := os.ReadFile(otherPath)
	if err != nil || string(otherBefore) != string(otherAfter) {
		t.Fatal("refresh touched another system index", err)
	}
	if err := os.WriteFile(legacy, []byte("User filing notes"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := h.Handle(ctx, event(1)); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(legacy); err != nil || string(data) != "User filing notes" {
		t.Fatal("unknown legacy file removed", err)
	}
}

func TestNamedIndexRejectsEscapingSystemRoot(t *testing.T) {
	root, outside := t.TempDir(), t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "S01")); err != nil {
		t.Fatal(err)
	}
	if err := writeIndex(t.Context(), root, "S01", []byte(generatedHeader+"data")); err == nil {
		t.Fatal("symlinked system root accepted")
	}
	if _, err := os.Stat(filepath.Join(outside, paths.IndexDirectory)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("index escaped render root")
	}
	for _, code := range []string{"../S02", "/S02", "S01/../S02", "s01"} {
		if err := writeIndex(t.Context(), root, code, []byte(generatedHeader+"data")); err == nil {
			t.Fatalf("unsafe system code %q accepted", code)
		}
	}
}

func TestIndexRejectsMismatchedJobOwnership(t *testing.T) {
	d, log := indexDB(t)
	root := t.TempDir()
	h := New(d, log, root)
	for _, event := range []pluginapi.Event{
		{SystemID: 2, Payload: map[string]any{"raw": `{"system_id":1}`}},
		{SystemID: 1, Payload: map[string]any{"raw": `{"system_id":1.5}`}},
		{SystemID: 1, Payload: map[string]any{}},
	} {
		if err := h.Handle(t.Context(), event); err == nil {
			t.Fatal("invalid ownership published an index")
		}
	}
	if _, err := os.Stat(filepath.Join(root, paths.IndexDirectory, Filename)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("invalid event produced a projection")
	}
}

func TestPausedUnnamedSnapshotCannotOutliveNamedRefresh(t *testing.T) {
	d, log := indexDB(t)
	root := t.TempDir()
	h := New(d, log, root)
	reached, release := make(chan struct{}), make(chan struct{})
	var paused atomic.Bool
	h.beforePublish = func() {
		if paused.CompareAndSwap(false, true) {
			close(reached)
			<-release
		}
	}
	event := pluginapi.Event{SystemID: 1, Payload: map[string]any{"raw": `{"system_id":1}`}}
	old := make(chan error, 1)
	go func() { old <- h.Handle(t.Context(), event) }()
	<-reached
	if _, err := d.ExecWrite(t.Context(), `UPDATE jd_systems SET code='S01',name='Named firm' WHERE id=1; UPDATE jd_categories SET description='After introduction' WHERE system_id=1 AND code=11`); err != nil {
		close(release)
		t.Fatal(err)
	}
	named := make(chan error, 1)
	go func() { named <- h.Handle(t.Context(), event) }()
	select {
	case err := <-named:
		close(release)
		<-old
		t.Fatalf("refresh overtook paused old snapshot: %v", err)
	case <-time.After(30 * time.Millisecond):
	}
	close(release)
	if err := <-old; err != nil {
		t.Fatal(err)
	}
	if err := <-named; err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(root, "S01", paths.IndexDirectory, Filename))
	if err != nil {
		t.Fatal(err)
	}
	pf, err := presetfile.Parse(data, presetfile.FormatHuML)
	if err != nil {
		t.Fatal(err)
	}
	if pf.System != "S01" || pf.Name != "Named firm" || pf.Areas[0].Categories[0].Description != "After introduction" {
		t.Fatalf("old snapshot won publication race: %+v", pf)
	}
	if _, err := os.Lstat(filepath.Join(root, paths.IndexDirectory, Filename)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("old snapshot survived the named refresh: %v", err)
	}
}
