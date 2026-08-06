package api

// CRUD roundtrip for POST/PATCH/DELETE /api/tags/. The list endpoint
// already has broader coverage; this pins the write verbs so a
// refactor of the shared helpers (slugFromName, isUniqueViolation)
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
