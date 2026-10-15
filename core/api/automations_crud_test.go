// SPDX-License-Identifier: AGPL-3.0-or-later

package api

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/johnnybravo-xyz/suchi/core/auth"
)

func TestCreateAutomationAcceptsJSONNumberActionIDs(t *testing.T) {
	s := newBulkServer(t)
	seedUser(t, s.DB, 1)
	if _, err := s.DB.Write.ExecContext(context.Background(), `
		INSERT INTO correspondents(id, system_id, name, slug, created_at, updated_at)
		VALUES (1, 1, 'Existing correspondent', 'existing-correspondent', 0, 0);
		INSERT INTO tags(id, system_id, name, slug, created_at, updated_at)
		VALUES (1, 1, 'Existing tag', 'existing-tag', 0, 0)
	`); err != nil {
		t.Fatal(err)
	}

	for _, test := range []struct {
		name   string
		action string
	}{
		{name: "scalar id", action: `{"type":"assign_correspondent","params":{"correspondent_id":1}}`},
		{name: "id array", action: `{"type":"assign_tags","params":{"tag_ids":[1]}}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			body := []byte(fmt.Sprintf(`{
				"name":%q,
				"enabled":true,
				"triggers":[{"type":2}],
				"actions":[%s]
			}`, test.name, test.action))
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/api/automations/", bytes.NewReader(body))
			req = req.WithContext(auth.WithPrincipal(req.Context(), adminPrincipal(1)))

			s.CreateAutomation(rec, req)
			if rec.Code != http.StatusCreated {
				t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
			}
		})
	}
}

func TestDisabledModelAutomationsStayDormant(t *testing.T) {
	s := newBulkServer(t)
	seedUser(t, s.DB, 1)
	result, err := s.DB.Write.ExecContext(context.Background(), `
		INSERT INTO automations(system_id, name, order_index, enabled, created_at, updated_at)
		VALUES (1, 'Dormant ask', 0, 1, 0, 0)`)
	if err != nil {
		t.Fatal(err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.Write.ExecContext(context.Background(), `
		INSERT INTO automation_asks(automation_id, question, answer_type, choices)
		VALUES (?, 'Is this relevant?', 'yes_no', '[]')`, id); err != nil {
		t.Fatal(err)
	}
	s.setDeploymentFeatures(DeploymentFeatures{})
	mux := http.NewServeMux()
	s.Register(mux)
	request := func(method, path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, bytes.NewBufferString(body))
		req = req.WithContext(auth.WithPrincipal(req.Context(), adminPrincipal(1)))
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		return rec
	}

	if rec := request(http.MethodGet, "/api/automations/", ""); rec.Code != http.StatusOK ||
		bytes.Contains(rec.Body.Bytes(), []byte("Dormant ask")) {
		t.Fatalf("list exposed dormant ask: status=%d body=%s", rec.Code, rec.Body.String())
	}
	if rec := request(http.MethodGet, fmt.Sprintf("/api/automations/%d", id), ""); rec.Code != http.StatusNotFound {
		t.Fatalf("get status=%d body=%s", rec.Code, rec.Body.String())
	}
	if rec := request(http.MethodPatch, fmt.Sprintf("/api/automations/%d", id), `{"name":"changed"}`); rec.Code != http.StatusConflict {
		t.Fatalf("patch status=%d body=%s", rec.Code, rec.Body.String())
	}
	if rec := request(http.MethodDelete, fmt.Sprintf("/api/automations/%d", id), ""); rec.Code != http.StatusConflict {
		t.Fatalf("delete status=%d body=%s", rec.Code, rec.Body.String())
	}
	if rec := request(http.MethodPost, "/api/automations/", `{
		"name":"new ask","enabled":true,"triggers":[{"type":2}],
		"ask":{"question":"Relevant?","answer":{"type":"yes_no"}},"actions":[]
	}`); rec.Code != http.StatusConflict {
		t.Fatalf("create status=%d body=%s", rec.Code, rec.Body.String())
	}
}
