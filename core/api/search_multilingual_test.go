// SPDX-License-Identifier: AGPL-3.0-or-later

package api

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"net/url"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/johnnybravo-xyz/suchi/core/auth"
)

func seedMultilingualDocs(t *testing.T, s *Server) {
	t.Helper()
	for _, doc := range []struct {
		id                          int64
		title, content, sensitivity string
	}{
		{1, "Beitrag", "Ihre Krankenversicherung für 2026", "public"},
		{2, "Rechnung", "請求書を送付します", "public"},
		{3, "Adresse", "東京都に住む", "public"},
		{4, "Bank", "Überweisung erhalten", "public"},
		{5, "Power", "electricity invoices for March", "public"},
		{6, "Water", "the bills for water", "public"},
		{7, "Mixed", "bill and Krankenversicherung", "public"},
		{8, "Reissue", "reinvoiced after correction", "public"},
		{9, "Private", "Haftpflichtversicherung", "confidential"},
	} {
		seedChatDoc(t, s, doc.id, 1, doc.title, doc.content, doc.sensitivity, false)
	}
}

func searchIDs(t *testing.T, s *Server, query string) ([]int64, map[int64]string) {
	t.Helper()
	code, response, body := doSearch(t, s, query, adminPrincipal(1))
	if code != 200 {
		t.Fatalf("search %q: status=%d body=%v", query, code, body)
	}
	ids := make([]int64, 0, len(response.Results))
	snippets := map[int64]string{}
	for _, hit := range response.Results {
		ids = append(ids, hit.ID)
		snippets[hit.ID] = hit.Snippet
	}
	if response.Count != len(ids) {
		t.Fatalf("search %q: count=%d results=%d", query, response.Count, len(ids))
	}
	return ids, snippets
}

func TestSearchMatchesAcrossLanguages(t *testing.T) {
	s := newChatTestServer(t)
	seedMultilingualDocs(t, s)
	for _, tc := range []struct {
		query string
		want  []int64
	}{
		{"Versicherung", []int64{1, 7, 9}}, // German compounds
		{"請求書", []int64{2}},                // Japanese without spaces
		{"東京", []int64{3}},                 // two characters: substring scan
		{"uberweisung", []int64{4}},        // diacritics fold
		{"bills", []int64{6, 7}},           // English stems: bills -> bill
		{"bills Versicherung", []int64{7}}, // each clause may match a different index
		{"Versicherung -bill", []int64{1, 9}},
		{`content:"Krankenversicherung"`, []int64{1, 7}},
	} {
		got, _ := searchIDs(t, s, tc.query)
		sort.Slice(got, func(i, j int) bool { return got[i] < got[j] })
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("search %q = %v, want %v", tc.query, got, tc.want)
		}
		_, rows, count := doList(t, s, "/api/documents/?q="+url.QueryEscape(tc.query), adminPrincipal(1))
		if count != len(tc.want) || len(rows) != len(tc.want) {
			t.Errorf("documents %q count=%d rows=%d, want %d", tc.query, count, len(rows), len(tc.want))
		}
	}
}

func TestSearchRanksWordMatchesBeforeSubstringMatches(t *testing.T) {
	s := newChatTestServer(t)
	seedMultilingualDocs(t, s)
	got, snippets := searchIDs(t, s, "invoice")
	if !reflect.DeepEqual(got, []int64{5, 8}) {
		t.Fatalf("order=%v, want word hit 5 before substring hit 8", got)
	}
	if !strings.Contains(snippets[8], "<mark>") || strings.Count(snippets[8], "<mark>") != strings.Count(snippets[8], "</mark>") {
		t.Fatalf("substring-only hit snippet=%q", snippets[8])
	}
	_, snippets = searchIDs(t, s, "Versicherung")
	if !strings.Contains(snippets[1], "<mark>versicherung</mark>") {
		t.Fatalf("compound snippet=%q", snippets[1])
	}
	if snippets[9] != "" {
		t.Fatalf("confidential snippet leaked: %q", snippets[9])
	}
}

// A document found by both indexes keeps its word-index score; a substring-only
// hit keeps its trigram score.
func TestSearchScoresEachHitFromItsBestIndex(t *testing.T) {
	s := newChatTestServer(t)
	seedMultilingualDocs(t, s)
	plan, err := s.compileQuery(context.Background(), "invoice")
	if err != nil {
		t.Fatal(err)
	}
	want := map[int64]float64{}
	for _, index := range []struct{ table, match string }{
		{"documents_trigram", plan.TrigramMatch},
		{"documents_fts", plan.Match},
	} {
		rows, err := s.DB.Read.Query(`SELECT rowid, bm25(`+index.table+`, ?, ?) FROM `+index.table+` WHERE `+index.table+` MATCH ?`,
			bm25TitleWeight, bm25ContentWeight, index.match)
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var id int64
			var score float64
			if err := rows.Scan(&id, &score); err != nil {
				t.Fatal(err)
			}
			want[id] = score
		}
		if err := rows.Close(); err != nil {
			t.Fatal(err)
		}
	}
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest("GET", "/api/search/?q=invoice&recency=off", nil).
		WithContext(auth.WithPrincipal(context.Background(), adminPrincipal(1)))
	s.Search(recorder, request)
	var response searchResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if len(response.Results) != 2 || len(want) != 2 {
		t.Fatalf("results=%+v index scores=%v", response.Results, want)
	}
	for _, hit := range response.Results {
		if hit.Rank != want[hit.ID] {
			t.Errorf("document %d rank=%v, want %v", hit.ID, hit.Rank, want[hit.ID])
		}
	}
}

func TestChatRetrievesUnspacedTerms(t *testing.T) {
	s := newChatTestServer(t)
	seedMultilingualDocs(t, s)
	ctx := auth.WithPrincipal(context.Background(), adminPrincipal(1))
	tx, err := s.DB.Read.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	where, args, err := s.chatSourceWhere(ctx, tx, ChatScope{}, false, false)
	if err != nil {
		t.Fatal(err)
	}
	markers, err := newChatFTSMarkers()
	if err != nil {
		t.Fatal(err)
	}
	sources, err := s.queryChatFTSSources(ctx, tx, []string{"請求書"}, nil, where, args, markers, 5, chatMaxSourceBalanced)
	if err != nil || len(sources) != 1 || sources[0].source.ID != 2 || !strings.Contains(sources[0].passages[0], "請求書") {
		t.Fatalf("sources=%+v err=%v", sources, err)
	}
	passages, err := s.queryChatAdditionalPassages(ctx, tx, "versicherung", []int64{1}, markers)
	if err != nil || !strings.Contains(passages[1], "versicherung") {
		t.Fatalf("passages=%+v err=%v", passages, err)
	}
}

// Equal text relevance: the recency boost puts the newer document first, and
// recency=off falls back to document ID order.
func TestSearchRecencyBreaksEqualRelevance(t *testing.T) {
	s := newChatTestServer(t)
	seedChatDoc(t, s, 1, 1, "Old", "quarterly statement", "public", false)
	seedChatDoc(t, s, 2, 1, "New", "quarterly statement", "public", false)
	if _, err := s.DB.Write.Exec(`UPDATE documents SET created_at = unixepoch() WHERE id = 2`); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		params string
		want   []int64
	}{
		{"q=statement", []int64{2, 1}},
		{"q=statement&recency=off", []int64{1, 2}},
	} {
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest("GET", "/api/search/?"+tc.params, nil).
			WithContext(auth.WithPrincipal(context.Background(), adminPrincipal(1)))
		s.Search(recorder, request)
		var response searchResponse
		if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
			t.Fatal(err)
		}
		got := []int64{}
		for _, hit := range response.Results {
			got = append(got, hit.ID)
		}
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s order=%v, want %v", tc.params, got, tc.want)
		}
	}
}
