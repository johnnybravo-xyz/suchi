// SPDX-License-Identifier: AGPL-3.0-or-later

package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/johnnybravo-xyz/suchi/core/auth"
)

func TestDocumentNotesRoundTripWithAttributionAndAuthorControl(t *testing.T) {
	s, mux := newSystemsBoundaryServer(t)
	seedSystemsBoundary(t, s)
	seedUser(t, s.DB, 7)
	if _, err := s.DB.Write.Exec(`UPDATE users SET role='member' WHERE id=7`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.Write.Exec(`INSERT OR IGNORE INTO jd_system_members(system_id,user_id,created_at) VALUES(1,5,0),(1,7,0)`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.Write.Exec(`
		INSERT INTO object_acls(object_kind,object_id,principal_kind,principal_id,perm_bits,created_at)
		VALUES('document',101,'user',7,3,0)
	`); err != nil {
		t.Fatal(err)
	}

	created := systemsBoundaryRequest(mux, http.MethodPost,
		"/api/documents/101/notes/?system=S01", `{"note":"Call the insurer before renewal."}`, memberPrincipal(5))
	if created.Code != http.StatusCreated {
		t.Fatalf("create: status=%d body=%s", created.Code, created.Body.String())
	}
	var note DocumentNote
	if err := json.Unmarshal(created.Body.Bytes(), &note); err != nil {
		t.Fatal(err)
	}
	if note.ID == 0 || note.UserID == nil || *note.UserID != 5 || !note.CanEdit || note.Author == "" {
		t.Fatalf("created note = %+v", note)
	}

	detail := systemsBoundaryRequest(mux, http.MethodGet, "/api/documents/101?system=S01", "", memberPrincipal(5))
	if detail.Code != http.StatusOK {
		t.Fatalf("detail: status=%d body=%s", detail.Code, detail.Body.String())
	}
	var projected struct {
		Notes []DocumentNote `json:"notes"`
	}
	if err := json.Unmarshal(detail.Body.Bytes(), &projected); err != nil {
		t.Fatal(err)
	}
	if len(projected.Notes) != 1 || projected.Notes[0].Note != "Call the insurer before renewal." {
		t.Fatalf("projected notes = %+v", projected.Notes)
	}

	path := fmt.Sprintf("/api/documents/101/notes/%d?system=S01", note.ID)
	forbiddenRequest := httptest.NewRequest(http.MethodPatch, path, strings.NewReader(`{"note":"Another editor overwrote this."}`))
	forbiddenRequest.SetPathValue("id", "101")
	forbiddenRequest.SetPathValue("note", fmt.Sprint(note.ID))
	ctx := context.WithValue(forbiddenRequest.Context(), systemContextKey{}, int64(1))
	forbiddenRequest = forbiddenRequest.WithContext(auth.WithPrincipal(ctx, memberPrincipal(7)))
	forbidden := httptest.NewRecorder()
	s.UpdateDocumentNote(forbidden, forbiddenRequest)
	if forbidden.Code != http.StatusForbidden {
		t.Fatalf("other editor patch: status=%d body=%s", forbidden.Code, forbidden.Body.String())
	}

	updated := systemsBoundaryRequest(mux, http.MethodPatch, path,
		`{"note":"Call the insurer and attach the renewal quote."}`, memberPrincipal(5))
	if updated.Code != http.StatusOK {
		t.Fatalf("author patch: status=%d body=%s", updated.Code, updated.Body.String())
	}
	var stored string
	if err := s.DB.Read.QueryRow(`SELECT note FROM notes WHERE id=?`, note.ID).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored != "Call the insurer and attach the renewal quote." {
		t.Fatalf("stored note = %q", stored)
	}

	deleted := systemsBoundaryRequest(mux, http.MethodDelete, path, "", adminPrincipal(1))
	if deleted.Code != http.StatusNoContent {
		t.Fatalf("admin delete: status=%d body=%s", deleted.Code, deleted.Body.String())
	}
	var remaining int
	if err := s.DB.Read.QueryRow(`SELECT count(*) FROM notes WHERE id=?`, note.ID).Scan(&remaining); err != nil {
		t.Fatal(err)
	}
	if remaining != 0 {
		t.Fatalf("remaining notes = %d", remaining)
	}
}
