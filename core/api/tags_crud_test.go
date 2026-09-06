package api

// CRUD roundtrip for POST/PATCH/DELETE /api/tags/. The list endpoint
// already has broader coverage; this pins the write verbs so a
// refactor of the shared helpers (slug.Make, isUniqueViolation)
// can't silently break them.

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http/httptest"
	"os"
	"strconv"
	"testing"

	pluginapi "github.com/johnnybravo-xyz/suchi/plugin-api"

	"github.com/johnnybravo-xyz/suchi/core/auth"
)

func TestTagCRUD_Roundtrip(t *testing.T) {
	d := openTestDB(t)
	s := &Server{DB: d, Log: slog.New(slog.NewTextHandler(os.Stderr, nil))}

	ctx := auth.WithPrincipal(context.Background(),
		&pluginapi.Principal{Kind: "user", UserID: 1, Email: "a@example.com", Role: "admin"})

	// Create.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/tags/",
		bytes.NewReader(mustJSON(t, map[string]any{"name": "tax-2026", "color": "#ff0000"}))).
		WithContext(ctx)
	s.CreateTag(rec, req)
	if rec.Code != 201 {
		t.Fatalf("create: status=%d body=%s", rec.Code, rec.Body.String())
	}
	var out struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || out.ID == 0 {
		t.Fatalf("create: bad response id: %v body=%s", err, rec.Body.String())
	}

	// Duplicate → 409.
	rec = httptest.NewRecorder()
	req = httptest.NewRequest("POST", "/api/tags/",
		bytes.NewReader(mustJSON(t, map[string]any{"name": "tax-2026"}))).WithContext(ctx)
	s.CreateTag(rec, req)
	if rec.Code != 409 {
		t.Fatalf("duplicate create: status=%d, want 409. body=%s", rec.Code, rec.Body.String())
	}

	// Update — rename.
	rec = httptest.NewRecorder()
	req = httptest.NewRequest("PATCH", "/api/tags/"+strconv.FormatInt(out.ID, 10),
		bytes.NewReader(mustJSON(t, map[string]any{"name": "tax-2027"}))).
		WithContext(ctx)
	req.SetPathValue("id", strconv.FormatInt(out.ID, 10))
	s.UpdateTag(rec, req)
	if rec.Code != 200 {
		t.Fatalf("update: status=%d body=%s", rec.Code, rec.Body.String())
	}

	// List — new name shows.
	rec = httptest.NewRecorder()
	req = httptest.NewRequest("GET", "/api/tags/", nil).WithContext(ctx)
	s.ListTags(rec, req)
	if rec.Code != 200 || !bytes.Contains(rec.Body.Bytes(), []byte(`"tax-2027"`)) {
		t.Errorf("list post-rename: status=%d body=%s", rec.Code, rec.Body.String())
	}

	// Delete.
	rec = httptest.NewRecorder()
	req = httptest.NewRequest("DELETE", "/api/tags/"+strconv.FormatInt(out.ID, 10), nil).WithContext(ctx)
	req.SetPathValue("id", strconv.FormatInt(out.ID, 10))
	s.DeleteTag(rec, req)
	if rec.Code != 204 {
		t.Fatalf("delete: status=%d body=%s", rec.Code, rec.Body.String())
	}

	// Delete again → 404.
	rec = httptest.NewRecorder()
	req = httptest.NewRequest("DELETE", "/api/tags/"+strconv.FormatInt(out.ID, 10), nil).WithContext(ctx)
	req.SetPathValue("id", strconv.FormatInt(out.ID, 10))
	s.DeleteTag(rec, req)
	if rec.Code != 404 {
		t.Errorf("delete-again: status=%d, want 404", rec.Code)
	}
}

func TestTagRenameTakesOwnershipOfClassifierReview(t *testing.T) {
	for _, field := range []string{"name", "slug", "color"} {
		t.Run(field, func(t *testing.T) {
			s := newBulkServer(t)
			docID := seedStatsDoc(t, s.DB, 1, "rename-review-sha", "Review", seedStatsJDInbox(t, s.DB), false, 0)
			if _, err := s.DB.Write.ExecContext(context.Background(), `
				INSERT INTO tags(id, name, slug, created_at, updated_at)
				VALUES (99, 'needs-review', 'needs-review', 0, 0);
				INSERT INTO document_tags(document_id, tag_id, classifier_owned) VALUES (?, 99, 1)
			`, docID); err != nil {
				t.Fatal(err)
			}
			value, wantOwned := "review-myself", 0
			if field == "color" {
				value, wantOwned = "#ff0000", 1
			}
			ctx := auth.WithPrincipal(context.Background(), adminPrincipal(1))
			req := httptest.NewRequest("PATCH", "/api/tags/99",
				bytes.NewReader(mustJSON(t, map[string]any{field: value}))).WithContext(ctx)
			req.SetPathValue("id", "99")
			rec := httptest.NewRecorder()
			s.UpdateTag(rec, req)
			if rec.Code != 200 {
				t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
			}
			var owned int
			if err := s.DB.Read.QueryRow(`SELECT classifier_owned FROM document_tags WHERE document_id = ?`, docID).Scan(&owned); err != nil {
				t.Fatal(err)
			}
			if owned != wantOwned {
				t.Fatalf("classifier_owned=%d, want %d", owned, wantOwned)
			}
		})
	}
}

func TestCreateTag_MemberForbidden(t *testing.T) {
	d := openTestDB(t)
	s := &Server{DB: d, Log: slog.New(slog.NewTextHandler(os.Stderr, nil))}

	ctx := auth.WithPrincipal(context.Background(),
		&pluginapi.Principal{Kind: "user", UserID: 2, Email: "m@example.com", Role: "member"})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/tags/",
		bytes.NewReader(mustJSON(t, map[string]any{"name": "hidden"}))).WithContext(ctx)
	s.CreateTag(rec, req)
	if rec.Code != 403 {
		t.Errorf("member create: status=%d, want 403", rec.Code)
	}
}
