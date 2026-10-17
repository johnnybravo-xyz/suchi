// SPDX-License-Identifier: AGPL-3.0-or-later

package api

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestPatchDocumentValidatesArchiveSerialNumber(t *testing.T) {
	s, mux := newSystemsBoundaryServer(t)
	seedSystemsBoundary(t, s)

	for _, value := range []string{
		`0`,
		`-1`,
		`1.5`,
		`9007199254740992`,
		`"42"`,
		`[]`,
	} {
		t.Run(value, func(t *testing.T) {
			response := systemsBoundaryRequest(mux, http.MethodPatch, "/api/documents/101?system=S01",
				`{"archive_serial_number":`+value+`}`, adminPrincipal(1))
			if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), `"code":"bad_archive_serial_number"`) {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}
		})
	}
}

func TestPatchDocumentArchiveSerialNumberRetentionAndSystemScope(t *testing.T) {
	s, mux := newSystemsBoundaryServer(t)
	seedSystemsBoundary(t, s)

	patch := func(path, body string) *httptest.ResponseRecorder {
		t.Helper()
		return systemsBoundaryRequest(mux, http.MethodPatch, path, body, adminPrincipal(1))
	}

	assigned := patch("/api/documents/101?system=S01", `{"archive_serial_number":42}`)
	if assigned.Code != http.StatusOK {
		t.Fatalf("assign: status=%d body=%s", assigned.Code, assigned.Body.String())
	}

	conflict := patch("/api/documents/103?system=S01", `{"archive_serial_number":42}`)
	if conflict.Code != http.StatusConflict || !strings.Contains(conflict.Body.String(), `"code":"archive_serial_number_conflict"`) {
		t.Fatalf("live conflict: status=%d body=%s", conflict.Code, conflict.Body.String())
	}

	if _, err := s.DB.Write.Exec(`UPDATE documents SET trashed_at=1 WHERE id=101`); err != nil {
		t.Fatal(err)
	}
	retained := patch("/api/documents/103?system=S01", `{"archive_serial_number":42}`)
	if retained.Code != http.StatusConflict {
		t.Fatalf("trash conflict: status=%d body=%s", retained.Code, retained.Body.String())
	}

	cleared := patch("/api/documents/101?system=S01", `{"archive_serial_number":null}`)
	if cleared.Code != http.StatusOK {
		t.Fatalf("clear trash: status=%d body=%s", cleared.Code, cleared.Body.String())
	}
	reused := patch("/api/documents/103?system=S01", `{"archive_serial_number":42}`)
	if reused.Code != http.StatusOK {
		t.Fatalf("reuse after clear: status=%d body=%s", reused.Code, reused.Body.String())
	}

	foreign := patch("/api/documents/201?system=S02", `{"archive_serial_number":42}`)
	if foreign.Code != http.StatusOK {
		t.Fatalf("cross-system reuse: status=%d body=%s", foreign.Code, foreign.Body.String())
	}
}

func TestPatchDocumentArchiveSerialNumberRequiresCurrentVersionButCanClearHistory(t *testing.T) {
	s, mux := newSystemsBoundaryServer(t)
	seedSystemsBoundary(t, s)
	if _, err := s.DB.Write.Exec(`
		UPDATE documents SET version_family_key='family', archive_serial_number=55 WHERE id=101;
		UPDATE documents SET version_family_key='family' WHERE id=103;
	`); err != nil {
		t.Fatal(err)
	}

	assign := systemsBoundaryRequest(mux, http.MethodPatch, "/api/documents/101?system=S01",
		`{"archive_serial_number":56}`, adminPrincipal(1))
	if assign.Code != http.StatusConflict || !strings.Contains(assign.Body.String(), `"code":"archive_serial_number_requires_current_version"`) {
		t.Fatalf("assign history: status=%d body=%s", assign.Code, assign.Body.String())
	}

	clear := systemsBoundaryRequest(mux, http.MethodPatch, "/api/documents/101?system=S01",
		`{"archive_serial_number":null}`, adminPrincipal(1))
	if clear.Code != http.StatusOK {
		t.Fatalf("clear history: status=%d body=%s", clear.Code, clear.Body.String())
	}
	var stored sql.NullInt64
	if err := s.DB.Read.QueryRow(`SELECT archive_serial_number FROM documents WHERE id=101`).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored.Valid {
		t.Fatalf("archive serial number after clear = %d", stored.Int64)
	}
}

func TestPatchDocumentArchiveSerialNumberCoordinatesASNLayout(t *testing.T) {
	s, mux := newSystemsBoundaryServer(t)
	seedSystemsBoundary(t, s)
	if _, err := s.DB.Write.Exec(`
		INSERT INTO storage_paths(id,system_id,name,slug,path,created_at,updated_at)
		VALUES (301,1,'ASN folders','asn-folders','Archive/{{ asn }}/{{ title }}',0,0)
	`); err != nil {
		t.Fatal(err)
	}

	var auditBefore, jobsBefore int
	if err := s.DB.Read.QueryRow(`SELECT count(*) FROM audit_events WHERE action='document.update' AND object_id=101`).Scan(&auditBefore); err != nil {
		t.Fatal(err)
	}
	if err := s.DB.Read.QueryRow(`SELECT count(*) FROM jobs WHERE kind='render' AND doc_id=101`).Scan(&jobsBefore); err != nil {
		t.Fatal(err)
	}

	combined := systemsBoundaryRequest(mux, http.MethodPatch, "/api/documents/101?system=S01",
		`{"archive_serial_number":77,"rendered_layout_id":301}`, adminPrincipal(1))
	if combined.Code != http.StatusOK {
		t.Fatalf("combined assign: status=%d body=%s", combined.Code, combined.Body.String())
	}
	var asn sql.NullInt64
	var layout sql.NullInt64
	if err := s.DB.Read.QueryRow(`SELECT archive_serial_number, storage_path_id FROM documents WHERE id=101`).Scan(&asn, &layout); err != nil {
		t.Fatal(err)
	}
	if !asn.Valid || asn.Int64 != 77 || !layout.Valid || layout.Int64 != 301 {
		t.Fatalf("stored ASN/layout = %v/%v", asn, layout)
	}

	var auditAfter, jobsAfter int
	if err := s.DB.Read.QueryRow(`SELECT count(*) FROM audit_events WHERE action='document.update' AND object_id=101`).Scan(&auditAfter); err != nil {
		t.Fatal(err)
	}
	if err := s.DB.Read.QueryRow(`SELECT count(*) FROM jobs WHERE kind='render' AND doc_id=101`).Scan(&jobsAfter); err != nil {
		t.Fatal(err)
	}
	if auditAfter != auditBefore+1 || jobsAfter != jobsBefore+1 {
		t.Fatalf("combined side effects: audit=%d want %d, jobs=%d want %d", auditAfter, auditBefore+1, jobsAfter, jobsBefore+1)
	}

	retry := systemsBoundaryRequest(mux, http.MethodPatch, "/api/documents/101?system=S01",
		`{"archive_serial_number":77}`, adminPrincipal(1))
	if retry.Code != http.StatusOK {
		t.Fatalf("same-value retry: status=%d body=%s", retry.Code, retry.Body.String())
	}
	var auditAfterRetry, jobsAfterRetry int
	if err := s.DB.Read.QueryRow(`SELECT count(*) FROM audit_events WHERE action='document.update' AND object_id=101`).Scan(&auditAfterRetry); err != nil {
		t.Fatal(err)
	}
	if err := s.DB.Read.QueryRow(`SELECT count(*) FROM jobs WHERE kind='render' AND doc_id=101`).Scan(&jobsAfterRetry); err != nil {
		t.Fatal(err)
	}
	if auditAfterRetry != auditAfter || jobsAfterRetry != jobsAfter {
		t.Fatalf("same-value retry created side effects: audit=%d jobs=%d", auditAfterRetry, jobsAfterRetry)
	}

	blocked := systemsBoundaryRequest(mux, http.MethodPatch, "/api/documents/101?system=S01",
		`{"archive_serial_number":null}`, adminPrincipal(1))
	if blocked.Code != http.StatusConflict || !strings.Contains(blocked.Body.String(), `"code":"rendered_layout_requires_archive_number"`) {
		t.Fatalf("clear with ASN layout: status=%d body=%s", blocked.Code, blocked.Body.String())
	}
	cleared := systemsBoundaryRequest(mux, http.MethodPatch, "/api/documents/101?system=S01",
		`{"archive_serial_number":null,"rendered_layout_id":0}`, adminPrincipal(1))
	if cleared.Code != http.StatusOK {
		t.Fatalf("coordinated clear: status=%d body=%s", cleared.Code, cleared.Body.String())
	}
}

func TestArchiveSerialNumberCollectionProjections(t *testing.T) {
	s, mux := newSystemsBoundaryServer(t)
	seedSystemsBoundary(t, s)
	if _, err := s.DB.Write.Exec(`
		UPDATE documents SET archive_serial_number=4021 WHERE id=101;
		UPDATE documents SET archive_serial_number=4022 WHERE id=104;
	`); err != nil {
		t.Fatal(err)
	}

	documentsResponse := systemsBoundaryRequest(
		mux, http.MethodGet, "/api/documents/?system=S01", "", adminPrincipal(1),
	)
	var documents Envelope[DocumentListRow]
	if documentsResponse.Code != http.StatusOK ||
		json.Unmarshal(documentsResponse.Body.Bytes(), &documents) != nil {
		t.Fatalf("documents: status=%d body=%s", documentsResponse.Code, documentsResponse.Body.String())
	}
	var projectedList bool
	for _, document := range documents.Results {
		if document.ID == 101 {
			projectedList = document.ArchiveSerialNumber != nil && *document.ArchiveSerialNumber == 4021
		}
	}
	if !projectedList {
		t.Fatalf("list projection = %+v", documents.Results)
	}

	searchResponse := systemsBoundaryRequest(
		mux, http.MethodGet, "/api/search/?system=S01&q=needle", "", adminPrincipal(1),
	)
	var search Envelope[SearchHit]
	if searchResponse.Code != http.StatusOK ||
		json.Unmarshal(searchResponse.Body.Bytes(), &search) != nil {
		t.Fatalf("search: status=%d body=%s", searchResponse.Code, searchResponse.Body.String())
	}
	var projectedSearch bool
	for _, hit := range search.Results {
		if hit.ID == 101 {
			projectedSearch = hit.ArchiveSerialNumber != nil && *hit.ArchiveSerialNumber == 4021
		}
	}
	if !projectedSearch {
		t.Fatalf("search projection = %+v", search.Results)
	}

	trashResponse := systemsBoundaryRequest(
		mux, http.MethodGet, "/api/trash/?system=S01", "", adminPrincipal(1),
	)
	var trashRows Envelope[TrashRow]
	if trashResponse.Code != http.StatusOK ||
		json.Unmarshal(trashResponse.Body.Bytes(), &trashRows) != nil {
		t.Fatalf("trash: status=%d body=%s", trashResponse.Code, trashResponse.Body.String())
	}
	var projectedTrash bool
	for _, row := range trashRows.Results {
		if row.ID == 104 {
			projectedTrash = row.ArchiveSerialNumber != nil && *row.ArchiveSerialNumber == 4022
		}
	}
	if !projectedTrash {
		t.Fatalf("trash projection = %+v", trashRows.Results)
	}
}
