package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/johnnybravo-xyz/suchi/core/auth"
	"github.com/johnnybravo-xyz/suchi/core/blob"
	"github.com/johnnybravo-xyz/suchi/core/db"
	migrations "github.com/johnnybravo-xyz/suchi/core/db/migrations"
	"github.com/johnnybravo-xyz/suchi/core/jd/importer"
	pluginapi "github.com/johnnybravo-xyz/suchi/plugin-api"
)

func newSystemsBoundaryServer(t *testing.T) (*Server, *http.ServeMux) {
	t.Helper()
	d := openTestDB(t)
	cas, err := blob.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s := newUploadTestServer(t, d, cas)
	for _, id := range []int64{1, 5, 6} {
		seedUser(t, d, id)
	}
	if _, err := d.Write.Exec(`UPDATE users SET role='member', capabilities='["archive_chat","archive_intelligence","share_links","mailboxes"]' WHERE id IN (5,6)`); err != nil {
		t.Fatal(err)
	}
	s.PublicURL = "https://archive.example"
	mux := http.NewServeMux()
	s.Register(mux)
	s.AttachDecrypt(mux, DecryptDeps{CAS: cas})
	return s, mux
}

func systemsBoundaryRequest(mux *http.ServeMux, method, path, body string, p *pluginapi.Principal) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Accept", "application/json")
	if p != nil {
		r = r.WithContext(auth.WithPrincipal(r.Context(), p))
	}
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	return w
}

func seedSystemsBoundary(t *testing.T, s *Server) {
	t.Helper()
	if _, err := s.DB.Write.Exec(`
		UPDATE jd_systems SET code='S01', name='First' WHERE id=1;
		INSERT INTO jd_systems(id,code,name,taxonomy,created_at,updated_at) VALUES (2,'S02','Foreign cabinet','jd',0,0);
		INSERT INTO jd_system_members(system_id,user_id,created_at) VALUES (2,6,0);
		INSERT INTO jd_areas(system_id,code_start,code_end,name,position) VALUES (1,10,19,'First',0),(2,10,19,'Foreign area',0);
		INSERT INTO jd_categories(id,system_id,area_start,code,name,system) VALUES (101,1,10,13,'First filing',1),(111,1,10,14,'Refiled',0),(201,2,10,13,'Foreign filing',1);
		UPDATE jd_systems SET inbox_category_id=CASE id WHEN 1 THEN 101 ELSE 201 END;
		INSERT INTO tags(id,system_id,name,slug,created_at,updated_at) VALUES (101,1,'Needle local tag','needle',0,0),(201,2,'Needle foreign tag','needle',0,0);
		INSERT INTO correspondents(id,system_id,name,slug,created_at,updated_at) VALUES (101,1,'Needle local correspondent','needle',0,0),(201,2,'Needle foreign correspondent','needle',0,0);
		INSERT INTO document_types(id,system_id,name,slug,created_at,updated_at) VALUES (101,1,'Needle local type','needle',0,0),(201,2,'Needle foreign type','needle',0,0);
		INSERT INTO storage_paths(id,system_id,name,slug,path,created_at,updated_at) VALUES (101,1,'Local path','path','local',0,0),(201,2,'Foreign path','path','foreign',0,0);
		INSERT INTO custom_fields(id,system_id,name,data_type,created_at,updated_at) VALUES (101,1,'Local field','text',0,0),(201,2,'Foreign field','text',0,0);
		INSERT INTO groups(id,name,created_at,updated_at) VALUES (51,'Reviewers',0,0);
		INSERT INTO group_members(group_id,user_id,created_at) VALUES (51,5,0);
	`); err != nil {
		t.Fatal(err)
	}
	for _, row := range []struct {
		id, system, owner, category int64
		title                       string
		trashed                     bool
	}{
		{101, 1, 5, 101, "Needle local electricity", false},
		{102, 1, 6, 101, "Needle private electricity", false},
		{103, 1, 5, 101, "Needle local utility", false},
		{104, 1, 5, 101, "Local discarded", true},
		{201, 2, 5, 201, "Needle foreign owner", false},
		{202, 2, 6, 201, "Needle foreign direct", false},
		{203, 2, 6, 201, "Needle foreign group", false},
		{204, 2, 6, 201, "Needle foreign assignment", false},
		{205, 2, 5, 201, "Foreign discarded", true},
	} {
		content := "needle electricity utility consumption residential invoice " + row.title
		ref, err := s.CAS.Put(strings.NewReader(content))
		if err != nil {
			t.Fatal(err)
		}
		var trashed any
		if row.trashed {
			trashed = int64(1)
		}
		if _, err := s.DB.Write.Exec(`INSERT INTO documents(id,system_id,owner_id,jd_category_id,title,content,original_blob,original_size,mime_type,sensitivity,trashed_at,created_at,updated_at) VALUES (?,?,?,?,?,?,?,?, 'text/plain','public',?,0,0)`, row.id, row.system, row.owner, row.category, row.title, content, ref.SHA256, len(content), trashed); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.DB.Write.Exec(`
		INSERT INTO object_acls(object_kind,object_id,principal_kind,principal_id,perm_bits,created_at) VALUES ('document',202,'user',5,7,0),('document',203,'group',51,7,0);
		UPDATE documents SET encryption_state='encrypted' WHERE id IN (101,201,202,203,204);
		INSERT INTO approval_defs(id,system_id,slug,version,spec_json,active,created_at) VALUES (201,2,'review',1,'{}',1,0);
		INSERT INTO approval_runs(id,system_id,def_id,doc_id,state,current_state,vars_json,state_entered_at,started_at) VALUES (201,2,201,204,'running','review','{}',0,0);
		INSERT INTO approval_tasks(id,run_id,state_key,assignee,prompt,choices_json,status,created_at) VALUES (201,201,'review','user:5','Foreign approval','["approve"]','open',0);
		INSERT INTO audit_events(id,system_id,ts,actor_kind,actor_id,action,object_kind,object_id) VALUES (101,1,1,'user',5,'document.update','document',101),(201,2,2,'user',5,'document.update','document',201),(202,2,3,'user',5,'document.update','document',202),(203,2,4,'user',5,'document.update','document',203);
	`); err != nil {
		t.Fatal(err)
	}
}

func TestSystemsSetupUsesSelectedTreeWithoutBorrowingDefaultChoice(t *testing.T) {
	s, mux := newSystemsBoundaryServer(t)
	if _, err := s.DB.Write.Exec(`
		UPDATE jd_systems SET code='S01', preset_id='first' WHERE id=1;
		INSERT INTO jd_systems(id,code,name,taxonomy,preset_id,created_at,updated_at)
		VALUES (2,'S02','Second','jd','second',0,0);
	`); err != nil {
		t.Fatal(err)
	}
	for code, preset := range map[string]string{"S01": "first", "S02": "second"} {
		w := systemsBoundaryRequest(mux, "GET", "/api/admin/setup/state?system="+code, "", adminPrincipal(1))
		var state struct {
			CurrentPreset    string `json:"current_preset"`
			FilingTreeChosen bool   `json:"filing_tree_chosen"`
		}
		if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &state) != nil ||
			state.CurrentPreset != preset || !state.FilingTreeChosen {
			t.Fatalf("%s setup state: %d %s", code, w.Code, w.Body.String())
		}
	}
	if _, err := s.DB.Write.Exec(`UPDATE jd_systems SET preset_id=NULL WHERE id=2`); err != nil {
		t.Fatal(err)
	}
	w := systemsBoundaryRequest(mux, "GET", "/api/admin/setup/state?system=S02", "", adminPrincipal(1))
	var state struct {
		FilingTreeChosen bool `json:"filing_tree_chosen"`
	}
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &state) != nil || state.FilingTreeChosen {
		t.Fatalf("unconfigured S02 borrowed S01's choice: %d %s", w.Code, w.Body.String())
	}
	w = systemsBoundaryRequest(mux, "POST", "/api/admin/setup/complete?system=S02", "", adminPrincipal(1))
	if w.Code != http.StatusConflict {
		t.Fatalf("completed unconfigured S02: %d %s", w.Code, w.Body.String())
	}
}

func TestSystemsBuiltinPresetAppliesOnlyToSelectedSystem(t *testing.T) {
	s, mux := newSystemsBoundaryServer(t)
	if _, err := s.DB.Write.Exec(`
		UPDATE jd_systems SET code='S01', preset_id='first' WHERE id=1;
		INSERT INTO jd_systems(id,code,name,taxonomy,created_at,updated_at)
		VALUES (2,'S02','Second','jd',0,0);
		INSERT INTO jd_areas(system_id,code_start,code_end,name,position)
		VALUES (1,40,49,'System',0),(2,40,49,'System',0);
		INSERT INTO jd_categories(id,system_id,area_start,code,name,system)
		VALUES (101,1,40,49,'Inbox',1),(201,2,40,49,'Inbox',1);
		UPDATE jd_systems SET inbox_category_id=CASE id WHEN 1 THEN 101 ELSE 201 END;
	`); err != nil {
		t.Fatal(err)
	}
	w := systemsBoundaryRequest(mux, "POST", "/api/admin/setup/preset?system=S02",
		`{"preset_id":"household","include_seeds":false,"refile":true}`, adminPrincipal(1))
	if w.Code != http.StatusOK {
		t.Fatalf("preset apply: %d %s", w.Code, w.Body.String())
	}
	for code, preset := range map[string]string{"S01": "first", "S02": "household"} {
		w = systemsBoundaryRequest(mux, "GET", "/api/admin/setup/state?system="+code, "", adminPrincipal(1))
		var state struct {
			CurrentPreset string `json:"current_preset"`
		}
		if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &state) != nil || state.CurrentPreset != preset {
			t.Fatalf("%s after preset apply: %d %s", code, w.Code, w.Body.String())
		}
	}
}

func TestSystemsDiscoveryTracksImportAndNeverFallsBackToForeignDefault(t *testing.T) {
	_, mux := newSystemsBoundaryServer(t)
	var discovery struct {
		Introduced bool              `json:"introduced"`
		Default    string            `json:"default_system_code"`
		Results    []filingSystemRow `json:"results"`
	}
	w := systemsBoundaryRequest(mux, "GET", "/api/jd/systems", "", memberPrincipal(5))
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &discovery) != nil || discovery.Introduced || discovery.Default != "" || len(discovery.Results) != 0 {
		t.Fatalf("unnamed discovery: %d %s", w.Code, w.Body.String())
	}
	for _, code := range []string{"S01", "S02"} {
		input := TaxonomyImportReq{Content: replacementTaxonomy + "system = \"" + code + "\"\n", Format: "toml"}
		body, err := json.Marshal(input)
		if err != nil {
			t.Fatal(err)
		}
		w = systemsBoundaryRequest(mux, "POST", "/api/admin/taxonomy/import", string(body), adminPrincipal(1))
		var preview importer.Diff
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &preview) != nil {
			t.Fatalf("preview %s: %d %s", code, w.Code, w.Body.String())
		}
		input.Apply, input.ExpectedStateHash = true, preview.StateHash
		body, err = json.Marshal(input)
		if err != nil {
			t.Fatal(err)
		}
		w = systemsBoundaryRequest(mux, "POST", "/api/admin/taxonomy/import", string(body), adminPrincipal(1))
		if w.Code != 200 {
			t.Fatalf("apply %s: %d %s", code, w.Code, w.Body.String())
		}
	}
	w = systemsBoundaryRequest(mux, "GET", "/api/jd/systems", "", memberPrincipal(5))
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &discovery) != nil || !discovery.Introduced || discovery.Default != "S01" || !reflect.DeepEqual(discovery.Results, []filingSystemRow{{Code: "S01", Name: "Replacement", IsDefault: true}}) {
		t.Fatalf("new cabinet leaked membership: %d %s", w.Code, w.Body.String())
	}
	w = systemsBoundaryRequest(mux, "GET", "/api/documents/?system=S02", "", memberPrincipal(5))
	if w.Code != 404 {
		t.Fatalf("new system admitted member: %d %s", w.Code, w.Body.String())
	}
	for code, body := range map[string]string{"S01": `{"user_ids":[6]}`, "S02": `{"user_ids":[5]}`} {
		w = systemsBoundaryRequest(mux, "PUT", "/api/admin/jd/systems/"+code+"/members", body, adminPrincipal(1))
		if w.Code != 200 {
			t.Fatalf("membership: %d %s", w.Code, w.Body.String())
		}
	}
	w = systemsBoundaryRequest(mux, "GET", "/api/jd/systems", "", memberPrincipal(5))
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &discovery) != nil || discovery.Default != "" || !reflect.DeepEqual(discovery.Results, []filingSystemRow{{Code: "S02", Name: "Replacement"}}) {
		t.Fatalf("S02-only discovery: %d %s", w.Code, w.Body.String())
	}
	for _, path := range []string{"/api/documents/", "/api/documents/?system=S01"} {
		w = systemsBoundaryRequest(mux, "GET", path, "", memberPrincipal(5))
		if w.Code != 404 {
			t.Fatalf("implicit fallback %s: %d %s", path, w.Code, w.Body.String())
		}
	}
	w = systemsBoundaryRequest(mux, "GET", "/api/documents/?system=S02", "", memberPrincipal(5))
	var docs Envelope[DocumentListRow]
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &docs) != nil || docs.Count != 0 || len(docs.Results) != 0 {
		t.Fatalf("explicit S02: %d %s", w.Code, w.Body.String())
	}
}

func TestSystemsCollectionsFilterBeforeCountsPagesAndHydration(t *testing.T) {
	s, mux := newSystemsBoundaryServer(t)
	seedSystemsBoundary(t, s)
	for _, id := range []int64{101, 201, 202, 203, 204} {
		seedDateIntelligence(t, s, id, "pending", "2027-09-01")
	}
	p := memberPrincipal(5)
	for _, tc := range []struct {
		path string
		ids  []int64
	}{
		{"/api/documents/?system=S01&ordering=created_at&page=2&page_size=1", []int64{103}},
		{"/api/search/?system=S01&q=needle&recency=off&page_size=25", []int64{101, 103}},
	} {
		w := systemsBoundaryRequest(mux, "GET", tc.path, "", p)
		var out struct {
			Count   int `json:"count"`
			Results []struct {
				ID      int64  `json:"id"`
				Snippet string `json:"snippet"`
			} `json:"results"`
		}
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &out) != nil || out.Count != 2 {
			t.Fatalf("scoped page %s: %d %s", tc.path, w.Code, w.Body.String())
		}
		ids := make([]int64, 0, len(out.Results))
		for _, row := range out.Results {
			ids = append(ids, row.ID)
		}
		sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
		if !reflect.DeepEqual(ids, tc.ids) {
			t.Fatalf("scoped identities %s: got=%v want=%v", tc.path, ids, tc.ids)
		}
		if strings.Contains(strings.ToLower(w.Body.String()), "foreign") || strings.Contains(w.Body.String(), "private electricity") {
			t.Fatalf("hidden snippet: %s", w.Body.String())
		}
	}
	w := systemsBoundaryRequest(mux, "GET", "/api/stats/?system=S01", "", p)
	var stats StatsResponse
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &stats) != nil || stats.DocumentsTotal != 2 || stats.TrashCount != 1 || stats.InboxCount != 2 || stats.PendingApprovals != 0 || stats.PendingIntelligence != 1 {
		t.Fatalf("scoped counters: %d %s", w.Code, w.Body.String())
	}
	for _, tc := range []struct {
		path  string
		id    int64
		count int
	}{
		{"/api/trash/?system=S01", 104, 1},
		{"/api/documents/pending-decryption?system=S01", 101, 0},
		{"/api/intelligence/?system=S01&type=date&status=pending", 101, 1},
		{"/api/tags/?system=S01", 101, 1},
		{"/api/correspondents/?system=S01", 101, 1},
		{"/api/document_types/?system=S01", 101, 1},
		{"/api/storage_paths/?system=S01", 101, 1},
		{"/api/custom_fields/?system=S01", 101, 1},
	} {
		w = systemsBoundaryRequest(mux, "GET", tc.path, "", p)
		var out struct {
			Count   int `json:"count"`
			Results []struct {
				ID         int64 `json:"id"`
				DocumentID int64 `json:"document_id"`
			} `json:"results"`
		}
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &out) != nil || len(out.Results) != 1 || (out.Results[0].ID != tc.id && out.Results[0].DocumentID != tc.id) || out.Count != tc.count || strings.Contains(strings.ToLower(w.Body.String()), "foreign") {
			t.Fatalf("projection %s: %d %s", tc.path, w.Code, w.Body.String())
		}
	}
	w = systemsBoundaryRequest(mux, "GET", "/api/autocomplete/?system=S01&q=Needle", "", p)
	var suggestions struct {
		Results []AutocompleteSuggestion `json:"results"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &suggestions) != nil || !reflect.DeepEqual(suggestions.Results, []AutocompleteSuggestion{{ID: 101, Kind: "tag", Value: "Needle local tag"}, {ID: 101, Kind: "correspondent", Value: "Needle local correspondent"}, {ID: 101, Kind: "document_type", Value: "Needle local type"}}) {
		t.Fatalf("autocomplete: %d %s", w.Code, w.Body.String())
	}
	w = systemsBoundaryRequest(mux, "GET", "/api/events/?system=S01&limit=1", "", p)
	var events EventsResponse
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &events) != nil || events.LatestID != 101 || len(events.Results) != 1 || events.Results[0].ID != 101 {
		t.Fatalf("foreign cursor/labels: %d %s", w.Code, w.Body.String())
	}
	for _, path := range []string{"/api/documents/", "/api/search/?q=needle", "/api/stats/", "/api/trash/", "/api/documents/pending-decryption", "/api/intelligence/?type=date", "/api/events/", "/api/autocomplete/?q=Needle", "/api/share_links/"} {
		sep := "?"
		if strings.Contains(path, "?") {
			sep = "&"
		}
		w = systemsBoundaryRequest(mux, "GET", path+sep+"system=S02", "", p)
		if w.Code != 404 || strings.Contains(strings.ToLower(w.Body.String()), "foreign") {
			t.Fatalf("foreign collection %s: %d %s", path, w.Code, w.Body.String())
		}
	}
}

func TestSystemsNumericAndAddressAccessRequireBothEntryAndACL(t *testing.T) {
	s, mux := newSystemsBoundaryServer(t)
	seedSystemsBoundary(t, s)
	for _, id := range []int64{201, 202, 203, 204} {
		w := systemsBoundaryRequest(mux, "GET", fmt.Sprintf("/api/documents/%d", id), "", memberPrincipal(5))
		if w.Code != 404 {
			t.Fatalf("owner/grant/assignment admitted %d: %d %s", id, w.Code, w.Body.String())
		}
	}
	if _, err := s.DB.Write.Exec(`INSERT INTO jd_system_members(system_id,user_id,created_at) VALUES (2,5,0)`); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		address string
		p       *pluginapi.Principal
		status  int
	}{
		{"S02.13.201", memberPrincipal(5), 200},
		{"S02.13.202", memberPrincipal(5), 200},
		{"S02.13.203", memberPrincipal(5), 200},
		{"S02.13.204", memberPrincipal(5), 404},
		{"S01.13.102", memberPrincipal(5), 404},
		{"S01.13.201", adminPrincipal(1), 404},
		{"S01.14.101", adminPrincipal(1), 404},
		{"S01.13.999999", adminPrincipal(1), 404},
		{"S01.13.0101", adminPrincipal(1), 400},
		{"s01.13.101", adminPrincipal(1), 400},
		{"S01.13.9223372036854775808", adminPrincipal(1), 400},
	} {
		w := systemsBoundaryRequest(mux, "GET", "/api/jd/resolve?address="+url.QueryEscape(tc.address), "", tc.p)
		if w.Code != tc.status {
			t.Fatalf("address %s: %d %s", tc.address, w.Code, w.Body.String())
		}
		if tc.status == 200 {
			var out struct {
				ID      int64  `json:"id"`
				Address string `json:"jd_address"`
			}
			if json.Unmarshal(w.Body.Bytes(), &out) != nil || out.Address != tc.address || out.ID < 201 || out.ID > 203 {
				t.Fatalf("wrong address identity: %s", w.Body.String())
			}
		}
	}
	adminToken := adminPrincipal(1)
	adminToken.Kind = "token"
	adminToken.TokenSystemID = 1
	adminToken.Scopes = []string{auth.ScopeDocumentsRead}
	for _, tc := range []struct {
		path string
		p    *pluginapi.Principal
	}{
		{"/api/documents/201?system=S01", adminPrincipal(1)},
		{"/api/jd/resolve?address=S02.13.201&system=S01", adminPrincipal(1)},
		{"/api/documents/201", adminToken},
		{"/api/jd/resolve?address=S02.13.201", adminToken},
	} {
		w := systemsBoundaryRequest(mux, "GET", tc.path, "", tc.p)
		if w.Code != 404 {
			t.Fatalf("admin mismatch %s: %d %s", tc.path, w.Code, w.Body.String())
		}
	}
	w := systemsBoundaryRequest(mux, "PATCH", "/api/documents/101", `{"jd_category_id":111}`, memberPrincipal(5))
	if w.Code != 200 {
		t.Fatalf("refile: %d %s", w.Code, w.Body.String())
	}
	for _, tc := range []struct {
		path   string
		status int
	}{
		{"/api/jd/resolve?address=S01.13.101", 404},
		{"/api/jd/resolve?address=S01.14.101", 200},
		{"/api/documents/101", 200},
	} {
		w = systemsBoundaryRequest(mux, "GET", tc.path, "", memberPrincipal(5))
		if w.Code != tc.status {
			t.Fatalf("refiled identity %s: %d %s", tc.path, w.Code, w.Body.String())
		}
		if tc.status == 200 {
			var out struct {
				ID      int64  `json:"id"`
				Address string `json:"jd_address"`
			}
			if json.Unmarshal(w.Body.Bytes(), &out) != nil || out.ID != 101 || out.Address != "S01.14.101" {
				t.Fatalf("refile renumbered: %s", w.Body.String())
			}
		}
	}
}

func TestSystemsSimilarityAndChatExcludeForeignEvidenceAndPriorSources(t *testing.T) {
	s, mux := newSystemsBoundaryServer(t)
	seedSystemsBoundary(t, s)
	// Unrelated documents make the electricity terms discriminative for FTS scoring.
	for i := range 15 {
		if _, err := s.DB.Write.Exec(`INSERT INTO documents(system_id,owner_id,jd_category_id,title,content,original_blob,original_size,created_at,updated_at) VALUES (1,6,101,'Recipe','pasta tomato basil garlic simmer',?,1,0,0)`, fmt.Sprintf("recipe-%d", i)); err != nil {
			t.Fatal(err)
		}
	}
	w := systemsBoundaryRequest(mux, "GET", "/api/documents/101/similar", "", memberPrincipal(5))
	var similar SimilarResponse
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &similar) != nil || len(similar.Results) != 1 || similar.Results[0].ID != 103 {
		t.Fatalf("similar candidates: %d %s", w.Code, w.Body.String())
	}
	for _, id := range []int64{201, 202, 203, 204} {
		w = systemsBoundaryRequest(mux, "GET", fmt.Sprintf("/api/documents/%d/similar", id), "", memberPrincipal(5))
		if w.Code != 404 {
			t.Fatalf("foreign source %d: %d %s", id, w.Code, w.Body.String())
		}
	}
	seedDateIntelligence(t, s, 101, "accepted", "2027-09-01")
	for _, id := range []int64{201, 202, 203, 204} {
		seedDateIntelligence(t, s, id, "accepted", "2039-12-31")
		seedDateIntelligence(t, s, id, "pending", "2040-12-31")
	}
	s.ChatEnabled = func() bool { return true }
	calls := 0
	s.ChatCompletion = func(_ context.Context, _ string, messages []ChatCompletionMessage, _ int) (string, error) {
		calls++
		for _, message := range messages {
			if strings.Contains(strings.ToLower(message.Content), "foreign") || strings.Contains(message.Content, "2039-12-31") || strings.Contains(message.Content, "2040-12-31") || strings.Contains(message.Content, "private electricity") {
				t.Errorf("hidden evidence sent to provider: %s", message.Content)
			}
		}
		return `{"answer":"Electricity evidence [1].","citations":[1],"sufficient":true}`, nil
	}
	// Prior sources cover an owned document, a hidden same-system document,
	// and a foreign document within the three-source request limit.
	w = systemsBoundaryRequest(mux, "POST", "/api/chat?system=S01", `{"question":"needle electricity","context_source_ids":[101,102,201],"scope":{"document_ids":[101,102,103,201,202,203,204]}}`, memberPrincipal(5))
	var chat ChatResponse
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &chat) != nil {
		t.Fatalf("chat: %d %s", w.Code, w.Body.String())
	}
	ids := make([]int64, 0, len(chat.Sources))
	for _, source := range chat.Sources {
		ids = append(ids, source.ID)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	if calls != 1 || !reflect.DeepEqual(ids, []int64{101, 103}) || chat.Intelligence.Accepted["date"] != 1 || chat.Intelligence.Pending["date"] != 0 || strings.Contains(strings.ToLower(w.Body.String()), "foreign") {
		t.Fatalf("chat scope/evidence: calls=%d body=%s", calls, w.Body.String())
	}
}

func TestSystemsForeignMetadataRejectsWithoutPartialWritesOrOutbox(t *testing.T) {
	for _, tc := range []struct {
		method, path, body string
		status             int
	}{
		{"POST", "/api/documents/bulk_edit?system=S01", `{"documents":[101,103],"method":"set_jd_category","parameters":{"jd_category_id":201}}`, 400},
		{"POST", "/api/documents/bulk_edit?system=S01", `{"documents":[101,103],"method":"set_correspondent","parameters":{"correspondent_id":201}}`, 400},
		{"POST", "/api/documents/bulk_edit?system=S01", `{"documents":[101,103],"method":"set_document_type","parameters":{"document_type_id":201}}`, 400},
		{"POST", "/api/documents/bulk_edit?system=S01", `{"documents":[101,103],"method":"set_storage_path","parameters":{"storage_path_id":201}}`, 400},
		{"POST", "/api/documents/bulk_edit?system=S01", `{"documents":[101,103],"method":"add_tag","parameters":{"tag_id":201}}`, 400},
		{"POST", "/api/documents/201/correspondents/?system=S01", `{"name":"Must not be created","role":"sender"}`, 404},
		{"PATCH", "/api/correspondents/201?system=S01", `{"name":"Must not change"}`, 404},
		{"PATCH", "/api/custom_fields/201?system=S01", `{"name":"Must not change"}`, 404},
		{"PUT", "/api/documents/101/custom_fields/201?system=S01", `{"value":"Must not be written"}`, 404},
	} {
		t.Run(tc.method+" "+tc.path+" "+tc.body, func(t *testing.T) {
			s, mux := newSystemsBoundaryServer(t)
			seedSystemsBoundary(t, s)
			w := systemsBoundaryRequest(mux, tc.method, tc.path, tc.body, adminPrincipal(1))
			if w.Code != tc.status {
				t.Fatalf("rejection: %d %s", w.Code, w.Body.String())
			}
			var unchanged int
			if err := s.DB.Read.QueryRow(`SELECT COUNT(*) FROM documents WHERE id IN (101,103) AND jd_category_id=101 AND correspondent_id IS NULL AND document_type_id IS NULL AND storage_path_id IS NULL AND updated_at=0`).Scan(&unchanged); err != nil || unchanged != 2 {
				t.Fatalf("partial document mutation: count=%d err=%v", unchanged, err)
			}
			for _, table := range []string{"document_tags", "document_correspondents", "document_custom_field_values", "jobs", "render_moves"} {
				var count int
				if err := s.DB.Read.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&count); err != nil || count != 0 {
					t.Fatalf("rejected write changed %s: %d %v", table, count, err)
				}
			}
			var foreignName, fieldName string
			if err := s.DB.Read.QueryRow(`SELECT name FROM correspondents WHERE id=201`).Scan(&foreignName); err != nil {
				t.Fatal(err)
			}
			if err := s.DB.Read.QueryRow(`SELECT name FROM custom_fields WHERE id=201`).Scan(&fieldName); err != nil {
				t.Fatal(err)
			}
			if foreignName != "Needle foreign correspondent" || fieldName != "Foreign field" {
				t.Fatalf("foreign definitions mutated: %q %q", foreignName, fieldName)
			}
			var count int
			if err := s.DB.Read.QueryRow(`SELECT COUNT(*) FROM audit_events`).Scan(&count); err != nil || count != 4 {
				t.Fatalf("rejected operation emitted audit: %d %v", count, err)
			}
		})
	}
}

func TestSystemsCorrespondentNameUpsertUsesIntrinsicDocumentSystem(t *testing.T) {
	s, mux := newSystemsBoundaryServer(t)
	seedSystemsBoundary(t, s)
	w := systemsBoundaryRequest(mux, "POST", "/api/documents/101/correspondents/", `{"name":"Needle foreign correspondent","role":"sender"}`, memberPrincipal(5))
	var correspondent DocCorrespondent
	if w.Code != 201 || json.Unmarshal(w.Body.Bytes(), &correspondent) != nil || correspondent.ID == 201 || correspondent.Name != "Needle foreign correspondent" {
		t.Fatalf("scoped upsert: %d %s", w.Code, w.Body.String())
	}
	var system, primary int64
	if err := s.DB.Read.QueryRow(`SELECT c.system_id,d.correspondent_id FROM correspondents c JOIN documents d ON d.correspondent_id=c.id WHERE d.id=101`).Scan(&system, &primary); err != nil || system != 1 || primary != correspondent.ID {
		t.Fatalf("foreign upsert relation: system=%d primary=%d err=%v", system, primary, err)
	}
	w = systemsBoundaryRequest(mux, "GET", "/api/documents/101/correspondents/", "", memberPrincipal(5))
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"id":`+strconv.FormatInt(correspondent.ID, 10)) {
		t.Fatalf("persisted correspondent: %d %s", w.Code, w.Body.String())
	}
}

func TestSystemsUploadDedupeAndIdempotencyHaveDifferentNamespaceBoundaries(t *testing.T) {
	s, mux := newSystemsBoundaryServer(t)
	seedSystemsBoundary(t, s)
	if _, err := s.DB.Write.Exec(`INSERT INTO jd_system_members(system_id,user_id,created_at) VALUES (2,5,0)`); err != nil {
		t.Fatal(err)
	}
	payload := []byte("GSTR-3B\nSharma Traders\nJuly 2026\n")
	var first, second UploadResponse
	for _, tc := range []struct {
		code, key string
		status    int
		out       *UploadResponse
	}{
		{"S01", testIdempotencyKey, 201, &first},
		{"S02", testIdempotencyKey, 409, nil},
		{"S02", secondIdempotencyKey, 201, &second},
	} {
		r := multipartUploadRequest(t, "/api/documents/?system="+tc.code, "return.txt", payload, nil, memberPrincipal(5))
		r.Header.Set("Idempotency-Key", tc.key)
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, r)
		if w.Code != tc.status {
			t.Fatalf("upload %s: %d %s", tc.code, w.Code, w.Body.String())
		}
		if tc.out != nil {
			if err := json.Unmarshal(w.Body.Bytes(), tc.out); err != nil {
				t.Fatal(err)
			}
		} else if !strings.Contains(w.Body.String(), `"code":"idempotency_conflict"`) {
			t.Fatalf("wrong collision: %s", w.Body.String())
		}
	}
	if first.ID == second.ID {
		t.Fatalf("cross-system dedupe returned the same ID: %d", first.ID)
	}
	var firstHash, secondHash string
	var firstSystem, secondSystem int64
	if err := s.DB.Read.QueryRow(`SELECT system_id,original_blob FROM documents WHERE id=?`, first.ID).Scan(&firstSystem, &firstHash); err != nil {
		t.Fatal(err)
	}
	if err := s.DB.Read.QueryRow(`SELECT system_id,original_blob FROM documents WHERE id=?`, second.ID).Scan(&secondSystem, &secondHash); err != nil {
		t.Fatal(err)
	}
	if firstSystem != 1 || secondSystem != 2 || firstHash != secondHash {
		t.Fatalf("dedupe/CAS scope: systems=%d,%d hashes=%s,%s", firstSystem, secondSystem, firstHash, secondHash)
	}
	stored, err := s.CAS.Get(firstHash)
	if err != nil {
		t.Fatal(err)
	}
	actual, err := io.ReadAll(stored)
	stored.Close()
	if err != nil || string(actual) != string(payload) {
		t.Fatalf("shared immutable bytes: %q %v", actual, err)
	}
	for _, tc := range []struct {
		code, key string
		id        int64
	}{{"S01", testIdempotencyKey, first.ID}, {"S02", secondIdempotencyKey, second.ID}} {
		for _, key := range []string{tc.key, ""} {
			r := multipartUploadRequest(t, "/api/documents/?system="+tc.code, "return.txt", payload, nil, memberPrincipal(5))
			if key != "" {
				r.Header.Set("Idempotency-Key", key)
			}
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, r)
			var replay UploadResponse
			wantStatus := 200
			if key != "" {
				wantStatus = 201
			}
			if w.Code != wantStatus || json.Unmarshal(w.Body.Bytes(), &replay) != nil || replay.ID != tc.id || replay.IdempotentReplay != (key != "") || replay.Deduplicated != (key == "") {
				t.Fatalf("retry %s key=%q: %d %s", tc.code, key, w.Code, w.Body.String())
			}
		}
	}
	assertUploadSideEffectCounts(t, s.DB, 11, 2, 2, 2)
}

func TestSystemsRestartReplaysPersistedDocumentAndVersionReceipts(t *testing.T) {
	ctx := t.Context()
	root := t.TempDir()
	databasePath := filepath.Join(root, "receipts.db")
	d, err := db.Open(ctx, databasePath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	migs, err := db.LoadMigrations(migrations.FS, ".")
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	if err := db.Migrate(ctx, d, migs, log); err != nil {
		t.Fatal(err)
	}
	if _, err := d.ExecWrite(ctx, `
		INSERT INTO users(id,email,display_name,role,created_at,updated_at)
			VALUES(5,'receipts@example.com','Receipts','member',0,0);
		INSERT INTO jd_areas(system_id,code_start,code_end,name,position)
			VALUES(1,40,49,'System',0);
		INSERT INTO jd_categories(system_id,id,area_start,code,name,system)
			VALUES(1,101,40,49,'Inbox',1);
		UPDATE jd_systems SET code='S01',name='Receipts',inbox_category_id=101 WHERE id=1;
	`); err != nil {
		t.Fatal(err)
	}
	cas, err := blob.New(root)
	if err != nil {
		t.Fatal(err)
	}
	s := newUploadTestServer(t, d, cas)
	mux := http.NewServeMux()
	s.Register(mux)
	requests := []struct {
		path, key, payload string
		response           uploadVersionResponse
		location           string
	}{
		{path: "/api/documents/?system=S01", key: testIdempotencyKey, payload: "persisted first receipt\n"},
		{key: secondIdempotencyKey, payload: "persisted version receipt\n"},
	}
	for i := range requests {
		request := &requests[i]
		if i == 1 {
			request.path = fmt.Sprintf("/api/documents/%d/versions/?system=S01", requests[0].response.ID)
		}
		r := multipartUploadRequest(t, request.path, "receipt.txt", []byte(request.payload), nil, memberPrincipal(5))
		r.Header.Set("Idempotency-Key", request.key)
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, r)
		if w.Code != http.StatusCreated || json.Unmarshal(w.Body.Bytes(), &request.response) != nil || request.response.ID <= 0 || request.response.IdempotentReplay || request.response.SystemCode != "S01" {
			t.Fatalf("initial upload %s: %d %s", request.path, w.Code, w.Body.String())
		}
		if request.response.JDAddress != fmt.Sprintf("S01.49.%d", request.response.ID) {
			t.Fatalf("receipt has wrong filing address: %+v", request.response)
		}
		request.location = w.Header().Get("Location")
	}
	if requests[1].response.PreviousVersionID != requests[0].response.ID || requests[1].response.ID == requests[0].response.ID {
		t.Fatalf("version receipt lost predecessor identity: %+v", requests)
	}
	assertUploadSideEffectCounts(t, d, 2, 2, 2, 2)
	// Replay must retain the persisted receipt even after current metadata changes.
	if _, err := d.ExecWrite(ctx, `UPDATE documents SET title='Edited after upload'`); err != nil {
		t.Fatal(err)
	}
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := db.Open(ctx, databasePath)
	if err != nil {
		t.Fatal(err)
	}
	d = reopened
	if err := db.Migrate(ctx, d, migs, log); err != nil {
		t.Fatal(err)
	}
	cas, err = blob.New(root)
	if err != nil {
		t.Fatal(err)
	}
	s = newUploadTestServer(t, d, cas)
	mux = http.NewServeMux()
	s.Register(mux)
	for _, request := range requests {
		for _, path := range []string{request.path, strings.TrimSuffix(request.path, "?system=S01")} {
			r := multipartUploadRequest(t, path, "receipt.txt", []byte(request.payload), nil, memberPrincipal(5))
			r.Header.Set("Idempotency-Key", request.key)
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, r)
			var receipt uploadVersionResponse
			want := request.response
			want.IdempotentReplay = true
			if w.Code != http.StatusCreated || json.Unmarshal(w.Body.Bytes(), &receipt) != nil || receipt != want || w.Header().Get("Location") != request.location {
				t.Fatalf("persisted replay %s: %d %s; want %+v", path, w.Code, w.Body.String(), want)
			}
		}
	}
	assertUploadSideEffectCounts(t, d, 2, 2, 2, 2)
	var unchanged int
	if err := d.Read.QueryRowContext(ctx, `SELECT count(*) FROM documents WHERE system_id=1 AND title='Edited after upload'`).Scan(&unchanged); err != nil || unchanged != 2 {
		t.Fatalf("replay changed current document metadata: count=%d err=%v", unchanged, err)
	}
}
