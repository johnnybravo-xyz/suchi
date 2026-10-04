// SPDX-License-Identifier: AGPL-3.0-or-later

package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestCustomFieldValuesPreserveTrashedDocuments(t *testing.T) {
	for _, method := range []string{http.MethodPut, http.MethodDelete} {
		t.Run(method, func(t *testing.T) {
			s, mux := newSystemsBoundaryServer(t)
			seedSystemsBoundary(t, s)
			if _, err := s.DB.Write.Exec(`
				INSERT INTO document_custom_field_values(document_id,field_id,value_text) VALUES (101,101,'Preserved value');
				UPDATE documents SET trashed_at=unixepoch() WHERE id=101`); err != nil {
				t.Fatal(err)
			}
			var beforeJobs int
			if err := s.DB.Read.QueryRow(`SELECT count(*) FROM jobs WHERE doc_id=101`).Scan(&beforeJobs); err != nil {
				t.Fatal(err)
			}
			w := systemsBoundaryRequest(mux, method, "/api/documents/101/custom_fields/101",
				`{"value":"Must not replace"}`, memberPrincipal(5))
			if w.Code != http.StatusNotFound {
				t.Fatalf("trashed custom field mutation: %d %s", w.Code, w.Body.String())
			}
			var value string
			var afterJobs int
			if err := s.DB.Read.QueryRow(`SELECT value_text, (SELECT count(*) FROM jobs WHERE doc_id=101)
				FROM document_custom_field_values WHERE document_id=101 AND field_id=101`).Scan(&value, &afterJobs); err != nil {
				t.Fatal(err)
			}
			if value != "Preserved value" || afterJobs != beforeJobs {
				t.Fatalf("trashed field changed: value=%q jobs=%d (before %d)", value, afterJobs, beforeJobs)
			}
		})
	}
}

func TestDocumentLinkTopologyRejectsSelfAndVersionFamilyButAllowsPeerCycles(t *testing.T) {
	s, mux := newSystemsBoundaryServer(t)
	seedSystemsBoundary(t, s)
	seedDocumentReferences(t, s)

	put := func(source, target int64) *httptest.ResponseRecorder {
		t.Helper()
		return systemsBoundaryRequest(mux, http.MethodPut,
			fmt.Sprintf("/api/documents/%d/custom_fields/105", source),
			fmt.Sprintf(`{"value":%d}`, target), memberPrincipal(5))
	}
	assertCode := func(response *httptest.ResponseRecorder, status int, code string) {
		t.Helper()
		var body struct {
			Code string `json:"code"`
		}
		if response.Code != status || json.Unmarshal(response.Body.Bytes(), &body) != nil || body.Code != code {
			t.Fatalf("status=%d body=%s, want %d %s", response.Code, response.Body.String(), status, code)
		}
	}

	assertCode(put(101, 101), http.StatusBadRequest, "document_link_self")
	assertCode(put(101, 105), http.StatusBadRequest, "document_link_same_family")

	if response := put(101, 103); response.Code != http.StatusNoContent {
		t.Fatalf("first peer link: %d %s", response.Code, response.Body.String())
	}
	if response := put(103, 101); response.Code != http.StatusNoContent {
		t.Fatalf("reciprocal peer link: %d %s", response.Code, response.Body.String())
	}
	for source, target := range map[int64]int64{101: 103, 103: 101} {
		var stored int64
		if err := s.DB.Read.QueryRow(`
			SELECT value_int FROM document_custom_field_values
			WHERE document_id=? AND field_id=105
		`, source).Scan(&stored); err != nil {
			t.Fatal(err)
		}
		if stored != target {
			t.Fatalf("source %d target=%d, want %d", source, stored, target)
		}
	}
}
