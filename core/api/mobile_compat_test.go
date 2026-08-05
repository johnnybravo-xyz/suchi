package api

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	pluginapi "github.com/suchi-dms/suchi/plugin-api"

	"github.com/suchi-dms/suchi/core/auth"
	"github.com/suchi-dms/suchi/core/db"
)

// seedJDCategory seeds one JD area + category so documents.jd_category_id
// (NOT NULL) can point at something. Idempotent per DB.
func seedJDCategory(t *testing.T, d *db.DB) {
	t.Helper()
	ctx := context.Background()
	if _, err := d.Write.ExecContext(ctx, `
		INSERT OR IGNORE INTO jd_areas(code_start, code_end, name, position)
		VALUES (10, 19, 'test-area', 0);
	`); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Write.ExecContext(ctx, `
		INSERT OR IGNORE INTO jd_categories(id, area_start, code, name, description, system)
		VALUES (1, 10, 11, 'test-cat', NULL, 0);
	`); err != nil {
		t.Fatal(err)
	}
}

// authedRequest returns an *http.Request with an admin principal on
// context — matches what the auth middleware would set upstream.
func authedRequest(method, path string) (*httptest.ResponseRecorder, context.Context) {
	rec := httptest.NewRecorder()
	ctx := auth.WithPrincipal(context.Background(),
		&pluginapi.Principal{Kind: "user", UserID: 1, Email: "a@example.com", Role: "admin"})
	return rec, ctx
}

func TestRemoteVersion(t *testing.T) {
	d := openTestDB(t)
	s := &Server{DB: d, Log: slog.New(slog.NewTextHandler(os.Stderr, nil))}

	rec, ctx := authedRequest("GET", "/api/remote_version/")
	r := httptest.NewRequest("GET", "/api/remote_version/", nil).WithContext(ctx)
	s.RemoteVersion(rec, r)

	if rec.Code != 200 {
		t.Fatalf("status: got %d, want 200 — body=%s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("json: %v", err)
	}
	// version + update_available are the two documented fields.
	if v, _ := body["version"].(string); v == "" {
		t.Errorf("version: empty, got %v", body["version"])
	}
	if body["update_available"] != false {
		t.Errorf("update_available: got %v, want false", body["update_available"])
	}
	// suchi is our provenance field — must be present so operators
	// can identify suchi instances in logs.
	if s, _ := body["suchi"].(string); s == "" {
		t.Errorf("suchi tag: empty")
	}
}

func TestRemoteVersion_RequiresAuth(t *testing.T) {
	d := openTestDB(t)
	s := &Server{DB: d, Log: slog.New(slog.NewTextHandler(os.Stderr, nil))}
	rec := httptest.NewRecorder()
	// No principal on the request context.
	r := httptest.NewRequest("GET", "/api/remote_version/", nil)
	s.RemoteVersion(rec, r)
	if rec.Code != 401 {
		t.Errorf("status: got %d, want 401", rec.Code)
	}
}

func TestNextASN_EmptyDatabase(t *testing.T) {
	d := openTestDB(t)
	s := &Server{DB: d, Log: slog.New(slog.NewTextHandler(os.Stderr, nil))}
	rec, ctx := authedRequest("GET", "/api/next_asn/")
	r := httptest.NewRequest("GET", "/api/next_asn/", nil).WithContext(ctx)
	s.NextASN(rec, r)
	if rec.Code != 200 {
		t.Fatalf("status: got %d, want 200 — body=%s", rec.Code, rec.Body.String())
	}
	// Empty db → next is 1.
	body := strings.TrimSpace(rec.Body.String())
	if body != "1" {
		t.Errorf("body: got %q, want %q", body, "1")
	}
}

func TestNextASN_WithASN(t *testing.T) {
	d := openTestDB(t)
	s := &Server{DB: d, Log: slog.New(slog.NewTextHandler(os.Stderr, nil))}
	seedUser(t, d, 1)
	seedJDCategory(t, d)
	// Seed two docs with ASNs 5 and 10 — next should be 11.
	if _, err := d.Write.ExecContext(context.Background(), `
		INSERT INTO documents(owner_id, title, original_blob, original_size, archive_serial_number, jd_category_id, created_at, added_at, updated_at)
		VALUES (1, 'a', 'aaa', 1, 5,  1, 0, 0, 0),
		       (1, 'b', 'bbb', 1, 10, 1, 0, 0, 0)
	`); err != nil {
		t.Fatal(err)
	}
	rec, ctx := authedRequest("GET", "/api/next_asn/")
	r := httptest.NewRequest("GET", "/api/next_asn/", nil).WithContext(ctx)
	s.NextASN(rec, r)
	if rec.Code != 200 {
		t.Fatalf("status: got %d, want 200 — body=%s", rec.Code, rec.Body.String())
	}
	body := strings.TrimSpace(rec.Body.String())
	if body != "11" {
		t.Errorf("body: got %q, want %q", body, "11")
	}
}

func TestNextASN_TrashedExcluded(t *testing.T) {
	d := openTestDB(t)
	s := &Server{DB: d, Log: slog.New(slog.NewTextHandler(os.Stderr, nil))}
	seedUser(t, d, 1)
	seedJDCategory(t, d)
	// Live doc ASN=3, trashed doc ASN=100. Next should be 4 — trashed
	// docs are excluded so undelete can't cause a duplicate ASN.
	if _, err := d.Write.ExecContext(context.Background(), `
		INSERT INTO documents(owner_id, title, original_blob, original_size, archive_serial_number, jd_category_id, trashed_at, created_at, added_at, updated_at)
		VALUES (1, 'live',    'a',   1, 3,   1, NULL, 0, 0, 0),
		       (1, 'trashed', 'b',   1, 100, 1, 999,  0, 0, 0)
	`); err != nil {
		t.Fatal(err)
	}
	rec, ctx := authedRequest("GET", "/api/next_asn/")
	r := httptest.NewRequest("GET", "/api/next_asn/", nil).WithContext(ctx)
	s.NextASN(rec, r)
	body := strings.TrimSpace(rec.Body.String())
	if body != "4" {
		t.Errorf("body: got %q, want %q (trashed doc must not count)", body, "4")
	}
}
