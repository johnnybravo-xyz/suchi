// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"archive/zip"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/johnnybravo-xyz/suchi/core/blob"
	"github.com/johnnybravo-xyz/suchi/core/db"
	"github.com/johnnybravo-xyz/suchi/core/db/migrations"
)

func TestNativeTakeoutAllOwnersStaysInSelectedSystem(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "data")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DATA_DIR", root)
	t.Setenv("SUCHI_CONFIG", "")
	t.Setenv("PUBLIC_URL", "http://127.0.0.1:8000")
	d, err := db.Open(t.Context(), filepath.Join(root, "suchi.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	migs, err := db.LoadMigrations(migrations.FS, ".")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Migrate(t.Context(), d, migs, slog.New(slog.NewTextHandler(io.Discard, nil))); err != nil {
		t.Fatal(err)
	}
	cas, err := blob.New(root)
	if err != nil {
		t.Fatal(err)
	}
	ref, err := cas.Put(strings.NewReader("shared immutable bytes"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = d.ExecWrite(t.Context(), `INSERT INTO users(id,email,display_name,role,created_at,updated_at) VALUES(1,'admin@test','Admin','admin',0,0),(2,'member@test','Member','member',0,0);
 UPDATE jd_systems SET code='S01',name='Audit firm' WHERE id=1;
 INSERT INTO jd_systems(id,code,name,taxonomy,created_at,updated_at) VALUES(2,'S02','Advisory firm','jd',0,0);
 INSERT INTO jd_areas(system_id,code_start,code_end,name,position) VALUES(1,10,19,'Records',0),(2,10,19,'Records',0);
 INSERT INTO jd_categories(system_id,id,area_start,code,name,system) VALUES(1,1,10,13,'Tax',0),(2,2,10,13,'Tax',0);
 INSERT INTO tags(system_id,id,name,slug,created_at,updated_at) VALUES(1,1,'Private audit label','same-name',0,0),(2,2,'Advisory label','same-name',0,0);
 INSERT INTO documents(system_id,id,owner_id,original_blob,original_size,title,mime_type,jd_category_id,created_at,updated_at) VALUES(1,147,1,?,22,'Excluded audit file','text/plain',1,0,0),(2,148,1,?,22,'First owner','text/plain',2,0,0),(2,149,2,?,22,'Second owner','text/plain',2,0,0);`, ref.SHA256, ref.SHA256, ref.SHA256)
	if err != nil {
		t.Fatal(err)
	}
	ambiguous := filepath.Join(base, "ambiguous.zip")
	if code := runExport([]string{"--system", "S02", "--out", ambiguous}); code == 0 {
		t.Fatal("ambiguous owner scope exported without an explicit choice")
	}
	if _, err := os.Stat(ambiguous); !os.IsNotExist(err) {
		t.Fatal("ambiguous owner scope produced a takeout", err)
	}
	out := filepath.Join(base, "takeout.zip")
	if code := runExport([]string{"--system", "S02", "--all", "--out", out}); code != 0 {
		t.Fatalf("export exit=%d", code)
	}
	archive, err := zip.OpenReader(out)
	if err != nil {
		t.Fatal(err)
	}
	defer archive.Close()
	sidecars := map[string]exportSidecar{}
	var manifest exportManifest
	for _, entry := range archive.File {
		rc, err := entry.Open()
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(data), "Excluded audit file") || strings.Contains(string(data), "Private audit label") {
			t.Fatalf("foreign system data in %s: %s", entry.Name, data)
		}
		switch {
		case entry.Name == "manifest.json":
			if err := json.Unmarshal(data, &manifest); err != nil {
				t.Fatal(err)
			}
		case strings.HasPrefix(entry.Name, "documents/") && strings.HasSuffix(entry.Name, ".json"):
			var side exportSidecar
			if err := json.Unmarshal(data, &side); err != nil {
				t.Fatal(err)
			}
			sidecars[side.JDAddress] = side
		case entry.Name == "taxonomy/tags.json":
			var tags []map[string]any
			if err := json.Unmarshal(data, &tags); err != nil {
				t.Fatal(err)
			}
			if len(tags) != 1 || tags[0]["name"] != "Advisory label" {
				t.Fatalf("unscoped tags: %s", data)
			}
		}
	}
	if manifest.Version != "2" || manifest.SystemCode != "S02" || manifest.SystemName != "Advisory firm" || manifest.Documents != 2 {
		t.Fatalf("manifest: %+v", manifest)
	}
	if len(sidecars) != 2 {
		t.Fatalf("all owners did not export both target documents: %+v", sidecars)
	}
	for address, title := range map[string]string{"S02.13.148": "First owner", "S02.13.149": "Second owner"} {
		side, ok := sidecars[address]
		if !ok || side.Title != title || side.Version != 1 || side.JDSystem != "S02" || side.JDCategory != 13 || side.SHA256 != ref.SHA256 {
			t.Fatalf("wrong source provenance: %+v", side)
		}
	}
	invalid := filepath.Join(root, "invalid.zip")
	if code := runExport([]string{"--system", "S99", "--all", "--out", invalid}); code == 0 {
		t.Fatal("unknown target silently defaulted")
	}
	if _, err := os.Stat(invalid); !os.IsNotExist(err) {
		t.Fatal("unknown target produced a takeout", err)
	}
}

func TestExportProtectsDestinationAndIncompleteArchives(t *testing.T) {
	base := t.TempDir()
	dataDir := filepath.Join(base, "data")
	if err := os.Mkdir(dataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DATA_DIR", dataDir)
	t.Setenv("SUCHI_CONFIG", "")
	t.Setenv("PUBLIC_URL", "http://127.0.0.1:8000")
	d, err := db.Open(t.Context(), filepath.Join(dataDir, "suchi.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	migs, err := db.LoadMigrations(migrations.FS, ".")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Migrate(t.Context(), d, migs, slog.New(slog.NewTextHandler(io.Discard, nil))); err != nil {
		t.Fatal(err)
	}
	_, err = d.ExecWrite(t.Context(), `
		INSERT INTO users(id,email,display_name,role,created_at,updated_at)
		VALUES(1,'admin@test','Admin','admin',0,0);
		INSERT INTO jd_areas(system_id,code_start,code_end,name,position)
		VALUES(1,10,19,'Records',0);
		INSERT INTO jd_categories(system_id,id,area_start,code,name,system)
		VALUES(1,1,10,13,'Tax',0);
		INSERT INTO documents(system_id,id,owner_id,original_blob,original_size,title,mime_type,jd_category_id,created_at,updated_at)
		VALUES(1,147,1,?,1,'Missing original','text/plain',1,0,0);`,
		strings.Repeat("0", 64))
	if err != nil {
		t.Fatal(err)
	}

	out := filepath.Join(base, "takeout.zip")
	if err := os.WriteFile(out, []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	if code := runExport([]string{"--all", "--out", out}); code == 0 {
		t.Fatal("existing output was overwritten without --force")
	}
	if code := runExport([]string{"--all", "--out", out, "--force"}); code == 0 {
		t.Fatal("incomplete export succeeded without --allow-incomplete")
	}
	if got, err := os.ReadFile(out); err != nil || string(got) != "keep" {
		t.Fatalf("failed export changed destination: %q, %v", got, err)
	}
	partials, err := filepath.Glob(filepath.Join(base, ".takeout.zip.*.tmp"))
	if err != nil || len(partials) != 0 {
		t.Fatalf("partial outputs remain: %v, %v", partials, err)
	}

	unsafe := []string{
		dataDir,
		filepath.Join(dataDir, "suchi.db"),
		filepath.Join(dataDir, "takeout.zip"),
	}
	dbAlias := filepath.Join(base, "database-alias")
	if err := os.Link(filepath.Join(dataDir, "suchi.db"), dbAlias); err == nil {
		unsafe = append(unsafe, dbAlias)
	}
	symlinkAlias := filepath.Join(base, "database-symlink")
	if err := os.Symlink(filepath.Join(dataDir, "suchi.db"), symlinkAlias); err == nil {
		unsafe = append(unsafe, symlinkAlias)
	}
	dataAlias := filepath.Join(base, "data-symlink")
	if err := os.Symlink(dataDir, dataAlias); err == nil {
		unsafe = append(unsafe, filepath.Join(dataAlias, "takeout.zip"))
	}
	for _, target := range unsafe {
		if code := runExport([]string{"--all", "--out", target, "--force", "--allow-incomplete"}); code == 0 {
			t.Errorf("unsafe output target accepted: %s", target)
		}
	}

	if code := runExport([]string{"--all", "--out", out, "--force", "--allow-incomplete"}); code != 0 {
		t.Fatalf("explicit incomplete export exit=%d", code)
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(out)
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != 0o600 {
			t.Fatalf("export mode = %04o, want 0600", got)
		}
	}
	archive, err := zip.OpenReader(out)
	if err != nil {
		t.Fatal(err)
	}
	defer archive.Close()
	var manifest exportManifest
	for _, entry := range archive.File {
		if entry.Name != "manifest.json" {
			continue
		}
		rc, err := entry.Open()
		if err != nil {
			t.Fatal(err)
		}
		if err := json.NewDecoder(rc).Decode(&manifest); err != nil {
			rc.Close()
			t.Fatal(err)
		}
		rc.Close()
	}
	if manifest.Documents != 0 || manifest.Skipped != 1 {
		t.Fatalf("incomplete manifest = %+v", manifest)
	}
}

func TestTaxonomyImportExplicitTargetMustAgreeWithFile(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "s02.toml")
	content := `format = "suchi-taxonomy/v1"
id = "s02"
version = 1
system = "S02"
name = "Advisory"
market = "global"
language = "en"
story = "File advisory records."
areas = []
`
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	dataDir := filepath.Join(root, "untouched")
	t.Setenv("DATA_DIR", dataDir)
	for _, code := range []string{"S01", "s02", "S02 ", "AUD"} {
		if status := runTaxonomyImport([]string{path, "--system", code, "--apply"}); status != 2 {
			t.Fatalf("mismatched target %q exit=%d", code, status)
		}
	}
	if _, err := os.Stat(dataDir); !os.IsNotExist(err) {
		t.Fatal("invalid target touched database", err)
	}
}
