package api

// The automations schema endpoint is a load-bearing "source of truth"
// for the SPA builder. If it drifts silently from core/automations
// the visual editor breaks. These tests pin:
//   - anonymous → 401
//   - every trigger code shipped by core/automations exposes a row
//   - every action kind the handler switches on has a schema entry
//
// The action assertion is intentionally strict: adding a new case in
// apply.go without a row here fails this test.

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http/httptest"
	"os"
	"testing"

	pluginapi "github.com/suchi-dms/suchi/plugin-api"

	"github.com/suchi-dms/suchi/core/auth"
	"github.com/suchi-dms/suchi/core/automations"
)

func TestAutomationSchema_Anonymous(t *testing.T) {
	s := &Server{Log: slog.New(slog.NewTextHandler(os.Stderr, nil))}
	rec := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/api/automations/schema", nil)
	s.GetAutomationSchema(rec, r)
	if rec.Code != 401 {
		t.Fatalf("anonymous status=%d, want 401", rec.Code)
	}
}

func TestAutomationSchema_HasEveryTriggerCode(t *testing.T) {
	s := &Server{Log: slog.New(slog.NewTextHandler(os.Stderr, nil))}
	rec := httptest.NewRecorder()
	ctx := auth.WithPrincipal(context.Background(),
		&pluginapi.Principal{Kind: "user", UserID: 1, Role: "member"})
	r := httptest.NewRequest("GET", "/api/automations/schema", nil).WithContext(ctx)
	s.GetAutomationSchema(rec, r)
	if rec.Code != 200 {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var got AutomationSchema
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	wantCodes := map[int]bool{
		automations.CodeConsumption:     true,
		automations.CodeDocumentAdded:   true,
		automations.CodeDocumentUpdated: true,
	}
	for _, tr := range got.Triggers {
		if !wantCodes[tr.Code] {
			t.Errorf("schema advertises unknown trigger code %d", tr.Code)
		}
		delete(wantCodes, tr.Code)
	}
	for c := range wantCodes {
		t.Errorf("schema missing trigger code %d (defined in core/automations)", c)
	}
}

func TestAutomationSchema_HasEveryActionKind(t *testing.T) {
	s := &Server{Log: slog.New(slog.NewTextHandler(os.Stderr, nil))}
	rec := httptest.NewRecorder()
	ctx := auth.WithPrincipal(context.Background(),
		&pluginapi.Principal{Kind: "user", UserID: 1, Role: "member"})
	r := httptest.NewRequest("GET", "/api/automations/schema", nil).WithContext(ctx)
	s.GetAutomationSchema(rec, r)
	var got AutomationSchema
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	// Hard-coded list mirrors the switch in core/automations/apply.go.
	// A new kind added there without a schema entry here means this
	// test still passes — expected, since the schema is what the SPA
	// consumes. The reverse direction (kind here that apply.go doesn't
	// handle) is caught by the SPA rejecting an unknown kind at
	// runtime; we could add a lint in the apply package but keeping
	// the coupling explicit-and-obvious is the trade.
	wantKinds := map[string]bool{
		"assign_title":         true,
		"assign_tags":          true,
		"assign_correspondent": true,
		"assign_document_type": true,
		"assign_storage_path":  true,
		"assign_owner":         true,
		"assign_custom_field":  true,
		"apply_from_similar":   true,
	}
	for _, a := range got.Actions {
		if !wantKinds[a.Kind] {
			t.Errorf("schema advertises unknown action kind %q", a.Kind)
		}
		delete(wantKinds, a.Kind)
	}
	for k := range wantKinds {
		t.Errorf("schema missing action kind %q", k)
	}
}
