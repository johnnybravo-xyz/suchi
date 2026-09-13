package view

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestArchiveReplacementAndRenameRetainPublishedOwnership(t *testing.T) {
	r, d, _ := recoveryRenderer(t)
	first, err := r.cas.Put(strings.NewReader("first archive"))
	if err != nil {
		t.Fatal(err)
	}
	second, err := r.cas.Put(strings.NewReader("second archive"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.ExecWrite(t.Context(), `UPDATE documents SET archive_blob=? WHERE id=147`, first.SHA256); err != nil {
		t.Fatal(err)
	}
	previous, err := r.Render(t.Context(), 147)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.ExecWrite(t.Context(), `UPDATE documents SET archive_blob=?,title='Renamed receipt' WHERE id=147`, second.SHA256); err != nil {
		t.Fatal(err)
	}
	if err := r.Move(t.Context(), 147); err != nil {
		t.Fatal(err)
	}
	current, _, err := r.resolveTarget(t.Context(), 147)
	if err != nil {
		t.Fatal(err)
	}
	if current == previous {
		t.Fatal("title edit did not change the projected path")
	}
	assertRenderedBytes(t, r, current, "second archive")
	if _, err := os.Lstat(filepath.Join(r.renderDir, previous)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("previous archive projection remains: %v", err)
	}
	var previousBlob, nextBlob string
	if err := d.Read.QueryRow(`SELECT prev_blob,new_blob FROM render_moves WHERE document_id=147 ORDER BY id DESC LIMIT 1`).Scan(&previousBlob, &nextBlob); err != nil {
		t.Fatal(err)
	}
	if previousBlob != first.SHA256 || nextBlob != second.SHA256 {
		t.Fatalf("journal lost publication identity: previous=%q next=%q", previousBlob, nextBlob)
	}
	if err := r.Reconcile(t.Context()); err != nil {
		t.Fatal(err)
	}
	for _, hash := range []string{first.SHA256, second.SHA256} {
		if _, err := r.cas.Stat(hash); err != nil {
			t.Fatalf("rendering changed an immutable blob: %v", err)
		}
	}
}

func TestSamePathProjectionRefusesForeignArtifacts(t *testing.T) {
	for _, artifact := range []string{"file", "symlink", "same-hash-basename", "foreign-cas"} {
		t.Run(artifact, func(t *testing.T) {
			r, _, original := recoveryRenderer(t)
			relative, err := r.Render(t.Context(), 147)
			if err != nil {
				t.Fatal(err)
			}
			full := filepath.Join(r.renderDir, relative)
			if err := os.Remove(full); err != nil {
				t.Fatal(err)
			}
			foreign := filepath.Join(t.TempDir(), "unrelated-file")
			switch artifact {
			case "file":
				if err := os.WriteFile(full, []byte("user notes"), 0600); err != nil {
					t.Fatal(err)
				}
			case "same-hash-basename":
				foreign = filepath.Join(t.TempDir(), filepath.Base(original))
			case "foreign-cas":
				ref, err := r.cas.Put(strings.NewReader("another document"))
				if err != nil {
					t.Fatal(err)
				}
				foreign, err = r.cas.Path(ref.SHA256)
				if err != nil {
					t.Fatal(err)
				}
			}
			if artifact != "file" {
				if err := os.Symlink(foreign, full); err != nil {
					t.Fatal(err)
				}
			}
			if err := r.Move(t.Context(), 147); err == nil {
				t.Fatal("same-path refresh replaced an unrelated artifact")
			}
			if _, err := r.Render(t.Context(), 147); err == nil {
				t.Fatal("same-path render replaced an unrelated artifact")
			}
			if artifact == "file" {
				assertRenderedBytes(t, r, relative, "user notes")
			} else if got, err := os.Readlink(full); err != nil || got != foreign {
				t.Fatalf("foreign symlink changed: %q %v", got, err)
			}
		})
	}
}

func TestArchiveRefreshIsJournaledBeforeSamePathPublication(t *testing.T) {
	r, d, _ := recoveryRenderer(t)
	previous, err := r.Render(t.Context(), 147)
	if err != nil {
		t.Fatal(err)
	}
	ref, err := r.cas.Put(strings.NewReader("replacement archive"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.ExecWrite(t.Context(), `UPDATE documents SET archive_blob=? WHERE id=147;
		CREATE TRIGGER reject_render BEFORE INSERT ON render_moves BEGIN SELECT RAISE(ABORT,'journal unavailable'); END`, ref.SHA256); err != nil {
		t.Fatal(err)
	}
	if err := r.Move(t.Context(), 147); err == nil {
		t.Fatal("archive refresh ignored its journal failure")
	}
	assertProjection(t, r, previous)
	if _, err := d.ExecWrite(t.Context(), `DROP TRIGGER reject_render`); err != nil {
		t.Fatal(err)
	}
	if err := r.Move(t.Context(), 147); err != nil {
		t.Fatal(err)
	}
	assertRenderedBytes(t, r, previous, "replacement archive")
}

func TestRestartRetainsSupersededArchiveTargets(t *testing.T) {
	for _, rename := range []bool{false, true} {
		name := "same-path"
		if rename {
			name = "renamed-path"
		}
		t.Run(name, func(t *testing.T) {
			r, d, _ := recoveryRenderer(t)
			originalPath, err := r.Render(t.Context(), 147)
			if err != nil {
				t.Fatal(err)
			}
			first, err := r.cas.Put(strings.NewReader("first archive"))
			if err != nil {
				t.Fatal(err)
			}
			second, err := r.cas.Put(strings.NewReader("second archive"))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := d.ExecWrite(t.Context(), `UPDATE documents SET archive_blob=?,title='First archive' WHERE id=147;
				CREATE TRIGGER reject_completion BEFORE UPDATE OF state ON render_moves WHEN NEW.state='applied'
				BEGIN SELECT RAISE(ABORT,'completion unavailable'); END`, first.SHA256); err != nil {
				t.Fatal(err)
			}
			if err := r.Move(t.Context(), 147); err == nil {
				t.Fatal("journal completion failure was ignored")
			}
			firstPath, _, err := r.resolveTarget(t.Context(), 147)
			if err != nil {
				t.Fatal(err)
			}
			assertRenderedBytes(t, r, firstPath, "first archive")
			title := "First archive"
			if rename {
				title = "Second archive"
			}
			if _, err := d.ExecWrite(t.Context(), `UPDATE documents SET archive_blob=?,title=? WHERE id=147`, second.SHA256, title); err != nil {
				t.Fatal(err)
			}
			// A second interrupted publication must retain both archive identities,
			// including when both were published at the same destination.
			if err := r.Reconcile(t.Context()); err == nil {
				t.Fatal("second journal completion failure was ignored")
			}
			current, _, err := r.resolveTarget(t.Context(), 147)
			if err != nil {
				t.Fatal(err)
			}
			assertRenderedBytes(t, r, current, "second archive")
			if _, err := d.ExecWrite(t.Context(), `DROP TRIGGER reject_completion`); err != nil {
				t.Fatal(err)
			}
			restarted, err := New(d, r.cas, r.renderDir, r.log)
			if err != nil {
				t.Fatal(err)
			}
			if err := restarted.Reconcile(t.Context()); err != nil {
				t.Fatal(err)
			}
			assertRenderedBytes(t, restarted, current, "second archive")
			for _, old := range []string{originalPath, firstPath} {
				if old == current {
					continue
				}
				if _, err := os.Lstat(filepath.Join(r.renderDir, old)); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("superseded path %q remains: %v", old, err)
				}
			}
			var pending int
			if err := d.Read.QueryRow(`SELECT count(*) FROM render_moves WHERE state='pending'`).Scan(&pending); err != nil || pending != 0 {
				t.Fatalf("recovery left pending work: %d %v", pending, err)
			}
		})
	}
}

func TestLegacyUnknownArchiveOwnershipIsNotGuessed(t *testing.T) {
	r, d, _ := recoveryRenderer(t)
	first, err := r.cas.Put(strings.NewReader("unrecorded historical archive"))
	if err != nil {
		t.Fatal(err)
	}
	second, err := r.cas.Put(strings.NewReader("current archive"))
	if err != nil {
		t.Fatal(err)
	}
	previous, err := r.Render(t.Context(), 147)
	if err != nil {
		t.Fatal(err)
	}
	full := filepath.Join(r.renderDir, previous)
	if err := os.Remove(full); err != nil {
		t.Fatal(err)
	}
	oldSource, err := r.cas.Path(first.SHA256)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(oldSource, full); err != nil {
		t.Fatal(err)
	}
	if _, err := d.ExecWrite(t.Context(), `UPDATE render_moves SET prev_blob='',new_blob='';
		UPDATE documents SET archive_blob=?,title='Renamed archive' WHERE id=147`, second.SHA256); err != nil {
		t.Fatal(err)
	}
	if err := r.Move(t.Context(), 147); err == nil || !strings.Contains(err.Error(), "refusing foreign symlink") {
		t.Fatalf("unrecorded legacy archive was attributed without evidence: %v", err)
	}
	if got, err := os.Readlink(full); err != nil || got != oldSource {
		t.Fatalf("unproved legacy link changed: %q %v", got, err)
	}
}

func assertRenderedBytes(t *testing.T, r *Renderer, relative, want string) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(r.renderDir, relative))
	if err != nil || string(data) != want {
		t.Fatalf("projection %q: %q %v, want %q", relative, data, err, want)
	}
}

func TestArchiveChangedDuringPublicationRetainsThePublishedSnapshot(t *testing.T) {
	r, d, _ := recoveryRenderer(t)
	first, err := r.cas.Put(strings.NewReader("first snapshot"))
	if err != nil {
		t.Fatal(err)
	}
	second, err := r.cas.Put(strings.NewReader("later snapshot"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.ExecWrite(t.Context(), `UPDATE documents SET archive_blob=? WHERE id=147`, first.SHA256); err != nil {
		t.Fatal(err)
	}
	r.beforePublish = func() {
		if _, err := d.ExecWrite(t.Context(), `UPDATE documents SET archive_blob=?,title='Later title' WHERE id=147`, second.SHA256); err != nil {
			t.Fatal(err)
		}
	}
	published, err := r.Render(t.Context(), 147)
	if err != nil {
		t.Fatal(err)
	}
	assertRenderedBytes(t, r, published, "first snapshot")
	r.beforePublish = nil
	if err := r.Move(t.Context(), 147); err != nil {
		t.Fatal(err)
	}
	current, _, err := r.resolveTarget(t.Context(), 147)
	if err != nil {
		t.Fatal(err)
	}
	assertRenderedBytes(t, r, current, "later snapshot")
	if _, err := os.Lstat(filepath.Join(r.renderDir, published)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("publication snapshot left a stale link: %v", err)
	}
}
