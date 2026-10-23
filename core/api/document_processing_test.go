// SPDX-License-Identifier: AGPL-3.0-or-later

package api

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"
)

func TestDocumentDetailReportsUnfinishedProcessing(t *testing.T) {
	s, mux, cas := newTrashAPIServer(t)
	now := time.Now().Unix()
	reading := seedTrashAPIDocument(t, s, cas, 1, "Still reading", false, now)
	finished := seedTrashAPIDocument(t, s, cas, 1, "Finished", false, now)
	if _, err := s.DB.Write.Exec(`
		INSERT INTO jobs(system_id, doc_id, kind, state, next_run_at, created_at, updated_at)
		VALUES (1, ?, 'post-ingest', 'pending', 0, 0, 0),
		       (1, ?, 'post-ingest', 'done', 0, 0, 0),
		       (1, ?, 'render', 'pending', 0, 0, 0)`, reading, finished, finished); err != nil {
		t.Fatal(err)
	}
	for id, want := range map[int64]bool{reading: true, finished: false} {
		response := doTrashAPIRequest(t, mux, http.MethodGet, "/api/documents/"+itoa(id), memberPrincipal(1))
		var detail struct {
			Processing bool `json:"processing"`
		}
		if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &detail) != nil || detail.Processing != want {
			t.Fatalf("document %d: status=%d processing=%v, want %v; body=%s", id, response.Code, detail.Processing, want, response.Body.String())
		}
	}
}
