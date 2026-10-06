// SPDX-License-Identifier: AGPL-3.0-or-later

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
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"testing"

	pluginapi "github.com/johnnybravo-xyz/suchi/plugin-api"

	"github.com/johnnybravo-xyz/suchi/core/auth"
)

// TestCustomFieldDef_Roundtrip covers Create → Update → Delete for a
// select-typed field. Passes when:
//   - POST returns 201 with the new id
//   - GET /api/custom_fields/ shows the row with data_type + extra_data
//   - PATCH updates the name and preserves extra_data.choices
//   - DELETE returns 204 and the row disappears
func TestCustomFieldDef_Roundtrip(t *testing.T) {
	d := openTestDB(t)
	seedUser(t, d, 1)
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

func TestCustomFieldDefinitionProtectsTypedValuesAndActiveChoices(t *testing.T) {
	s, mux := newSystemsBoundaryServer(t)
	seedSystemsBoundary(t, s)
	if _, err := s.DB.Write.Exec(`
		INSERT INTO custom_fields(id,system_id,name,data_type,extra_data,created_at,updated_at)
		VALUES
			(120,1,'Priority','select','{"choices":["low","high"]}',0,0),
			(121,1,'Regions','multi','{"choices":["north","south"]}',0,0);
		INSERT INTO document_custom_field_values(document_id,field_id,value_text)
		VALUES
			(101,120,'high'),
			(101,121,'["north","south"]');
	`); err != nil {
		t.Fatal(err)
	}

	assertError := func(method, path, body string, status int, code string) {
		t.Helper()
		response := systemsBoundaryRequest(mux, method, path, body, adminPrincipal(1))
		var problem struct {
			Code string `json:"code"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &problem); err != nil {
			t.Fatalf("%s %s: decode %q: %v", method, path, response.Body.String(), err)
		}
		if response.Code != status || problem.Code != code {
			t.Fatalf("%s %s: status=%d code=%q body=%s; want %d %q",
				method, path, response.Code, problem.Code, response.Body.String(), status, code)
		}
	}

	assertError("POST", "/api/custom_fields/?system=S01",
		`{"name":"Incomplete choice","data_type":"select"}`,
		http.StatusBadRequest, "bad_choices")
	assertError("PATCH", "/api/custom_fields/120?system=S01",
		`{"data_type":"text"}`,
		http.StatusConflict, "data_type_in_use")
	assertError("PATCH", "/api/custom_fields/120?system=S01",
		`{"extra_data":"{\"choices\":[\"low\"]}"}`,
		http.StatusConflict, "choice_in_use")
	assertError("PATCH", "/api/custom_fields/121?system=S01",
		`{"extra_data":"{\"choices\":[\"north\"]}"}`,
		http.StatusConflict, "choice_in_use")

	updated := systemsBoundaryRequest(mux, "PATCH", "/api/custom_fields/120?system=S01",
		`{"extra_data":"{\"choices\":[\"low\",\"high\",\"urgent\"]}"}`, adminPrincipal(1))
	if updated.Code != http.StatusOK {
		t.Fatalf("add choice: status=%d body=%s", updated.Code, updated.Body.String())
	}
	var extra string
	if err := s.DB.Read.QueryRow(`SELECT extra_data FROM custom_fields WHERE id=120`).Scan(&extra); err != nil {
		t.Fatal(err)
	}
	if extra != `{"choices":["low","high","urgent"]}` {
		t.Fatalf("extra_data = %s", extra)
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
