// SPDX-License-Identifier: AGPL-3.0-or-later

package api

import (
	"encoding/json"
	"net/http"
	"testing"
)

func TestRenderedLayoutPreviewAndDocumentAssignment(t *testing.T) {
	s, mux := newSystemsBoundaryServer(t)
	seedSystemsBoundary(t, s)

	preview := systemsBoundaryRequest(mux, http.MethodPost, "/api/rendered_layouts/preview?system=S01",
		`{"template":"Bills/{{ created_year }}/{{ correspondent }}/{{ title }}-{{ asn }}"}`, adminPrincipal(1))
	if preview.Code != http.StatusOK {
		t.Fatalf("preview: status=%d body=%s", preview.Code, preview.Body.String())
	}
	var sample renderedLayoutPreview
	if err := json.Unmarshal(preview.Body.Bytes(), &sample); err != nil {
		t.Fatal(err)
	}
	if sample.Path != "Bills/2026/City Energy/March electricity bill-4021" || !sample.UsesASN {
		t.Fatalf("preview = %+v", sample)
	}

	reserved := systemsBoundaryRequest(mux, http.MethodPost, "/api/rendered_layouts/preview?system=S01",
		`{"template":"00-09 System index/{{ title }}"}`, adminPrincipal(1))
	if reserved.Code != http.StatusBadRequest {
		t.Fatalf("reserved preview: status=%d body=%s", reserved.Code, reserved.Body.String())
	}

	created := systemsBoundaryRequest(mux, http.MethodPost, "/api/rendered_layouts/?system=S01",
		`{"name":"Previous archive folders","path":"Legacy/{{ asn }}/{{ title }}"}`, adminPrincipal(1))
	if created.Code != http.StatusCreated {
		t.Fatalf("create: status=%d body=%s", created.Code, created.Body.String())
	}
	var createdBody struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal(created.Body.Bytes(), &createdBody); err != nil {
		t.Fatal(err)
	}

	assignPath := "/api/documents/101?system=S01"
	missingASN := systemsBoundaryRequest(mux, http.MethodPatch, assignPath,
		`{"rendered_layout_id":`+itoa(createdBody.ID)+`}`, memberPrincipal(5))
	if missingASN.Code != http.StatusConflict {
		t.Fatalf("assign without archive number: status=%d body=%s", missingASN.Code, missingASN.Body.String())
	}
	if _, err := s.DB.Write.Exec(`UPDATE documents SET archive_serial_number=42 WHERE id=101`); err != nil {
		t.Fatal(err)
	}
	assigned := systemsBoundaryRequest(mux, http.MethodPatch, assignPath,
		`{"rendered_layout_id":`+itoa(createdBody.ID)+`}`, memberPrincipal(5))
	if assigned.Code != http.StatusOK {
		t.Fatalf("assign: status=%d body=%s", assigned.Code, assigned.Body.String())
	}

	detail := systemsBoundaryRequest(mux, http.MethodGet, assignPath, "", memberPrincipal(5))
	if detail.Code != http.StatusOK {
		t.Fatalf("detail: status=%d body=%s", detail.Code, detail.Body.String())
	}
	var document DocumentDetail
	if err := json.Unmarshal(detail.Body.Bytes(), &document); err != nil {
		t.Fatal(err)
	}
	if document.ArchiveSerialNumber == nil || *document.ArchiveSerialNumber != 42 ||
		document.RenderedLayout == nil || document.RenderedLayout.ID != createdBody.ID || !document.RenderedLayout.UsesASN {
		t.Fatalf("detail projection = %+v", document)
	}

	var jobsBeforeUpdate int
	if err := s.DB.Read.QueryRow(`SELECT count(*) FROM jobs WHERE kind='render' AND doc_id=101`).Scan(&jobsBeforeUpdate); err != nil {
		t.Fatal(err)
	}
	updated := systemsBoundaryRequest(mux, http.MethodPatch,
		"/api/rendered_layouts/"+itoa(createdBody.ID)+"?system=S01",
		`{"path":"Updated/{{ title }}"}`, adminPrincipal(1))
	if updated.Code != http.StatusOK {
		t.Fatalf("update: status=%d body=%s", updated.Code, updated.Body.String())
	}
	var jobsAfterUpdate int
	if err := s.DB.Read.QueryRow(`SELECT count(*) FROM jobs WHERE kind='render' AND doc_id=101`).Scan(&jobsAfterUpdate); err != nil {
		t.Fatal(err)
	}
	if jobsAfterUpdate != jobsBeforeUpdate+1 {
		t.Fatalf("render jobs after template update = %d, want %d", jobsAfterUpdate, jobsBeforeUpdate+1)
	}

	cleared := systemsBoundaryRequest(mux, http.MethodPatch, assignPath,
		`{"rendered_layout_id":0}`, memberPrincipal(5))
	if cleared.Code != http.StatusOK {
		t.Fatalf("clear: status=%d body=%s", cleared.Code, cleared.Body.String())
	}
	var stored any
	if err := s.DB.Read.QueryRow(`SELECT storage_path_id FROM documents WHERE id=101`).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored != nil {
		t.Fatalf("stored layout after clear = %v", stored)
	}

	reassigned := systemsBoundaryRequest(mux, http.MethodPatch, assignPath,
		`{"rendered_layout_id":`+itoa(createdBody.ID)+`}`, memberPrincipal(5))
	if reassigned.Code != http.StatusOK {
		t.Fatalf("reassign: status=%d body=%s", reassigned.Code, reassigned.Body.String())
	}
	var jobsBeforeDelete int
	if err := s.DB.Read.QueryRow(`SELECT count(*) FROM jobs WHERE kind='render' AND doc_id=101`).Scan(&jobsBeforeDelete); err != nil {
		t.Fatal(err)
	}
	deleted := systemsBoundaryRequest(mux, http.MethodDelete,
		"/api/rendered_layouts/"+itoa(createdBody.ID)+"?system=S01", "", adminPrincipal(1))
	if deleted.Code != http.StatusNoContent {
		t.Fatalf("delete: status=%d body=%s", deleted.Code, deleted.Body.String())
	}
	if err := s.DB.Read.QueryRow(`SELECT storage_path_id FROM documents WHERE id=101`).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored != nil {
		t.Fatalf("stored layout after delete = %v", stored)
	}
	var jobsAfterDelete int
	if err := s.DB.Read.QueryRow(`SELECT count(*) FROM jobs WHERE kind='render' AND doc_id=101`).Scan(&jobsAfterDelete); err != nil {
		t.Fatal(err)
	}
	if jobsAfterDelete != jobsBeforeDelete+1 {
		t.Fatalf("render jobs after layout delete = %d, want %d", jobsAfterDelete, jobsBeforeDelete+1)
	}
}
