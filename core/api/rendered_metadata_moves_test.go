// SPDX-License-Identifier: AGPL-3.0-or-later

package api

// Renames and deletions of values a rendered-path template reads must queue
// render moves in the same transaction, only for live documents whose
// effective template reads that variable.

import (
	"net/http"
	"testing"
)

func renderJobCounts(t *testing.T, s *Server) map[int64]int {
	t.Helper()
	rows, err := s.DB.Read.Query(`SELECT doc_id, count(*) FROM jobs WHERE kind='render' GROUP BY doc_id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	counts := map[int64]int{}
	for rows.Next() {
		var docID int64
		var count int
		if err := rows.Scan(&docID, &count); err != nil {
			t.Fatal(err)
		}
		counts[docID] = count
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return counts
}

func assertRenderJobs(t *testing.T, step string, s *Server, want map[int64]int) {
	t.Helper()
	got := renderJobCounts(t, s)
	for _, id := range []int64{101, 102, 103, 104} {
		if got[id] != want[id] {
			t.Fatalf("%s: render jobs=%v, want %v", step, got, want)
		}
	}
}

func TestCorrespondentRenameAndDeleteQueueRenderedMoves(t *testing.T) {
	s, mux := newSystemsBoundaryServer(t)
	seedSystemsBoundary(t, s)
	// 101 and 102 list 101 as a sender in a layout that reads it; 103 uses the
	// default JD layout; 104 is trashed.
	if _, err := s.DB.Write.Exec(`
		INSERT INTO correspondents(id,system_id,name,slug,created_at,updated_at) VALUES (102,1,'Other','other',0,0);
		INSERT INTO storage_paths(id,system_id,name,slug,path,created_at,updated_at) VALUES (301,1,'By sender','by-sender','{{ correspondent }}/{{ title }}',0,0);
		UPDATE documents SET storage_path_id=301 WHERE id IN (101,102,104);
		INSERT INTO document_correspondents(document_id,correspondent_id,role,position) VALUES
			(101,101,'sender',0),(102,102,'sender',0),(102,101,'sender',1),(103,101,'sender',0),(104,101,'sender',0);
	`); err != nil {
		t.Fatal(err)
	}
	steps := []struct {
		name, method, body string
		want               map[int64]int
	}{
		{"rename", http.MethodPatch, `{"name":"Renamed sender"}`, map[int64]int{101: 1, 102: 1}},
		{"same name", http.MethodPatch, `{"name":"Renamed sender"}`, map[int64]int{101: 1, 102: 1}},
		{"match only", http.MethodPatch, `{"match":"acme"}`, map[int64]int{101: 1, 102: 1}},
		{"delete", http.MethodDelete, ``, map[int64]int{101: 2, 102: 2}},
	}
	for _, step := range steps {
		w := systemsBoundaryRequest(mux, step.method, "/api/correspondents/101?system=S01", step.body, adminPrincipal(1))
		if w.Code != http.StatusOK && w.Code != http.StatusNoContent {
			t.Fatalf("%s: status=%d body=%s", step.name, w.Code, w.Body.String())
		}
		assertRenderJobs(t, step.name, s, step.want)
	}
}

func TestTagRenameAndDeleteQueueRenderedMoves(t *testing.T) {
	s, mux := newSystemsBoundaryServer(t)
	seedSystemsBoundary(t, s)
	// 101 and 102 render tag_list and carry a tag; 103 carries it under the
	// default JD layout; 104 is trashed.
	if _, err := s.DB.Write.Exec(`
		INSERT INTO tags(id,system_id,name,slug,created_at,updated_at) VALUES (102,1,'Paid','paid',0,0),(103,1,'Unused','unused',0,0);
		INSERT INTO storage_paths(id,system_id,name,slug,path,created_at,updated_at) VALUES (301,1,'By tags','by-tags','{{ tag_list }}/{{ title }}',0,0);
		UPDATE documents SET storage_path_id=301 WHERE id IN (101,102,104);
		INSERT INTO document_tags(document_id,tag_id) VALUES (101,101),(102,102),(103,101),(104,101),(104,102);
	`); err != nil {
		t.Fatal(err)
	}
	steps := []struct {
		name, method, path, body string
		want                     map[int64]int
	}{
		{"rename", http.MethodPatch, "/api/tags/101", `{"name":"Renamed tag"}`, map[int64]int{101: 1}},
		{"same name", http.MethodPatch, "/api/tags/101", `{"name":"Renamed tag"}`, map[int64]int{101: 1}},
		{"color only", http.MethodPatch, "/api/tags/101", `{"color":"#123456"}`, map[int64]int{101: 1}},
		{"delete", http.MethodDelete, "/api/tags/101", ``, map[int64]int{101: 2}},
		{"batch delete", http.MethodDelete, "/api/tags/", `{"ids":[102,103]}`, map[int64]int{101: 2, 102: 1}},
	}
	for _, step := range steps {
		w := systemsBoundaryRequest(mux, step.method, step.path+"?system=S01", step.body, adminPrincipal(1))
		if w.Code != http.StatusOK && w.Code != http.StatusNoContent {
			t.Fatalf("%s: status=%d body=%s", step.name, w.Code, w.Body.String())
		}
		assertRenderJobs(t, step.name, s, step.want)
	}
}

func TestFilingSystemRenameQueuesRenderedMoves(t *testing.T) {
	s, mux := newSystemsBoundaryServer(t)
	seedSystemsBoundary(t, s)
	if _, err := s.DB.Write.Exec(`
		INSERT INTO storage_paths(id,system_id,name,slug,path,created_at,updated_at) VALUES (301,1,'By cabinet','by-cabinet','{{ jd.system.name }}/{{ title }}',0,0);
		UPDATE documents SET storage_path_id=301 WHERE id IN (101,104);
	`); err != nil {
		t.Fatal(err)
	}
	for _, step := range []struct {
		name string
		want map[int64]int
	}{
		{"Renamed cabinet", map[int64]int{101: 1}},
		{"Renamed cabinet", map[int64]int{101: 1}},
	} {
		w := systemsBoundaryRequest(mux, http.MethodPatch, "/api/admin/jd/systems/S01?system=S01", `{"name":"`+step.name+`"}`, adminPrincipal(1))
		if w.Code != http.StatusOK {
			t.Fatalf("rename: status=%d body=%s", w.Code, w.Body.String())
		}
		assertRenderJobs(t, "rename "+step.name, s, step.want)
	}
}

func TestClassifierReviewRemovalQueuesRenderedMove(t *testing.T) {
	s, mux := newSystemsBoundaryServer(t)
	seedSystemsBoundary(t, s)
	// 101 and 103 carry the classifier's review marker; 102 does not; 104 is
	// trashed. Only a live document that lost the marker moves.
	if _, err := s.DB.Write.Exec(`
		INSERT INTO tags(id,system_id,name,slug,created_at,updated_at) VALUES (199,1,'needs-review','needs-review',0,0);
		INSERT INTO document_tags(document_id,tag_id,classifier_owned) VALUES (101,199,1),(103,199,1),(104,199,1);
	`); err != nil {
		t.Fatal(err)
	}
	w := systemsBoundaryRequest(mux, http.MethodPost, "/api/documents/bulk_edit?system=S01",
		`{"documents":[101,102,104],"method":"set_sensitivity","parameters":{"sensitivity":"internal"}}`, adminPrincipal(1))
	if w.Code != http.StatusOK {
		t.Fatalf("bulk: status=%d body=%s", w.Code, w.Body.String())
	}
	assertRenderJobs(t, "bulk review removal", s, map[int64]int{101: 1})

	w = systemsBoundaryRequest(mux, http.MethodPatch, "/api/documents/103?system=S01", `{"sensitivity":"internal"}`, adminPrincipal(1))
	if w.Code != http.StatusOK {
		t.Fatalf("patch: status=%d body=%s", w.Code, w.Body.String())
	}
	assertRenderJobs(t, "patch review removal", s, map[int64]int{101: 1, 103: 1})
}

func TestBulkPathEditInTrashQueuesNoRenderedMove(t *testing.T) {
	s, mux := newSystemsBoundaryServer(t)
	seedSystemsBoundary(t, s)
	w := systemsBoundaryRequest(mux, http.MethodPost, "/api/documents/bulk_edit?system=S01",
		`{"documents":[103,104],"method":"set_jd_category","parameters":{"jd_category_id":111}}`, adminPrincipal(1))
	if w.Code != http.StatusOK {
		t.Fatalf("bulk: status=%d body=%s", w.Code, w.Body.String())
	}
	var category int64
	if err := s.DB.Read.QueryRow(`SELECT jd_category_id FROM documents WHERE id=104`).Scan(&category); err != nil {
		t.Fatal(err)
	}
	if category != 111 {
		t.Fatalf("trashed category=%d, want the edit applied", category)
	}
	assertRenderJobs(t, "trashed category edit", s, map[int64]int{103: 1})
}
