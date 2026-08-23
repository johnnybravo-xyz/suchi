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
	pluginapi "github.com/johnnybravo-xyz/suchi/plugin-api"
)

const replacementTaxonomy = `format = "suchi-taxonomy/v1"
id = "replacement"
version = 1
name = "Replacement"
license = "CC0-1.0"
inbox = 49

[[areas]]
code = 40
name = "System"

  [[areas.categories]]
  code = 49
  name = "Inbox"
`

func TestImportTaxonomyReplaceKeepsTrashedDocumentsRestorable(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	if err := jd.ApplyPreset(ctx, d, log, "solo", jd.ApplyPresetOpts{}); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Write.ExecContext(ctx, `
		INSERT INTO users(id, email, display_name, role, created_at, updated_at)
		VALUES (1, 'admin@example.test', 'Admin', 'admin', 0, 0);
		INSERT INTO documents(
			owner_id, title, original_blob, original_size,
			jd_category_id, created_at, added_at, updated_at, trashed_at
		) VALUES (
			1, 'trashed.pdf', 'sha-trash', 1,
			(SELECT id FROM jd_categories WHERE system = 1), 0, 0, 0, 1
		)
	`); err != nil {
		t.Fatal(err)
	}

	body, err := json.Marshal(TaxonomyImportReq{
		Content: replacementTaxonomy,
		Format:  "toml",
		Apply:   true,
	})
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{DB: d, Log: log}
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
		WHERE d.title = 'trashed.pdf' AND d.trashed_at IS NOT NULL AND c.system = 1
	`).Scan(&categoryExists); err != nil {
		t.Fatal(err)
	}
	if categoryExists != 1 {
		t.Fatal("trashed document was not repointed to the replacement inbox")
	}
}
