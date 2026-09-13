package api

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/johnnybravo-xyz/suchi/core/auth"
	"github.com/johnnybravo-xyz/suchi/core/jd"
	"github.com/johnnybravo-xyz/suchi/core/jd/importer"
	pluginapi "github.com/johnnybravo-xyz/suchi/plugin-api"
)

const replacementTaxonomy = `format = "suchi-taxonomy/v1"
id = "replacement"
version = 1
name = "Replacement"
market = "global"
language = "en"
story = "A blank authoring tree leaves new documents in the generated Inbox."
areas = []
`

func TestImportTaxonomyKeepsTrashedFilingRestorable(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	if err := jd.ApplyPreset(ctx, d, log, "solo", jd.ApplyPresetOpts{SystemID: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Write.ExecContext(ctx, `
		INSERT INTO users(id, email, display_name, role, created_at, updated_at)
		VALUES (1, 'admin@example.test', 'Admin', 'admin', 0, 0);
		INSERT INTO documents(
			system_id, owner_id, title, original_blob, original_size,
			jd_category_id, created_at, added_at, updated_at, trashed_at
		) VALUES (
			1, 1, 'trashed.pdf', 'sha-trash', 1,
			(SELECT id FROM jd_categories WHERE code = 11), 0, 0, 0, 1
		)
	`); err != nil {
		t.Fatal(err)
	}

	input := TaxonomyImportReq{Content: replacementTaxonomy, Format: "toml"}
	s := &Server{DB: d, Log: log}
	preview := taxonomyRequest(t, s, input)
	if preview.Code != http.StatusOK {
		t.Fatalf("preview status=%d body=%s", preview.Code, preview.Body.String())
	}
	var diff importer.Diff
	if err := json.Unmarshal(preview.Body.Bytes(), &diff); err != nil {
		t.Fatal(err)
	}
	if diff.Mode != "merge" {
		t.Fatalf("filed Trash allowed replacement: %+v", diff)
	}
	input.Apply, input.ExpectedStateHash = true, diff.StateHash
	body, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/admin/taxonomy/import", strings.NewReader(string(body)))
	req = req.WithContext(auth.WithPrincipal(req.Context(), &pluginapi.Principal{
		Kind: "user", UserID: 1, Role: "admin",
	}))
	rec := httptest.NewRecorder()
	s.ImportTaxonomy(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}

	var categoryExists int
	if err := d.Read.QueryRowContext(ctx, `
		SELECT COUNT(*)
		FROM documents d
		JOIN jd_categories c ON c.id = d.jd_category_id
		WHERE d.title = 'trashed.pdf' AND d.trashed_at IS NOT NULL AND c.code = 11
	`).Scan(&categoryExists); err != nil {
		t.Fatal(err)
	}
	if categoryExists != 1 {
		t.Fatal("import lost the trashed document's filing")
	}
}

func taxonomyRequest(t *testing.T, s *Server, input TaxonomyImportReq) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/admin/taxonomy/import", strings.NewReader(string(body)))
	req = req.WithContext(auth.WithPrincipal(req.Context(), &pluginapi.Principal{Kind: "user", UserID: 1, Role: "admin"}))
	rec := httptest.NewRecorder()
	s.ImportTaxonomy(rec, req)
	return rec
}

func TestTaxonomyApplyRequiresCurrentPreview(t *testing.T) {
	d := openTestDB(t)
	seedUser(t, d, 1)
	s := &Server{DB: d, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	input := TaxonomyImportReq{Content: replacementTaxonomy, Format: "toml", Apply: true}
	if response := taxonomyRequest(t, s, input); response.Code != http.StatusConflict {
		t.Fatalf("unpreviewed apply status=%d body=%s", response.Code, response.Body.String())
	}
	input.Apply = false
	response := taxonomyRequest(t, s, input)
	var diff importer.Diff
	if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &diff) != nil {
		t.Fatalf("preview: %s", response.Body.String())
	}
	input.Apply, input.ExpectedStateHash, input.SkipSeeds = true, diff.StateHash, true
	if response := taxonomyRequest(t, s, input); response.Code != 409 {
		t.Fatalf("changed options status=%d body=%s", response.Code, response.Body.String())
	}
	var rows int
	if err := d.Read.QueryRow(`SELECT COUNT(*) FROM jd_areas`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 0 {
		t.Fatal("stale apply changed the database")
	}
	input = TaxonomyImportReq{Content: replacementTaxonomy}
	if response := taxonomyRequest(t, s, input); response.Code != 400 {
		t.Fatalf("omitted serialization guessed TOML: %s", response.Body.String())
	}
}
