// SPDX-License-Identifier: AGPL-3.0-or-later

package api

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestUploadDuplicateCompletedSplitKeepsOriginRetired(t *testing.T) {
	s, d, principal := newUploadMetadataServer(t)
	body := testPDFBytes()
	first := httptest.NewRecorder()
	s.UploadDocument(first, sourceUploadRequest(t, "batch.pdf", body, principal))
	if first.Code != http.StatusCreated {
		t.Fatalf("first upload status=%d body=%s", first.Code, first.Body.String())
	}
	var created UploadResponse
	if err := json.Unmarshal(first.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if _, err := d.ExecWrite(t.Context(), `
		INSERT INTO documents(
			system_id,owner_id,original_blob,original_size,title,mime_type,jd_category_id,
			split_parent_id,split_origin_id,split_index,created_at,updated_at
		)
		SELECT system_id,owner_id,'split-child-1',1,'Part 1',mime_type,jd_category_id,id,id,1,1,1
		FROM documents WHERE id=?;
		INSERT INTO documents(
			system_id,owner_id,original_blob,original_size,title,mime_type,jd_category_id,
			split_parent_id,split_origin_id,split_index,created_at,updated_at
		)
		SELECT system_id,owner_id,'split-child-2',1,'Part 2',mime_type,jd_category_id,id,id,2,1,1
		FROM documents WHERE id=?;
		UPDATE documents SET trashed_at=1,updated_at=1 WHERE id=?
	`, created.ID, created.ID, created.ID); err != nil {
		t.Fatal(err)
	}

	duplicate := httptest.NewRecorder()
	s.UploadDocument(duplicate, sourceUploadRequest(t, "batch-again.pdf", body, principal))
	if duplicate.Code != http.StatusOK {
		t.Fatalf("duplicate status=%d body=%s", duplicate.Code, duplicate.Body.String())
	}
	var replay UploadResponse
	if err := json.Unmarshal(duplicate.Body.Bytes(), &replay); err != nil {
		t.Fatal(err)
	}
	if replay.ID != created.ID || !replay.Deduplicated || replay.Restored {
		t.Fatalf("duplicate response = %+v", replay)
	}
	var retired sql.NullInt64
	if err := d.Read.QueryRowContext(t.Context(), `SELECT trashed_at FROM documents WHERE id=?`, created.ID).Scan(&retired); err != nil {
		t.Fatal(err)
	}
	if !retired.Valid {
		t.Fatal("completed split origin was restored")
	}

	if _, err := d.ExecWrite(t.Context(), `
		UPDATE documents SET trashed_at=2
		WHERE split_origin_id=? AND split_index=2
	`, created.ID); err != nil {
		t.Fatal(err)
	}
	blocked := httptest.NewRecorder()
	s.UploadDocument(blocked, sourceUploadRequest(t, "batch-third.pdf", body, principal))
	if blocked.Code != http.StatusConflict {
		t.Fatalf("trashed child status=%d body=%s", blocked.Code, blocked.Body.String())
	}
	var failure struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(blocked.Body.Bytes(), &failure); err != nil {
		t.Fatal(err)
	}
	if failure.Code != "split_origin_children_trashed" {
		t.Fatalf("failure code=%q", failure.Code)
	}
}
