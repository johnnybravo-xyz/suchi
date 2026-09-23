// SPDX-License-Identifier: AGPL-3.0-or-later

package api

import (
	"net/http"
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
