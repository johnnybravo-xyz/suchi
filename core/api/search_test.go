package api

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	pluginapi "github.com/johnnybravo-xyz/suchi/plugin-api"

	"github.com/johnnybravo-xyz/suchi/core/auth"
)

func TestSearchRanksBeforeBuildingSnippets(t *testing.T) {
	s := newChatTestServer(t)
	query := "EXPLAIN " + searchFTSPageSQL(
		" WHERE documents_fts MATCH ? AND d.trashed_at IS NULL",
		"bm25(documents_fts, ?, ?)",
	)
	rows, err := s.DB.Read.QueryContext(context.Background(), query,
		bm25TitleWeight, bm25ContentWeight, "needle*", 25, 0, "needle*")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	seenReturn := false
	seenRankedRewind := false
	snippets := 0
	for rows.Next() {
		var addr, p1, p2, p3, p5 int
		var opcode string
		var p4, comment any
		if err := rows.Scan(&addr, &opcode, &p1, &p2, &p3, &p4, &p5, &comment); err != nil {
			t.Fatal(err)
		}
		if opcode == "Return" {
			seenReturn = true
		}
		if seenReturn && opcode == "Rewind" {
			seenRankedRewind = true
		}
		if opcode == "Function" && strings.Contains(fmt.Sprint(p4), "snippet(") {
			snippets++
			if !seenRankedRewind {
				t.Fatalf("snippet opcode %d precedes the bounded ranked-row loop", addr)
			}
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if snippets != 1 || !seenRankedRewind {
		t.Fatalf("unexpected query bytecode: snippets=%d ranked_rewind=%t", snippets, seenRankedRewind)
	}
}

func TestSearchFTSPaginationKeepsStableSnippets(t *testing.T) {
	s := newChatTestServer(t)
	for id := int64(1); id <= 3; id++ {
		seedChatDoc(t, s, id, 1, fmt.Sprintf("Record %d", id), "needle evidence", "public", false)
	}
	req := httptest.NewRequest(http.MethodGet,
		"/api/search/?q=needle&page=2&page_size=1&recency=off", nil)
	req = req.WithContext(auth.WithPrincipal(req.Context(), adminPrincipal(1)))
	rec := httptest.NewRecorder()
	s.Search(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var out Envelope[SearchHit]
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Count != 3 || len(out.Results) != 1 || out.Results[0].ID != 2 {
		t.Fatalf("unexpected page: %+v", out)
	}
	if !strings.Contains(out.Results[0].Snippet, "<mark>needle</mark>") {
		t.Fatalf("snippet=%q", out.Results[0].Snippet)
	}
}

func TestSearchRecencyPaginationAppliesFiltersAndACLBeforeLimit(t *testing.T) {
	s := newListServer(t)
	inbox := seedStatsJDInbox(t, s.DB)
	seedMember(t, s, 2, `[]`)
	granted := seedStatsDoc(t, s.DB, 1, "search-granted", "Needle granted", inbox, false, 100)
	owned := seedStatsDoc(t, s.DB, 2, "search-owned", "Needle owned", inbox, false, 200)
	seedStatsDoc(t, s.DB, 1, "search-hidden", "Needle hidden", inbox, false, 300)
	if _, err := s.DB.Write.ExecContext(context.Background(), `
		UPDATE documents SET sensitivity = 'internal';
		INSERT INTO object_acls(object_kind, object_id, principal_kind, principal_id, perm_bits, created_at)
		VALUES ('document', ?, 'user', 2, 1, 0)`, granted); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet,
		"/api/search/?q=needle&sensitivity=internal&page=2&page_size=1", nil)
	req = req.WithContext(auth.WithPrincipal(req.Context(), memberPrincipal(2)))
	rec := httptest.NewRecorder()
	s.Search(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var out Envelope[SearchHit]
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Count != 2 || len(out.Results) != 1 || out.Results[0].ID != granted {
		t.Fatalf("unexpected filtered page: %+v; owned=%d", out, owned)
	}
}

func TestSearchMalformedQueryReturnsBadRequest(t *testing.T) {
	s := &Server{
		DB:  openTestDB(t),
		Log: slog.New(slog.NewTextHandler(os.Stderr, nil)),
	}
	req := httptest.NewRequest(http.MethodGet, "/api/search/?q=%22", nil)
	req = req.WithContext(auth.WithPrincipal(context.Background(), adminPrincipal(1)))
	rec := httptest.NewRecorder()

	s.Search(rec, req)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), `"code":"bad_query"`) {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestSearchRequiresDocumentsReadScope(t *testing.T) {
	s := &Server{
		DB:  openTestDB(t),
		Log: slog.New(slog.NewTextHandler(os.Stderr, nil)),
	}
	request := func(scopes []string) *httptest.ResponseRecorder {
		t.Helper()
		principal := &pluginapi.Principal{Kind: "token", UserID: 1, Role: "admin", Scopes: scopes}
		req := httptest.NewRequest(http.MethodGet, "/api/search/?q=", nil)
		req = req.WithContext(auth.WithPrincipal(context.Background(), principal))
		rec := httptest.NewRecorder()
		s.Search(rec, req)
		return rec
	}

	denied := request([]string{auth.ScopeEventsRead})
	if denied.Code != http.StatusForbidden || !strings.Contains(denied.Body.String(), `"code":"insufficient_scope"`) {
		t.Fatalf("missing scope status=%d body=%s", denied.Code, denied.Body.String())
	}
	allowed := request([]string{auth.ScopeDocumentsRead})
	if allowed.Code != http.StatusOK {
		t.Fatalf("read scope status=%d body=%s", allowed.Code, allowed.Body.String())
	}
}

func TestSearchValidatesLanguageFilter(t *testing.T) {
	s := &Server{
		DB:  openTestDB(t),
		Log: slog.New(slog.NewTextHandler(os.Stderr, nil)),
	}
	request := func(rawURL string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, rawURL, nil)
		req = req.WithContext(auth.WithPrincipal(context.Background(), adminPrincipal(1)))
		rec := httptest.NewRecorder()
		s.Search(rec, req)
		return rec
	}

	for _, rawURL := range []string{
		"/api/search/?q=needle&lang=en-US",
		"/api/search/?q=needle&lang=%C3%A9%C3%A9",
		"/api/search/?q=needle&lang=12",
		"/api/search/?q=needle&lang=abcd",
		"/api/search/?q=&lang=en-US",
	} {
		rec := request(rawURL)
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), `"code":"bad_lang"`) {
			t.Fatalf("url=%s status=%d body=%s", rawURL, rec.Code, rec.Body.String())
		}
	}

	if rec := request("/api/search/?q=&lang=%20DE%20"); rec.Code != http.StatusOK {
		t.Fatalf("normalized language status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestSearchFacetIDLimits(t *testing.T) {
	s := &Server{
		DB:  openTestDB(t),
		Log: slog.New(slog.NewTextHandler(os.Stderr, nil)),
	}
	request := func(field, raw string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet,
			"/api/search/?q=needle&"+field+"="+raw, nil)
		req = req.WithContext(auth.WithPrincipal(req.Context(), adminPrincipal(1)))
		rec := httptest.NewRecorder()
		s.Search(rec, req)
		return rec
	}

	for _, test := range []struct {
		field string
		code  string
	}{
		{"tags__id__in", "bad_tags"},
		{"correspondents__id__in", "bad_correspondents"},
	} {
		for _, raw := range []string{testCSVRange(100), testRepeatedCSV(1, 100)} {
			if rec := request(test.field, raw); rec.Code != http.StatusOK {
				t.Fatalf("%s accepted boundary status=%d body=%s", test.field, rec.Code, rec.Body.String())
			}
		}
		for _, raw := range []string{testCSVRange(101), testRepeatedCSV(1, 101)} {
			rec := request(test.field, raw)
			if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), `"code":"`+test.code+`"`) {
				t.Fatalf("%s oversized status=%d body=%s", test.field, rec.Code, rec.Body.String())
			}
		}
	}
}
