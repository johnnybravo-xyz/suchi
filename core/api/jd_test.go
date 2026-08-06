package api

// Roundtrip tests for the JD taxonomy JSON surface — the endpoint
// mobile pickers + MCP tools + agents lean on to resolve
// jd_category_id to a human-readable filing chip.

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http/httptest"
	"os"
	"testing"

	pluginapi "github.com/johnnybravo-xyz/suchi/plugin-api"

	"github.com/johnnybravo-xyz/suchi/core/auth"
	"github.com/johnnybravo-xyz/suchi/core/db"
)

// seedJDTree lands a tiny two-area / four-category tree so the
// filter tests have something to prune. Every JD area shipped by
// jd.EnsureTree uses this same shape.
func seedJDTree(t *testing.T, d *db.DB) {
	t.Helper()
	ctx := context.Background()
	if _, err := d.Write.ExecContext(ctx, `
		INSERT INTO jd_areas(code_start, code_end, name, description, position) VALUES
		  (10, 19, 'Life',  'life admin',  0),
		  (20, 29, 'Money', 'money stuff', 1);
	`); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Write.ExecContext(ctx, `
		INSERT INTO jd_categories(id, area_start, code, name, description, system) VALUES
		  (1, 10, 11, 'Identity',   NULL,             0),
		  (2, 10, 12, 'Health',     NULL,             0),
		  (3, 20, 21, 'Banking',    NULL,             0),
		  (4, 20, 22, 'Tax',        'annual filings', 0);
	`); err != nil {
		t.Fatal(err)
	}
}

func authedGET(t *testing.T, path string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	ctx := auth.WithPrincipal(context.Background(),
		&pluginapi.Principal{Kind: "user", UserID: 1, Email: "a@example.com", Role: "member"})
	r := httptest.NewRequest("GET", path, nil).WithContext(ctx)

	d := getServerDB(t)
	s := &Server{DB: d, Log: slog.New(slog.NewTextHandler(os.Stderr, nil))}
	s.ListJDCategories(rec, r)
	return rec
}

// getServerDB is a per-test constructor. We can't reuse openTestDB's
// return value across sub-tests here because each subtest needs a
// fresh tree; convenient wrapper.
func getServerDB(t *testing.T) *db.DB {
	t.Helper()
	d := openTestDB(t)
	seedJDTree(t, d)
	return d
}

// jdResponse mirrors the envelope for typed access in tests.
type jdResponse struct {
	Count   int          `json:"count"`
	Results []JDCategory `json:"results"`
}

func TestListJDCategories_Unfiltered(t *testing.T) {
	rec := authedGET(t, "/api/jd/categories/")
	if rec.Code != 200 {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var body jdResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Count != 4 {
		t.Errorf("count=%d, want 4", body.Count)
	}
	if len(body.Results) != 4 {
		t.Errorf("results=%d, want 4", len(body.Results))
	}
	// Every row must carry the denormalized area info — the whole
	// reason this endpoint exists.
	for _, c := range body.Results {
		if c.AreaCode == 0 || c.AreaName == "" {
			t.Errorf("category %d missing area denormalization: %+v", c.Code, c)
		}
	}
	// Order is by code ascending.
	for i := 1; i < len(body.Results); i++ {
		if body.Results[i].Code < body.Results[i-1].Code {
			t.Errorf("results not ordered by code: %d then %d",
				body.Results[i-1].Code, body.Results[i].Code)
		}
	}
}

func TestListJDCategories_QueryByName(t *testing.T) {
	rec := authedGET(t, "/api/jd/categories/?q=tax")
	if rec.Code != 200 {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var body jdResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Count != 1 || body.Results[0].Name != "Tax" {
		t.Errorf("q=tax: got %+v, want single Tax row", body.Results)
	}
}

func TestListJDCategories_QueryByCode(t *testing.T) {
	rec := authedGET(t, "/api/jd/categories/?q=22")
	if rec.Code != 200 {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var body jdResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Count != 1 || body.Results[0].Code != 22 {
		t.Errorf("q=22: got %+v, want single code=22 row", body.Results)
	}
}

func TestListJDCategories_AreaScope(t *testing.T) {
	rec := authedGET(t, "/api/jd/categories/?area=20")
	if rec.Code != 200 {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var body jdResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Count != 2 {
		t.Errorf("area=20: count=%d, want 2", body.Count)
	}
	for _, c := range body.Results {
		if c.AreaCode != 20 {
			t.Errorf("area=20 filter leaked category %d (area %d)", c.Code, c.AreaCode)
		}
	}
}

func TestListJDCategories_Unauthenticated(t *testing.T) {
	d := openTestDB(t)
	seedJDTree(t, d)
	s := &Server{DB: d, Log: slog.New(slog.NewTextHandler(os.Stderr, nil))}
	rec := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/api/jd/categories/", nil) // no principal on ctx
	s.ListJDCategories(rec, r)
	if rec.Code != 401 {
		t.Fatalf("anonymous should get 401, got %d body=%s", rec.Code, rec.Body.String())
	}
}
