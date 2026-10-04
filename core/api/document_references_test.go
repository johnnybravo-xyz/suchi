// SPDX-License-Identifier: AGPL-3.0-or-later

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/johnnybravo-xyz/suchi/core/auth"
)

func seedDocumentReferences(t *testing.T, s *Server) {
	t.Helper()
	if _, err := s.DB.Write.Exec(`
		INSERT INTO jd_system_members(system_id,user_id,created_at) VALUES (2,5,0);
		INSERT INTO custom_fields(id,system_id,name,data_type,extra_data,created_at,updated_at) VALUES
			(102,1,'Amount','number','{}',0,0),
			(103,1,'Active','bool','{}',0,0),
			(104,1,'Labels','multi','{"choices":["a","b"]}',0,0),
			(105,1,'Related','documentlink','{}',0,0),
			(106,1,'Secret','documentlink','{}',0,0),
			(107,1,'Due','date','{}',0,0);
		INSERT INTO document_custom_field_values(document_id,field_id,value_text) VALUES
			(101,101,'memo'),(101,104,'["a","b"]');
		INSERT INTO document_custom_field_values(document_id,field_id,value_number) VALUES (101,102,12.5);
		INSERT INTO document_custom_field_values(document_id,field_id,value_bool) VALUES (101,103,0);
		INSERT INTO document_custom_field_values(document_id,field_id,value_date) VALUES (101,107,1704067200);
		INSERT INTO document_custom_field_values(document_id,field_id,value_int) VALUES
			(101,105,202),(101,106,204),(103,105,202),(102,105,202);

		UPDATE documents SET version_family_key='v:source-family' WHERE id=101;
		INSERT INTO documents(id,system_id,owner_id,jd_category_id,title,content,original_blob,original_size,mime_type,sensitivity,version_family_key,created_at,updated_at)
		SELECT 105,system_id,owner_id,jd_category_id,'Newer source','','source-newer',original_size,mime_type,sensitivity,'v:source-family',1,1 FROM documents WHERE id=101;

		UPDATE documents SET version_family_key='v:target-family' WHERE id=202;
		INSERT INTO documents(id,system_id,owner_id,jd_category_id,title,content,original_blob,original_size,mime_type,sensitivity,version_family_key,created_at,updated_at)
		SELECT 206,system_id,owner_id,jd_category_id,'Newer target','','target-newer',original_size,mime_type,sensitivity,'v:target-family',1,1 FROM documents WHERE id=202;
		INSERT INTO object_acls(object_kind,object_id,principal_kind,principal_id,perm_bits,created_at)
		VALUES ('document',206,'user',5,1,0);
	`); err != nil {
		t.Fatal(err)
	}
}

func TestDocumentDetailProjectsTypedCustomFieldsWithoutHiddenTargets(t *testing.T) {
	s, mux := newSystemsBoundaryServer(t)
	seedSystemsBoundary(t, s)
	seedDocumentReferences(t, s)

	w := systemsBoundaryRequest(mux, http.MethodGet, "/api/documents/101", "", memberPrincipal(5))
	if w.Code != http.StatusOK {
		t.Fatalf("detail: %d %s", w.Code, w.Body.String())
	}
	var body struct {
		CustomFields []struct {
			FieldID  int64           `json:"field_id"`
			Name     string          `json:"name"`
			DataType string          `json:"data_type"`
			Value    json.RawMessage `json:"value"`
		} `json:"custom_fields"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	values := make(map[string]json.RawMessage, len(body.CustomFields))
	for _, field := range body.CustomFields {
		values[field.Name] = field.Value
	}
	if len(values) != 6 {
		t.Fatalf("custom fields = %s", w.Body.String())
	}
	for name, want := range map[string]string{
		"Active": "false", "Amount": "12.5", "Due": "1704067200", "Labels": `["a","b"]`, "Local field": `"memo"`,
	} {
		if got := string(values[name]); got != want {
			t.Errorf("%s value = %s, want %s", name, got, want)
		}
	}
	if _, exists := values["Secret"]; exists {
		t.Fatalf("hidden target field leaked: %s", w.Body.String())
	}
	var target DocumentReferenceSummary
	if err := json.Unmarshal(values["Related"], &target); err != nil {
		t.Fatal(err)
	}
	if target.ID != 202 || target.Title != "Needle foreign direct" || target.IsLatest {
		t.Fatalf("visible exact target = %+v", target)
	}
}

func TestReferencedByCountsVisibleCrossSystemSourcesBeforePaging(t *testing.T) {
	s, mux := newSystemsBoundaryServer(t)
	seedSystemsBoundary(t, s)
	seedDocumentReferences(t, s)

	w := systemsBoundaryRequest(mux, http.MethodGet, "/api/documents/202/referenced-by/?page_size=1", "", memberPrincipal(5))
	if w.Code != http.StatusOK {
		t.Fatalf("backlinks: %d %s", w.Code, w.Body.String())
	}
	var page Envelope[DocumentBacklink]
	if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if page.Count != 2 || len(page.Results) != 1 || page.Results[0].ID != 103 || page.Results[0].FieldName != "Related" {
		t.Fatalf("first page = %+v body=%s", page, w.Body.String())
	}
	w = systemsBoundaryRequest(mux, http.MethodGet, "/api/documents/202/referenced-by/?page_size=1&page=2", "", memberPrincipal(5))
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &page) != nil || len(page.Results) != 1 || page.Results[0].ID != 101 || page.Results[0].IsLatest {
		t.Fatalf("second page: %d %s", w.Code, w.Body.String())
	}

	w = systemsBoundaryRequest(mux, http.MethodGet, "/api/documents/204/referenced-by/", "", memberPrincipal(5))
	if w.Code != http.StatusNotFound {
		t.Fatalf("hidden anchor: %d %s", w.Code, w.Body.String())
	}
}

func TestDocumentReferencesDisappearOnTargetTrashAndReturnOnRestore(t *testing.T) {
	s, mux := newSystemsBoundaryServer(t)
	seedSystemsBoundary(t, s)
	seedDocumentReferences(t, s)

	if _, err := s.DB.Write.Exec(`UPDATE documents SET trashed_at=10 WHERE id=202`); err != nil {
		t.Fatal(err)
	}
	w := systemsBoundaryRequest(mux, http.MethodGet, "/api/documents/101", "", memberPrincipal(5))
	if w.Code != http.StatusOK {
		t.Fatalf("detail while target trashed: %d %s", w.Code, w.Body.String())
	}
	var detail DocumentDetail
	if err := json.Unmarshal(w.Body.Bytes(), &detail); err != nil {
		t.Fatal(err)
	}
	for _, field := range detail.CustomFields {
		if field.Name == "Related" {
			t.Fatalf("trashed target remained visible: %s", w.Body.String())
		}
	}
	w = systemsBoundaryRequest(mux, http.MethodGet, "/api/documents/202/referenced-by/", "", memberPrincipal(5))
	var backlinks Envelope[DocumentBacklink]
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &backlinks) != nil || backlinks.Count != 0 || len(backlinks.Results) != 0 {
		t.Fatalf("trashed target backlinks: %d %s", w.Code, w.Body.String())
	}

	if _, err := s.DB.Write.Exec(`UPDATE documents SET trashed_at=NULL WHERE id=202`); err != nil {
		t.Fatal(err)
	}
	w = systemsBoundaryRequest(mux, http.MethodGet, "/api/documents/101", "", memberPrincipal(5))
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &detail) != nil {
		t.Fatalf("detail after restore: %d %s", w.Code, w.Body.String())
	}
	found := false
	for _, field := range detail.CustomFields {
		found = found || field.Name == "Related"
	}
	if !found {
		t.Fatalf("restored target did not reappear: %s", w.Body.String())
	}
}

func TestDeletingDocumentLinkFieldRemovesOutgoingValuesAndBacklinks(t *testing.T) {
	s, mux := newSystemsBoundaryServer(t)
	seedSystemsBoundary(t, s)
	seedDocumentReferences(t, s)

	if _, err := s.DB.Write.Exec(`DELETE FROM custom_fields WHERE id=105`); err != nil {
		t.Fatal(err)
	}
	var values int
	if err := s.DB.Read.QueryRow(`SELECT COUNT(*) FROM document_custom_field_values WHERE field_id=105`).Scan(&values); err != nil {
		t.Fatal(err)
	}
	if values != 0 {
		t.Fatalf("deleted field retained %d values", values)
	}
	w := systemsBoundaryRequest(mux, http.MethodGet, "/api/documents/202/referenced-by/", "", memberPrincipal(5))
	var backlinks Envelope[DocumentBacklink]
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &backlinks) != nil || backlinks.Count != 0 {
		t.Fatalf("deleted field backlinks: %d %s", w.Code, w.Body.String())
	}
}

func TestFieldPresenceQueryUsesVisibleLiveReferenceTargets(t *testing.T) {
	s, _ := newSystemsBoundaryServer(t)
	seedSystemsBoundary(t, s)
	seedDocumentReferences(t, s)
	principal := memberPrincipal(5)

	assertIDs := func(label string, got []int64, want ...int64) {
		t.Helper()
		gotSet := make(map[int64]bool, len(got))
		for _, id := range got {
			gotSet[id] = true
		}
		if len(gotSet) != len(want) {
			t.Fatalf("%s ids=%v, want %v", label, got, want)
		}
		for _, id := range want {
			if !gotSet[id] {
				t.Fatalf("%s ids=%v, missing %d", label, got, id)
			}
		}
	}
	list := func(query string) (int, []int64) {
		t.Helper()
		code, rows, count := doList(t, s, "/api/documents/?q="+url.QueryEscape(query), principal)
		ids := make([]int64, 0, len(rows))
		for _, row := range rows {
			ids = append(ids, row.ID)
		}
		if count != len(ids) {
			t.Fatalf("list %q count=%d ids=%v", query, count, ids)
		}
		return code, ids
	}

	code, ids := list(`has-field:"Related" version:all`)
	if code != http.StatusOK {
		t.Fatalf("related list status=%d", code)
	}
	assertIDs("visible exact references", ids, 101, 103)

	searchCode, search, searchErr := doSearch(t, s, `has-field:"Related" version:all`, principal)
	searchIDs := make([]int64, 0, len(search.Results))
	for _, result := range search.Results {
		searchIDs = append(searchIDs, result.ID)
	}
	if searchCode != http.StatusOK {
		t.Fatalf("related search status=%d body=%v", searchCode, searchErr)
	}
	assertIDs("ranked search", searchIDs, 101, 103)

	_, ids = list(`has-field:"Local field" version:all`)
	assertIDs("ordinary value", ids, 101)
	_, ids = list(`has-field:"Secret" version:all`)
	assertIDs("hidden reference target", ids)
	_, ids = list(`-has-field:"Secret" version:all`)
	if !containsDocumentID(ids, 101) {
		t.Fatalf("hidden reference did not behave as absent: ids=%v", ids)
	}

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest("GET", "/api/autocomplete/?q="+url.QueryEscape("has-field:Rel"), nil)
	request = request.WithContext(auth.WithPrincipal(context.Background(), principal))
	s.Autocomplete(recorder, request)
	var suggestions struct {
		Results []AutocompleteSuggestion `json:"results"`
	}
	if recorder.Code != http.StatusOK || json.Unmarshal(recorder.Body.Bytes(), &suggestions) != nil ||
		len(suggestions.Results) != 1 || suggestions.Results[0].Query != "has-field:Related" {
		t.Fatalf("field autocomplete: %d %s", recorder.Code, recorder.Body.String())
	}

	if _, err := s.DB.Write.Exec(`UPDATE documents SET trashed_at=10 WHERE id=202`); err != nil {
		t.Fatal(err)
	}
	_, ids = list(`has-field:"Related" version:all`)
	assertIDs("trashed reference target", ids)
	if _, err := s.DB.Write.Exec(`UPDATE documents SET trashed_at=NULL WHERE id=202`); err != nil {
		t.Fatal(err)
	}
	_, ids = list(`has-field:"Related" version:all`)
	assertIDs("restored reference target", ids, 101, 103)

	if _, err := s.DB.Write.Exec(`UPDATE custom_fields SET name='Relationship' WHERE id=105`); err != nil {
		t.Fatal(err)
	}
	code, _ = list(`has-field:"Related" version:all`)
	if code != http.StatusBadRequest {
		t.Fatalf("old field name status=%d, want 400", code)
	}
	_, ids = list(`has-field:"Relationship" version:all`)
	assertIDs("renamed field", ids, 101, 103)
	if _, err := s.DB.Write.Exec(`DELETE FROM custom_fields WHERE id=105`); err != nil {
		t.Fatal(err)
	}
	code, _ = list(`has-field:"Relationship" version:all`)
	if code != http.StatusBadRequest {
		t.Fatalf("deleted field status=%d, want 400", code)
	}
}

func containsDocumentID(ids []int64, want int64) bool {
	for _, id := range ids {
		if id == want {
			return true
		}
	}
	return false
}
