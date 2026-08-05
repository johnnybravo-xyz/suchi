package api

// Roundtrip test for the field-definition CRUD API. Exercises the four
// verbs: Create → PATCH → DELETE with a select-typed field (the
// interesting case because extra_data.choices carries the vocabulary
// and must survive an update).

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http/httptest"
	"os"
	"strconv"
	"testing"

	pluginapi "github.com/suchi-dms/suchi/plugin-api"

	"github.com/suchi-dms/suchi/core/auth"
)

// TestCustomFieldDef_Roundtrip covers Create → Update → Delete for a
// select-typed field. Passes when:
//   - POST returns 201 with the new id
//   - GET /api/custom_fields/ shows the row with data_type + extra_data
//   - PATCH updates the name and preserves extra_data.choices
//   - DELETE returns 204 and the row disappears
func TestCustomFieldDef_Roundtrip(t *testing.T) {
	d := openTestDB(t)
	s := &Server{DB: d, Log: slog.New(slog.NewTextHandler(os.Stderr, nil))}

	ctx := auth.WithPrincipal(context.Background(),
		&pluginapi.Principal{Kind: "user", UserID: 1, Email: "a@example.com", Role: "admin"})

	// Create.
	body := mustJSON(t, map[string]any{
		"name":       "priority",
		"data_type":  "select",
		"extra_data": `{"choices":["low","medium","high"]}`,
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/custom_fields/", bytes.NewReader(body)).WithContext(ctx)
	s.CreateCustomFieldDef(rec, req)
	if rec.Code != 201 {
		t.Fatalf("create: status=%d body=%s", rec.Code, rec.Body.String())
	}
	var createOut struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &createOut); err != nil {
		t.Fatal(err)
	}
	if createOut.ID == 0 {
		t.Fatalf("create: id=0")
	}

	// List — the row must appear with its data_type + preserved
	// extra_data.
	rec = httptest.NewRecorder()
	req = httptest.NewRequest("GET", "/api/custom_fields/", nil).WithContext(ctx)
	s.ListCustomFieldDefs(rec, req)
	if rec.Code != 200 {
		t.Fatalf("list: status=%d body=%s", rec.Code, rec.Body.String())
	}
	if !bytes.Contains(rec.Body.Bytes(), []byte(`"priority"`)) {
		t.Errorf("list: created row not present. body=%s", rec.Body.String())
	}
	if !bytes.Contains(rec.Body.Bytes(), []byte(`"select"`)) {
		t.Errorf("list: data_type missing. body=%s", rec.Body.String())
	}
	if !bytes.Contains(rec.Body.Bytes(), []byte(`high`)) {
		t.Errorf("list: extra_data.choices not preserved. body=%s", rec.Body.String())
	}

	// Update the name only. extra_data must survive since we don't
	// touch it.
	body = mustJSON(t, map[string]any{"name": "urgency"})
	rec = httptest.NewRecorder()
	req = httptest.NewRequest("PATCH",
		"/api/custom_fields/"+strconv.FormatInt(createOut.ID, 10),
		bytes.NewReader(body)).WithContext(ctx)
	req.SetPathValue("id", strconv.FormatInt(createOut.ID, 10))
	s.UpdateCustomFieldDef(rec, req)
	if rec.Code != 200 {
		t.Fatalf("update: status=%d body=%s", rec.Code, rec.Body.String())
	}

	rec = httptest.NewRecorder()
	req = httptest.NewRequest("GET", "/api/custom_fields/", nil).WithContext(ctx)
	s.ListCustomFieldDefs(rec, req)
	if !bytes.Contains(rec.Body.Bytes(), []byte(`"urgency"`)) {
		t.Errorf("update: name not renamed. body=%s", rec.Body.String())
	}
	if bytes.Contains(rec.Body.Bytes(), []byte(`"priority"`)) {
		t.Errorf("update: old name still present. body=%s", rec.Body.String())
	}
	if !bytes.Contains(rec.Body.Bytes(), []byte(`high`)) {
		t.Errorf("update: extra_data.choices lost after name update. body=%s", rec.Body.String())
	}

	// Delete.
	rec = httptest.NewRecorder()
	req = httptest.NewRequest("DELETE",
		"/api/custom_fields/"+strconv.FormatInt(createOut.ID, 10),
		nil).WithContext(ctx)
	req.SetPathValue("id", strconv.FormatInt(createOut.ID, 10))
	s.DeleteCustomFieldDef(rec, req)
	if rec.Code != 204 {
		t.Fatalf("delete: status=%d body=%s", rec.Code, rec.Body.String())
	}

	rec = httptest.NewRecorder()
	req = httptest.NewRequest("GET", "/api/custom_fields/", nil).WithContext(ctx)
	s.ListCustomFieldDefs(rec, req)
	if bytes.Contains(rec.Body.Bytes(), []byte(`"urgency"`)) {
		t.Errorf("delete: row still visible in list. body=%s", rec.Body.String())
	}
}

// Non-admin members cannot mutate the schema (create/update/delete).
// Reads are open — the list endpoint is used by autocomplete + the
// per-doc value editor and would be unusable otherwise.
func TestCustomFieldDef_NonAdminRefused(t *testing.T) {
	d := openTestDB(t)
	s := &Server{DB: d, Log: slog.New(slog.NewTextHandler(os.Stderr, nil))}

	// Member principal — role=member, not admin.
	ctx := auth.WithPrincipal(context.Background(),
		&pluginapi.Principal{Kind: "user", UserID: 2, Email: "m@example.com", Role: "member"})

	body := mustJSON(t, map[string]any{"name": "x", "data_type": "text"})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/custom_fields/", bytes.NewReader(body)).WithContext(ctx)
	s.CreateCustomFieldDef(rec, req)
	if rec.Code == 201 {
		t.Fatalf("create: non-admin succeeded (status=201) — expected 403")
	}
	if rec.Code != 403 {
		t.Fatalf("create: status=%d body=%s — expected 403", rec.Code, rec.Body.String())
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
