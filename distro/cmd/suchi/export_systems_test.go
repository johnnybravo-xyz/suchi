// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"archive/zip"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/johnnybravo-xyz/suchi/core/blob"
	"github.com/johnnybravo-xyz/suchi/core/db"
	"github.com/johnnybravo-xyz/suchi/core/db/migrations"
)

func TestNativeTakeoutAllOwnersStaysInSelectedSystem(t *testing.T) {
	root := t.TempDir()
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
	out := filepath.Join(root, "takeout.zip")
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
