package api

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	pluginapi "github.com/johnnybravo-xyz/suchi/plugin-api"

	"github.com/johnnybravo-xyz/suchi/core/auth"
)

type searchResponse struct {
	Count   int         `json:"count"`
	Results []SearchHit `json:"results"`
}

func doSearch(t *testing.T, s *Server, query string, principal *pluginapi.Principal) (int, searchResponse, map[string]any) {
	t.Helper()
	recorder := httptest.NewRecorder()
	ctx := context.Background()
	if principal != nil {
		ctx = auth.WithPrincipal(ctx, principal)
	}
	request := httptest.NewRequest("GET", "/api/search/?q="+url.QueryEscape(query), nil).WithContext(ctx)
	s.Search(recorder, request)
	var response searchResponse
	var errorBody map[string]any
	if recorder.Code == 200 {
		if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
			t.Fatalf("decode search response: %v; body=%s", err, recorder.Body.String())
		}
	} else if err := json.Unmarshal(recorder.Body.Bytes(), &errorBody); err != nil {
		t.Fatalf("decode search error: %v; body=%s", err, recorder.Body.String())
	}
	return recorder.Code, response, errorBody
}

func seedRichQueryData(t *testing.T, s *Server) (matchingID, otherID int64) {
	t.Helper()
	inbox := seedStatsJDInbox(t, s.DB)
	if _, err := s.DB.Write.ExecContext(context.Background(), `
		INSERT INTO jd_areas(code_start, code_end, name, position) VALUES (20, 29, 'Finance', 1);
		INSERT INTO jd_categories(id, area_start, code, name) VALUES (6, 20, 22, 'Investments');
		INSERT INTO tags(id, name, slug, created_at, updated_at) VALUES
			(7, 'tax', 'tax', 0, 0),
			(8, 'archived', 'archived', 0, 0);
		INSERT INTO correspondents(id, name, slug, created_at, updated_at)
			VALUES (9, 'Bagmane Prime', 'bagmane-prime', 0, 0);
		INSERT INTO document_types(id, name, slug, created_at, updated_at)
			VALUES (10, 'statement', 'statement', 0, 0)`); err != nil {
		t.Fatal(err)
	}

	matchingID = seedStatsDoc(t, s.DB, 1, "rich-match", "Annual distribution advice", inbox, false,
		time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC).Unix())
	otherID = seedStatsDoc(t, s.DB, 1, "rich-other", "Annual draft", inbox, false,
		time.Date(2025, 2, 1, 0, 0, 0, 0, time.UTC).Unix())
	if _, err := s.DB.Write.ExecContext(context.Background(), `
		UPDATE documents
		SET content = 'Distribution advice for the annual report',
		    jd_category_id = 6,
		    correspondent_id = 9,
		    document_type_id = 10,
		    sensitivity = 'internal',
		    languages = ',de,',
		    added_at = ?,
		    encryption_state = 'encrypted'
		WHERE id = ?`,
		time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC).Unix(), matchingID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.Write.ExecContext(context.Background(),
		`UPDATE documents SET content = 'annual draft', added_at = ? WHERE id = ?`,
		time.Date(2025, 2, 1, 0, 0, 0, 0, time.UTC).Unix(), otherID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.Write.ExecContext(context.Background(),
		`INSERT INTO document_tags(document_id, tag_id) VALUES (?, 7), (?, 8)`,
		matchingID, otherID); err != nil {
		t.Fatal(err)
	}
	return matchingID, otherID
}

func TestRichQueryEndpointParity(t *testing.T) {
	s := newListServer(t)
	matchingID, _ := seedRichQueryData(t, s)
	query := `"distribution advice" jd:22 from:"Bagmane Prime" tag:tax type:statement sensitivity:internal lang:de added:>=2026-01-01 is:encrypted -tag:archived`

	searchCode, searchResult, _ := doSearch(t, s, query, adminPrincipal(1))
	if searchCode != 200 {
		t.Fatalf("search status=%d", searchCode)
	}
	listCode, listRows, listCount := doList(t, s, "/api/documents/?q="+url.QueryEscape(query), adminPrincipal(1))
	if listCode != 200 {
		t.Fatalf("documents status=%d", listCode)
	}
	if searchResult.Count != 1 || listCount != 1 {
		t.Fatalf("counts search=%d documents=%d", searchResult.Count, listCount)
	}
	if searchResult.Results[0].ID != matchingID || listRows[0].ID != matchingID {
		t.Fatalf("search=%+v documents=%+v", searchResult.Results, listRows)
	}
}

func TestRichQueryAcceptedDateIntelligenceParity(t *testing.T) {
	s := newListServer(t)
	matchingID, otherID := seedRichQueryData(t, s)
	if _, err := s.DB.Write.ExecContext(context.Background(), `
		INSERT INTO document_intelligence(
			document_id, intelligence_type, role, value_json, sort_value,
			raw_text, evidence_text, confidence, status, extractor,
			extraction_version, created_at, updated_at
		) VALUES
			(?, 'date', 'renewal', '{"date":"2026-09-14","precision":"day"}',
			 '2026-09-14', '14 September 2026', 'Renews 14 September 2026',
			 0.9, 'accepted', 'test', 1, 0, 0),
			(?, 'date', 'renewal', '{"date":"2026-10-01","precision":"day"}',
			 '2026-10-01', '1 October 2026', 'Renews 1 October 2026',
			 0.9, 'pending', 'test', 1, 0, 0)
	`, matchingID, otherID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.Write.ExecContext(context.Background(), `
		INSERT INTO document_intelligence(
			document_id, intelligence_type, role, value_json, sort_value,
			raw_text, evidence_text, confidence, status, extractor,
			extraction_version, created_at, updated_at
		) VALUES
			(?, 'date', 'renewal', '{"date":"2026-08-01","precision":"day"}',
			 '2026-08-01', '1 August 2026', 'Renews 1 August 2026',
			 0.9, 'accepted', 'test', 1, 0, 0),
			(?, 'date', 'issued', '{"date":"2026-09-15","precision":"day"}',
			 '2026-09-15', '15 September 2026', 'Issued 15 September 2026',
			 0.9, 'accepted', 'test', 1, 0, 0)
	`, otherID, otherID); err != nil {
		t.Fatal(err)
	}
	query := `date:>=2026-09-01 date:<=2026-09-30 date-role:renewal is:dated`
	searchCode, searchResult, _ := doSearch(t, s, query, adminPrincipal(1))
	listCode, listRows, listCount := doList(t, s, "/api/documents/?q="+url.QueryEscape(query), adminPrincipal(1))
	if searchCode != 200 || listCode != 200 || searchResult.Count != 1 || listCount != 1 {
		t.Fatalf("search status/count=%d/%d list status/count=%d/%d", searchCode, searchResult.Count, listCode, listCount)
	}
	if searchResult.Results[0].ID != matchingID || listRows[0].ID != matchingID {
		t.Fatalf("search=%+v documents=%+v", searchResult.Results, listRows)
	}
}

func TestRichQueryPlainTextUsesPrefixAND(t *testing.T) {
	s := newListServer(t)
	matchingID, _ := seedRichQueryData(t, s)
	query := "annu rep"

	_, searchResult, _ := doSearch(t, s, query, adminPrincipal(1))
	_, listRows, listCount := doList(t, s, "/api/documents/?q="+url.QueryEscape(query), adminPrincipal(1))
	if searchResult.Count != 1 || listCount != 1 || searchResult.Results[0].ID != matchingID || listRows[0].ID != matchingID {
		t.Fatalf("search=%+v documents=%+v", searchResult, listRows)
	}
}

func TestListStyleRichTextQueryIsFTSDriven(t *testing.T) {
	s := newListServer(t)
	plan, err := s.compileQuery(context.Background(), "rareterm")
	if err != nil {
		t.Fatal(err)
	}
	where, args := appendFTSDrivenQueryPredicates(
		[]string{"d.trashed_at IS NULL"}, nil, plan)
	rows, err := s.DB.Read.QueryContext(context.Background(), `
		EXPLAIN QUERY PLAN
		SELECT d.id
		FROM documents d`+queryDocumentFTSJoin(plan)+`
		WHERE `+strings.Join(where, " AND "), args...)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var details strings.Builder
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		details.WriteString(detail)
		details.WriteByte('\n')
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	queryPlan := details.String()
	if !strings.Contains(queryPlan, "SCAN documents_fts VIRTUAL TABLE INDEX") ||
		strings.Contains(queryPlan, "CORRELATED") {
		t.Fatalf("rich text query is not FTS-driven:\n%s", queryPlan)
	}
}

func TestRichQueryJDUsesVisibleCodeNotRowID(t *testing.T) {
	s := newListServer(t)
	matchingID, _ := seedRichQueryData(t, s)

	code, response, _ := doSearch(t, s, "jd:22", adminPrincipal(1))
	if code != 200 || response.Count != 1 || response.Results[0].ID != matchingID {
		t.Fatalf("jd:22 status=%d response=%+v", code, response)
	}
	code, _, body := doSearch(t, s, "jd:6", adminPrincipal(1))
	if code != 400 || body["code"] != "bad_query" {
		t.Fatalf("jd:6 status=%d body=%v", code, body)
	}
}

func TestRichQueryErrorsIdentifyFilterAndPosition(t *testing.T) {
	s := newListServer(t)
	seedRichQueryData(t, s)
	code, _, body := doSearch(t, s, "  annual corr:bank", adminPrincipal(1))
	if code != 400 {
		t.Fatalf("status=%d body=%v", code, body)
	}
	if body["code"] != "bad_query" || body["filter"] != "corr" || body["position"] != float64(9) {
		t.Fatalf("body=%v", body)
	}
}

func TestSavedViewsResolveRichQueryBeforeWrite(t *testing.T) {
	s := newListServer(t)
	seedRichQueryData(t, s)

	created := savedViewCall(t, s, "POST", "/api/saved_views/",
		`{"name":"Known values","filter_json":"{\"q\":\"jd:22 tag:tax\"}"}`,
		adminPrincipal(1))
	if created.Code != 201 {
		t.Fatalf("valid create status=%d body=%s", created.Code, created.Body.String())
	}

	invalid := savedViewCall(t, s, "POST", "/api/saved_views/",
		`{"name":"Unknown value","filter_json":"{\"q\":\"tag:missing\"}"}`,
		adminPrincipal(1))
	if invalid.Code != 400 || !strings.Contains(invalid.Body.String(), `"code":"invalid_filter"`) {
		t.Fatalf("invalid create status=%d body=%s", invalid.Code, invalid.Body.String())
	}

	var id int64
	var before string
	if err := s.DB.Read.QueryRow(`SELECT id, filter_json FROM saved_views WHERE name = 'Known values'`).Scan(&id, &before); err != nil {
		t.Fatal(err)
	}
	updated := savedViewCall(t, s, "PATCH", "/api/saved_views/"+strconv.FormatInt(id, 10),
		`{"filter_json":"{\"q\":\"jd:999\"}"}`, adminPrincipal(1))
	if updated.Code != 400 || !strings.Contains(updated.Body.String(), `"code":"invalid_filter"`) {
		t.Fatalf("invalid update status=%d body=%s", updated.Code, updated.Body.String())
	}
	var after string
	if err := s.DB.Read.QueryRow(`SELECT filter_json FROM saved_views WHERE id = ?`, id).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if after != before {
		t.Fatalf("invalid update changed filter_json from %s to %s", before, after)
	}
}

func TestRichQueryCannotBypassVisibility(t *testing.T) {
	s := newListServer(t)
	seedRichQueryData(t, s)
	inbox := int64(1)
	hiddenID := seedStatsDoc(t, s.DB, 2, "rich-hidden", "Private annual report", inbox, false, 200)
	if _, err := s.DB.Write.ExecContext(context.Background(),
		`UPDATE documents SET content = 'annual report' WHERE id = ?`, hiddenID); err != nil {
		t.Fatal(err)
	}

	_, searchResult, _ := doSearch(t, s, "annual report", memberPrincipal(1))
	_, listRows, _ := doList(t, s, "/api/documents/?q=annual+report", memberPrincipal(1))
	searchIDs := make([]int64, 0, len(searchResult.Results))
	for _, hit := range searchResult.Results {
		searchIDs = append(searchIDs, hit.ID)
	}
	listIDs := make([]int64, 0, len(listRows))
	for _, row := range listRows {
		listIDs = append(listIDs, row.ID)
	}
	slices.Sort(searchIDs)
	slices.Sort(listIDs)
	if !slices.Equal(searchIDs, listIDs) {
		t.Fatalf("search ids=%v documents ids=%v", searchIDs, listIDs)
	}
	for _, id := range searchIDs {
		if id == hiddenID {
			t.Fatalf("hidden document %d leaked", hiddenID)
		}
	}
}

func TestRichQueryValuesStayParameterized(t *testing.T) {
	s := newListServer(t)
	matchingID, _ := seedRichQueryData(t, s)
	value := `tax') OR 1=1 --`
	if _, err := s.DB.Write.ExecContext(context.Background(),
		`INSERT INTO tags(id, name, slug, created_at, updated_at) VALUES (11, ?, 'injection-value', 0, 0)`,
		value); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.Write.ExecContext(context.Background(),
		`INSERT INTO document_tags(document_id, tag_id) VALUES (?, 11)`, matchingID); err != nil {
		t.Fatal(err)
	}
	code, response, body := doSearch(t, s, `tag:"tax') OR 1=1 --"`, adminPrincipal(1))
	if code != 200 || response.Count != 1 || response.Results[0].ID != matchingID {
		t.Fatalf("status=%d response=%+v body=%v", code, response, body)
	}
}

func TestRichQueryTrashState(t *testing.T) {
	s := newListServer(t)
	inbox := seedStatsJDInbox(t, s.DB)
	trashedID := seedStatsDoc(t, s.DB, 1, "trash-query", "discarded", inbox, true, 200)

	_, searchResult, _ := doSearch(t, s, "is:trash", adminPrincipal(1))
	_, listRows, listCount := doList(t, s, "/api/documents/?q=is%3Atrash", adminPrincipal(1))
	if searchResult.Count != 1 || listCount != 1 || searchResult.Results[0].ID != trashedID || listRows[0].ID != trashedID {
		t.Fatalf("search=%+v documents=%+v", searchResult, listRows)
	}
}

func TestRichQueryAutocompleteUsesVisibleValues(t *testing.T) {
	s := newListServer(t)
	seedRichQueryData(t, s)

	get := func(query string) []AutocompleteSuggestion {
		t.Helper()
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest("GET", "/api/autocomplete/?q="+url.QueryEscape(query), nil)
		request = request.WithContext(auth.WithPrincipal(context.Background(), adminPrincipal(1)))
		s.Autocomplete(recorder, request)
		if recorder.Code != 200 {
			t.Fatalf("autocomplete %q status=%d body=%s", query, recorder.Code, recorder.Body.String())
		}
		var response struct {
			Results []AutocompleteSuggestion `json:"results"`
		}
		if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
			t.Fatal(err)
		}
		return response.Results
	}

	filterNames := get("j")
	if len(filterNames) != 1 || filterNames[0].Query != "jd:" {
		t.Fatalf("filter suggestions=%+v", filterNames)
	}
	categories := get("annual jd:2")
	if len(categories) != 1 || categories[0].Value != "22 Investments" || categories[0].Query != "annual jd:22" {
		t.Fatalf("category suggestions=%+v", categories)
	}
	tags := get("tag:ta")
	if len(tags) != 1 || tags[0].Query != "tag:tax" {
		t.Fatalf("tag suggestions=%+v", tags)
	}
	longPrefix := strings.Repeat("annual ", 16) + "jd:2"
	if len(longPrefix) <= 100 {
		t.Fatalf("test query length=%d, want over legacy autocomplete limit", len(longPrefix))
	}
	longQuery := get(longPrefix)
	if len(longQuery) != 1 || longQuery[0].Query != strings.TrimSuffix(longPrefix, "2")+"22" {
		t.Fatalf("long query suggestions=%+v", longQuery)
	}
}
