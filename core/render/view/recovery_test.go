// SPDX-License-Identifier: AGPL-3.0-or-later

package view

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/johnnybravo-xyz/suchi/core/blob"
	"github.com/johnnybravo-xyz/suchi/core/db"
	"github.com/johnnybravo-xyz/suchi/core/db/migrations"
	"github.com/johnnybravo-xyz/suchi/core/render/paths"
	"github.com/johnnybravo-xyz/suchi/core/trash"
)

func recoveryRenderer(t *testing.T) (*Renderer, *db.DB, string) {
	t.Helper()
	root := t.TempDir()
	d, err := db.Open(t.Context(), filepath.Join(root, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	migs, err := db.LoadMigrations(migrations.FS, ".")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Migrate(t.Context(), d, migs, log); err != nil {
		t.Fatal(err)
	}
	cas, err := blob.New(root)
	if err != nil {
		t.Fatal(err)
	}
	ref, err := cas.Put(strings.NewReader("immutable original"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = d.ExecWrite(t.Context(), `INSERT INTO users(id,email,display_name,role,created_at,updated_at) VALUES(1,'owner@test','Owner','admin',0,0);
 INSERT INTO jd_areas(system_id,code_start,code_end,name,position) VALUES(1,10,19,'Records',0);
 INSERT INTO jd_categories(system_id,id,area_start,code,name,system) VALUES(1,1,10,13,'Tax',0);
 INSERT INTO documents(system_id,id,owner_id,original_blob,original_size,title,jd_category_id,created_at,updated_at) VALUES(1,147,1,?,18,'Receipt',1,0,0);`, ref.SHA256)
	if err != nil {
		t.Fatal(err)
	}
	r, err := New(d, cas, filepath.Join(root, "rendered"), log)
	if err != nil {
		t.Fatal(err)
	}
	src, err := cas.Path(ref.SHA256)
	if err != nil {
		t.Fatal(err)
	}
	return r, d, src
}

func TestPublicationDoesNotRecreatePurgedDocument(t *testing.T) {
	r, d, _ := recoveryRenderer(t)
	reached, release := make(chan struct{}), make(chan struct{})
	r.beforePublish = func() {
		close(reached)
		<-release
	}
	finished := make(chan error, 1)
	go func() { _, err := r.Render(t.Context(), 147); finished <- err }()
	<-reached
	var path string
	if err := d.Read.QueryRowContext(t.Context(), `SELECT new_path FROM render_moves WHERE document_id=147`).Scan(&path); err != nil {
		close(release)
		t.Fatal(err)
	}
	s, err := trash.New(d, r.renderDir, r.log)
	if err == nil {
		_, err = d.ExecWrite(t.Context(), `UPDATE documents SET trashed_at = 1 WHERE id=147`)
	}
	if err == nil {
		_, err = s.PurgeExpired(t.Context(), time.Now())
	}
	close(release)
	if err != nil {
		t.Fatal(err)
	}
	<-finished
	if _, err := os.Lstat(filepath.Join(r.renderDir, path)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("rendered link recreated after purge: %v", err)
	}
}

func assertProjection(t *testing.T, r *Renderer, path string) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(r.renderDir, path))
	if err != nil || string(data) != "immutable original" {
		t.Fatalf("projection %q: %q %v", path, data, err)
	}
}

func TestInitialPublicationPausedAcrossIntroductionConverges(t *testing.T) {
	for _, operation := range []string{"move", "reconcile"} {
		t.Run(operation, func(t *testing.T) {
			r, d, _ := recoveryRenderer(t)
			reached, release := make(chan struct{}), make(chan struct{})
			var paused atomic.Bool
			r.beforePublish = func() {
				if paused.CompareAndSwap(false, true) {
					close(reached)
					<-release
				}
			}
			initial := make(chan error, 1)
			go func() { _, err := r.Render(t.Context(), 147); initial <- err }()
			<-reached
			var old string
			if err := d.Read.QueryRow(`SELECT new_path FROM render_moves WHERE document_id=147 AND state='pending'`).Scan(&old); err != nil {
				close(release)
				t.Fatal("initial publication not journaled", err)
			}
			if _, err := os.Lstat(filepath.Join(r.renderDir, old)); !errors.Is(err, os.ErrNotExist) {
				close(release)
				t.Fatalf("published before barrier: %v", err)
			}
			if _, err := d.ExecWrite(t.Context(), `UPDATE jd_systems SET code='S01',name='Firm' WHERE id=1`); err != nil {
				close(release)
				t.Fatal(err)
			}
			next := make(chan error, 1)
			go func() {
				if operation == "move" {
					next <- r.Move(t.Context(), 147)
				} else {
					next <- r.Reconcile(t.Context())
				}
			}()
			// The second public operation must not finish while the first publisher is
			// paused. Without serialization it can publish named state before the old
			// publisher resumes, orphaning the old-root link.
			select {
			case err := <-next:
				close(release)
				<-initial
				t.Fatalf("projection overtook paused publication: %v", err)
			case <-time.After(30 * time.Millisecond):
			}
			close(release)
			if err := <-initial; err != nil {
				t.Fatal(err)
			}
			if err := <-next; err != nil {
				t.Fatal(err)
			}
			// Reconcile only recovers pending work; the import also queues an ordinary Move.
			if err := r.Move(t.Context(), 147); err != nil {
				t.Fatal(err)
			}
			current, _, err := r.resolveTarget(t.Context(), 147)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.HasPrefix(current, "S01/") || !strings.HasSuffix(current, "__S01.13.147.pdf") {
				t.Fatal(current)
			}
			assertProjection(t, r, current)
			if _, err := os.Lstat(filepath.Join(r.renderDir, old)); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("orphan old-root link: %v", err)
			}
			if _, err := d.ExecWrite(t.Context(), `UPDATE jd_systems SET name='Renamed firm' WHERE id=1`); err != nil {
				t.Fatal(err)
			}
			if err := r.Move(t.Context(), 147); err != nil {
				t.Fatal(err)
			}
			renamed, _, err := r.resolveTarget(t.Context(), 147)
			if err != nil || renamed != current {
				t.Fatalf("display rename relocated document: %q %v", renamed, err)
			}
		})
	}
}

func TestInitialJournalFailurePublishesNothingAndRestartRecoversPending(t *testing.T) {
	r, d, _ := recoveryRenderer(t)
	target, _, err := r.resolveTarget(t.Context(), 147)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.ExecWrite(t.Context(), `CREATE TRIGGER reject_render BEFORE INSERT ON render_moves BEGIN SELECT RAISE(ABORT,'journal unavailable'); END;`); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Render(t.Context(), 147); err == nil {
		t.Fatal("journal failure was ignored")
	}
	if _, err := os.Lstat(filepath.Join(r.renderDir, target)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("unjournaled publication", err)
	}
	if _, err := d.ExecWrite(t.Context(), `DROP TRIGGER reject_render; INSERT INTO render_moves(document_id,prev_path,new_path,state,created_at) VALUES(147,'',?,'pending',0)`, target); err != nil {
		t.Fatal(err)
	}
	// Restart before the first link ever existed, after first system adoption.
	if _, err := d.ExecWrite(t.Context(), `UPDATE jd_systems SET code='S01' WHERE id=1`); err != nil {
		t.Fatal(err)
	}
	restarted, err := New(d, r.cas, r.renderDir, r.log)
	if err != nil {
		t.Fatal(err)
	}
	if err := restarted.Reconcile(t.Context()); err != nil {
		t.Fatal(err)
	}
	current, _, err := restarted.resolveTarget(t.Context(), 147)
	if err != nil {
		t.Fatal(err)
	}
	assertProjection(t, restarted, current)
	if _, err := os.Lstat(filepath.Join(r.renderDir, target)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("superseded destination revived", err)
	}
}

func TestRecoveryCleansReservedLegacyDestinationOnlyWithCASProof(t *testing.T) {
	for _, priorState := range []string{"pending", "applied"} {
		for _, occupied := range []string{"owned", "foreign-link", "user-file"} {
			t.Run(priorState+"/"+occupied, func(t *testing.T) {
				r, d, src := recoveryRenderer(t)
				old := filepath.Join("S01", paths.IndexDirectory, "00.00 archive.huml")
				full := filepath.Join(r.renderDir, old)
				if err := os.MkdirAll(filepath.Dir(full), 0750); err != nil {
					t.Fatal(err)
				}
				foreignRef, err := r.cas.Put(strings.NewReader("another document's CAS bytes"))
				if err != nil {
					t.Fatal(err)
				}
				foreignSrc, err := r.cas.Path(foreignRef.SHA256)
				if err != nil {
					t.Fatal(err)
				}
				switch occupied {
				case "owned":
					if err := os.Symlink(src, full); err != nil {
						t.Fatal(err)
					}
				case "foreign-link":
					if err := os.Symlink(foreignSrc, full); err != nil {
						t.Fatal(err)
					}
				case "user-file":
					if err := os.WriteFile(full, []byte("user notes"), 0600); err != nil {
						t.Fatal(err)
					}
				}
				if _, err := d.ExecWrite(t.Context(), `INSERT INTO render_moves(document_id,prev_path,new_path,state,created_at,applied_at) VALUES(147,'',?,?,0,0); UPDATE jd_systems SET code='S01' WHERE id=1`, old, priorState); err != nil {
					t.Fatal(err)
				}
				err = r.Move(t.Context(), 147)
				if occupied != "owned" {
					if err == nil {
						t.Fatal("unproved old artifact was accepted")
					}
					if occupied == "foreign-link" {
						got, e := os.Readlink(full)
						if e != nil || got != foreignSrc {
							t.Fatal("foreign symlink changed", got, e)
						}
					} else {
						got, e := os.ReadFile(full)
						if e != nil || string(got) != "user notes" {
							t.Fatal("user file changed", e)
						}
					}
					return
				}
				if err != nil {
					t.Fatal(err)
				}
				if _, err := os.Lstat(full); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("owned reserved old link remains", err)
				}
				current, _, err := r.resolveTarget(t.Context(), 147)
				if err != nil {
					t.Fatal(err)
				}
				assertProjection(t, r, current)
			})
		}
	}
}

func TestSystemRootCannotBeEscapedByTemplatesOrSymlinks(t *testing.T) {
	for _, tpl := range []string{"../S02/leak.pdf", "/tmp/leak.pdf", "safe/../../leak.pdf", paths.IndexDirectory + "/00.00 archive.huml"} {
		t.Run(tpl, func(t *testing.T) {
			r, d, _ := recoveryRenderer(t)
			if _, err := d.ExecWrite(t.Context(), `UPDATE jd_systems SET code='S01' WHERE id=1; INSERT INTO storage_paths(system_id,id,name,slug,path,created_at,updated_at) VALUES(1,1,'Custom','custom',?,0,0); UPDATE documents SET storage_path_id=1 WHERE id=147`, tpl); err != nil {
				t.Fatal(err)
			}
			if _, err := r.Render(t.Context(), 147); err == nil {
				t.Fatal("unsafe template accepted")
			}
		})
	}
	for _, parent := range []string{"system", "installation"} {
		t.Run(parent, func(t *testing.T) {
			r, d, _ := recoveryRenderer(t)
			if _, err := d.ExecWrite(t.Context(), `UPDATE jd_systems SET code='S01' WHERE id=1`); err != nil {
				t.Fatal(err)
			}
			outside := t.TempDir()
			link := filepath.Join(r.renderDir, "S01")
			if parent == "installation" {
				if err := os.Remove(r.renderDir); err != nil {
					t.Fatal(err)
				}
				link = r.renderDir
			}
			if err := os.Symlink(outside, link); err != nil {
				t.Fatal(err)
			}
			if _, err := r.Render(context.Background(), 147); err == nil {
				t.Fatal("symlinked root accepted")
			}
			entries, err := os.ReadDir(outside)
			if err != nil || len(entries) != 0 {
				t.Fatal("projection escaped root", entries, err)
			}
		})
	}
}

func TestLegacyExplicitTemplateRelocatesOutOfFutureIndex(t *testing.T) {
	r, d, _ := recoveryRenderer(t)
	template := "S01/" + paths.IndexDirectory + "/00.00 archive.huml"
	if _, err := d.ExecWrite(t.Context(), `INSERT INTO storage_paths(system_id,id,name,slug,path,created_at,updated_at) VALUES(1,1,'Legacy','legacy',?,0,0); UPDATE documents SET storage_path_id=1 WHERE id=147`, template); err != nil {
		t.Fatal(err)
	}
	old, err := r.Render(t.Context(), 147)
	if err != nil {
		t.Fatal(err)
	}
	assertProjection(t, r, old)
	if _, err := d.ExecWrite(t.Context(), `UPDATE jd_systems SET code='S01' WHERE id=1`); err != nil {
		t.Fatal(err)
	}
	if err := r.Move(t.Context(), 147); err != nil {
		t.Fatal(err)
	}
	assertProjection(t, r, filepath.Join("S01", template))
	if _, err := os.Lstat(filepath.Join(r.renderDir, old)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("legacy index destination was not released", err)
	}
}

func TestTwoSystemsResolveIdenticalFilingCodesUsingTheirOwnMode(t *testing.T) {
	r, d, _ := recoveryRenderer(t)
	_, err := d.ExecWrite(t.Context(), `UPDATE jd_systems SET code='S01',name='Audit' WHERE id=1;
 INSERT INTO jd_systems(id,code,name,taxonomy,created_at,updated_at) VALUES(2,'S02','Advisory','flat',0,0);
 INSERT INTO jd_areas(system_id,code_start,code_end,name,position) VALUES(2,10,19,'Other records',0);
 INSERT INTO jd_categories(system_id,id,area_start,code,name,system) VALUES(2,2,10,13,'Tax',0);
 INSERT INTO correspondents(system_id,id,name,slug,created_at,updated_at) VALUES(1,1,'Same client','same-client',0,0),(2,2,'Same client','same-client',0,0);
 UPDATE documents SET correspondent_id=1 WHERE id=147;
 INSERT INTO documents(system_id,id,owner_id,original_blob,original_size,title,jd_category_id,correspondent_id,created_at,updated_at) SELECT 2,148,owner_id,original_blob,original_size,title,2,2,created_at,updated_at FROM documents WHERE id=147;`)
	if err != nil {
		t.Fatal(err)
	}
	first, err := r.Render(t.Context(), 147)
	if err != nil {
		t.Fatal(err)
	}
	second, err := r.Render(t.Context(), 148)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(first, "S01/10-19 Records/13 Tax/") || !strings.HasSuffix(first, "__S01.13.147.pdf") {
		t.Fatal(first)
	}
	if !strings.HasPrefix(second, "S02/Same client/") || !strings.HasSuffix(second, "__S02.13.148.pdf") {
		t.Fatal(second)
	}
	assertProjection(t, r, first)
	assertProjection(t, r, second)
	if _, err := d.ExecWrite(t.Context(), `UPDATE jd_systems SET taxonomy='jd' WHERE id=2`); err != nil {
		t.Fatal(err)
	}
	if err := r.Move(t.Context(), 148); err != nil {
		t.Fatal(err)
	}
	current, _, err := r.resolveTarget(t.Context(), 148)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(current, "S02/10-19 Other records/13 Tax/") {
		t.Fatal("mode or area cached across systems", current)
	}
	assertProjection(t, r, current)
	assertProjection(t, r, first)
	if _, err := os.Lstat(filepath.Join(r.renderDir, second)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("old mode path remains", err)
	}
}

func TestJournalCompletionFailureRetainsRecoverablePublication(t *testing.T) {
	r, d, _ := recoveryRenderer(t)
	if _, err := d.ExecWrite(t.Context(), `CREATE TRIGGER reject_completion BEFORE UPDATE OF state ON render_moves WHEN NEW.state='applied' BEGIN SELECT RAISE(ABORT,'completion unavailable'); END;`); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Render(t.Context(), 147); err == nil {
		t.Fatal("failed journal completion reported success")
	}
	target, _, err := r.resolveTarget(t.Context(), 147)
	if err != nil {
		t.Fatal(err)
	}
	assertProjection(t, r, target)
	if _, err := d.ExecWrite(t.Context(), `DROP TRIGGER reject_completion; UPDATE jd_systems SET code='S01' WHERE id=1`); err != nil {
		t.Fatal(err)
	}
	if err := r.Reconcile(t.Context()); err != nil {
		t.Fatal(err)
	}
	current, _, err := r.resolveTarget(t.Context(), 147)
	if err != nil {
		t.Fatal(err)
	}
	assertProjection(t, r, current)
	if _, err := os.Lstat(filepath.Join(r.renderDir, target)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("failed-completion link orphaned", err)
	}
}

func TestNewDestinationNeverOverwritesForeignArtifact(t *testing.T) {
	for _, kind := range []string{"file", "symlink"} {
		t.Run(kind, func(t *testing.T) {
			r, _, _ := recoveryRenderer(t)
			target, _, err := r.resolveTarget(t.Context(), 147)
			if err != nil {
				t.Fatal(err)
			}
			full := filepath.Join(r.renderDir, target)
			if err := os.MkdirAll(filepath.Dir(full), 0750); err != nil {
				t.Fatal(err)
			}
			if kind == "file" {
				if err := os.WriteFile(full, []byte("user notes"), 0600); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := os.Symlink("foreign-target", full); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := r.Render(t.Context(), 147); err == nil {
				t.Fatal("occupied destination overwritten")
			}
			if kind == "file" {
				data, err := os.ReadFile(full)
				if err != nil || string(data) != "user notes" {
					t.Fatal("user file changed", err)
				}
			} else {
				data, err := os.Readlink(full)
				if err != nil || data != "foreign-target" {
					t.Fatal("foreign link changed", err)
				}
			}
		})
	}
}

func TestSystemTemplateValuesRemainInsideEnforcedRoot(t *testing.T) {
	r, d, _ := recoveryRenderer(t)
	if _, err := d.ExecWrite(t.Context(), `UPDATE jd_systems SET code='S01',name='Audit firm' WHERE id=1;
 INSERT INTO storage_paths(system_id,id,name,slug,path,created_at,updated_at) VALUES(1,1,'Custom','custom','{{ jd.system.code }}/{{ jd.system.name }}/{{ jd.address }}.pdf',0,0);
 UPDATE documents SET storage_path_id=1 WHERE id=147;`); err != nil {
		t.Fatal(err)
	}
	actual, err := r.Render(t.Context(), 147)
	if err != nil {
		t.Fatal(err)
	}
	if actual != filepath.Join("S01", "S01", "Audit firm", "S01.13.147.pdf") {
		t.Fatalf("template replaced enforced system root: %q", actual)
	}
	assertProjection(t, r, actual)
}
