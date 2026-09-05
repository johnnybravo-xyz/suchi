package api

// GET /api/documents/ contract:
//   - anonymous → 401
//   - happy-path returns seeded docs with DRF envelope
//   - jd_category_id filter narrows
//   - trashed toggle hides live vs shows trashed
//   - non-admin owner-scope hides other users' docs
//   - tags__id__in enforces AND-match
//   - ordering honors the allow-list

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"testing"

	pluginapi "github.com/johnnybravo-xyz/suchi/plugin-api"

	"github.com/johnnybravo-xyz/suchi/core/auth"
	"github.com/johnnybravo-xyz/suchi/core/authz"
)

func newListServer(t *testing.T) *Server {
	t.Helper()
	d := openTestDB(t)
	return &Server{
		DB:    d,
		Log:   slog.New(slog.NewTextHandler(os.Stderr, nil)),
		Authz: authz.ACLAuthorizer{DB: d},
	}
}

func doList(t *testing.T, s *Server, path string, p *pluginapi.Principal) (int, []DocumentListRow, int) {
	t.Helper()
	rec := httptest.NewRecorder()
	ctx := context.Background()
	if p != nil {
		ctx = auth.WithPrincipal(ctx, p)
	}
	r := httptest.NewRequest("GET", path, nil).WithContext(ctx)
	s.ListDocuments(rec, r)
	var env struct {
		Count   int               `json:"count"`
		Results []DocumentListRow `json:"results"`
	}
	if rec.Code == 200 {
		if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
			t.Fatalf("json: %v; body=%s", err, rec.Body.String())
		}
	}
	return rec.Code, env.Results, env.Count
}

func TestListDocuments_Anonymous(t *testing.T) {
	s := newListServer(t)
	code, _, _ := doList(t, s, "/api/documents/", nil)
	if code != 401 {
		t.Fatalf("anonymous status=%d, want 401", code)
	}
}

func TestListDocuments_ReturnsSeeded(t *testing.T) {
	s := newListServer(t)
	inbox := seedStatsJDInbox(t, s.DB)
	seedStatsDoc(t, s.DB, 1, "sha_a", "March rent", inbox, false, 100)
	seedStatsDoc(t, s.DB, 1, "sha_b", "April electricity", inbox, false, 200)

	code, rows, count := doList(t, s, "/api/documents/", adminPrincipal(1))
	if code != 200 {
		t.Fatalf("status=%d", code)
	}
	if count != 2 || len(rows) != 2 {
		t.Fatalf("count=%d results=%d, want 2/2", count, len(rows))
	}
	// Default ordering is -created_at, so the newer doc comes first.
	if rows[0].Title != "April electricity" {
		t.Errorf("ordering broken: %+v", rows)
	}
	// JD denormalization must land.
	for _, r := range rows {
		if r.JDCategoryID != inbox || r.JDCategoryCode == 0 {
			t.Errorf("JD fields empty on row: %+v", r)
		}
	}
	// Empty slices, not null.
	if rows[0].Tags == nil {
		t.Errorf("Tags should be []string{}, got nil")
	}
}

func TestListDocuments_OrderingHasStableIDTieBreak(t *testing.T) {
	s := newListServer(t)
	inbox := seedStatsJDInbox(t, s.DB)
	first := seedStatsDoc(t, s.DB, 1, "same-time-a", "first", inbox, false, 100)
	second := seedStatsDoc(t, s.DB, 1, "same-time-b", "second", inbox, false, 100)

	_, rows, _ := doList(t, s, "/api/documents/", adminPrincipal(1))
	if len(rows) != 2 {
		t.Fatalf("descending rows=%+v", rows)
	}
	if rows[0].ID != second || rows[1].ID != first {
		t.Fatalf("descending ids=%v,%v, want %d,%d", rows[0].ID, rows[1].ID, second, first)
	}
	_, rows, _ = doList(t, s, "/api/documents/?ordering=created_at", adminPrincipal(1))
	if len(rows) != 2 {
		t.Fatalf("ascending rows=%+v", rows)
	}
	if rows[0].ID != first || rows[1].ID != second {
		t.Fatalf("ascending ids=%v,%v, want %d,%d", rows[0].ID, rows[1].ID, first, second)
	}

	if _, err := s.DB.Write.ExecContext(context.Background(),
		`UPDATE documents SET title = 'same title' WHERE id IN (?, ?)`, first, second); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		ordering string
		want     []int64
	}{
		{"updated_at", []int64{first, second}},
		{"-updated_at", []int64{second, first}},
		{"title", []int64{first, second}},
		{"-title", []int64{second, first}},
	} {
		_, rows, _ = doList(t, s, "/api/documents/?ordering="+test.ordering, adminPrincipal(1))
		if len(rows) != 2 || rows[0].ID != test.want[0] || rows[1].ID != test.want[1] {
			t.Fatalf("ordering %q rows=%+v, want ids=%v", test.ordering, rows, test.want)
		}
	}
}

func TestListDocuments_CorrespondentsPreferJunctionAndPreserveFallback(t *testing.T) {
	s := newListServer(t)
	inbox := seedStatsJDInbox(t, s.DB)
	fallback := seedStatsDoc(t, s.DB, 1, "corr-fallback", "singular", inbox, false, 100)
	junction := seedStatsDoc(t, s.DB, 1, "corr-junction", "multiple roles", inbox, false, 200)
	junctionOnly := seedStatsDoc(t, s.DB, 1, "corr-junction-only", "junction only", inbox, false, 300)
	empty := seedStatsDoc(t, s.DB, 1, "corr-empty", "none", inbox, false, 400)
	if _, err := s.DB.Write.ExecContext(t.Context(), `
		INSERT INTO correspondents(id, name, slug, created_at, updated_at) VALUES
			(1, 'Alpha', 'alpha', 0, 0), (2, 'Beta', 'beta', 0, 0), (3, 'Gamma', 'gamma', 0, 0);
		UPDATE documents SET correspondent_id = 1 WHERE id IN (?, ?)
	`, fallback, junction); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.Write.ExecContext(t.Context(), `
		INSERT INTO document_correspondents(document_id, correspondent_id, role) VALUES
			(?, 3, 'recipient'), (?, 2, 'sender'), (?, 2, 'recipient'), (?, 2, 'sender')
	`, junction, junction, junction, junctionOnly); err != nil {
		t.Fatal(err)
	}

	code, rows, count := doList(t, s, "/api/documents/", adminPrincipal(1))
	if code != 200 || count != 4 || len(rows) != 4 {
		t.Fatalf("status=%d count=%d rows=%+v", code, count, rows)
	}
	want := map[int64][]string{
		fallback:     {"Alpha"},
		junction:     {"Beta", "Beta", "Gamma"},
		junctionOnly: {"Beta"},
		empty:        nil,
	}
	for _, row := range rows {
		if !reflect.DeepEqual(row.Correspondents, want[row.ID]) {
			t.Errorf("doc %d correspondents=%v, want %v", row.ID, row.Correspondents, want[row.ID])
		}
	}
}

func TestListDocuments_ExactDocumentSnapshotStillAppliesACL(t *testing.T) {
	s := newListServer(t)
	inbox := seedStatsJDInbox(t, s.DB)
	visible := seedStatsDoc(t, s.DB, 2, "snapshot-visible", "visible", inbox, false, 100)
	hidden := seedStatsDoc(t, s.DB, 1, "snapshot-hidden", "hidden", inbox, false, 200)

	path := fmt.Sprintf("/api/documents/?document_ids=%d,%d", visible, hidden)
	code, rows, count := doList(t, s, path, memberPrincipal(2))
	if code != 200 || count != 1 || len(rows) != 1 || rows[0].ID != visible {
		t.Fatalf("status=%d count=%d rows=%+v", code, count, rows)
	}
}

func TestListDocuments_JDCategoryFilter(t *testing.T) {
	s := newListServer(t)
	inbox := seedStatsJDInbox(t, s.DB)
	// A second JD category so we can filter.
	if _, err := s.DB.Write.ExecContext(context.Background(), `
		INSERT INTO jd_categories(id, area_start, code, name) VALUES (2, 0, 2, 'Tax')`); err != nil {
		t.Fatal(err)
	}
	seedStatsDoc(t, s.DB, 1, "sha_a", "inbox doc", inbox, false, 100)
	// Move it: seedStatsDoc leaves docs under inbox; use UPDATE to
	// switch one row to cat 2.
	seedStatsDoc(t, s.DB, 1, "sha_b", "tax doc", inbox, false, 200)
	if _, err := s.DB.Write.ExecContext(context.Background(),
		`UPDATE documents SET jd_category_id = 2 WHERE original_blob = 'sha_b'`); err != nil {
		t.Fatal(err)
	}

	// Filter to Inbox.
	_, rows, count := doList(t, s, "/api/documents/?jd_category_id=1", adminPrincipal(1))
	if count != 1 || len(rows) != 1 || rows[0].Title != "inbox doc" {
		t.Errorf("jd_category filter broken: count=%d rows=%+v", count, rows)
	}
}

func TestListDocuments_TrashedToggle(t *testing.T) {
	s := newListServer(t)
	inbox := seedStatsJDInbox(t, s.DB)
	seedStatsDoc(t, s.DB, 1, "sha_live", "live", inbox, false, 100)
	seedStatsDoc(t, s.DB, 1, "sha_gone", "gone", inbox, true, 200) // trashed

	// Default: live only.
	_, rows, _ := doList(t, s, "/api/documents/", adminPrincipal(1))
	if len(rows) != 1 || rows[0].Title != "live" {
		t.Errorf("default should hide trashed: %+v", rows)
	}
	// ?trashed=1: trashed only.
	_, rows, _ = doList(t, s, "/api/documents/?trashed=1", adminPrincipal(1))
	if len(rows) != 1 || rows[0].Title != "gone" || rows[0].TrashedAt == nil {
		t.Errorf("trashed=1 should show only trashed: %+v", rows)
	}
}

func TestListDocuments_MemberACLScope(t *testing.T) {
	s := newListServer(t)
	inbox := seedStatsJDInbox(t, s.DB)
	seedStatsDoc(t, s.DB, 1, "sha_1", "user1 doc", inbox, false, 100)
	seedStatsDoc(t, s.DB, 2, "sha_2", "user2 doc", inbox, false, 200)

	// User 2 as member sees only their own row — no ACL grants
	// wired here, and admin bypass is off.
	_, rows, count := doList(t, s, "/api/documents/", memberPrincipal(2))
	if count != 1 || len(rows) != 1 || rows[0].Title != "user2 doc" {
		t.Errorf("member ACL scope broken: count=%d rows=%+v", count, rows)
	}
}

func TestListDocuments_AllTagsFilterPrecedesPagination(t *testing.T) {
	s := newListServer(t)
	inbox := seedStatsJDInbox(t, s.DB)
	first := seedStatsDoc(t, s.DB, 1, "sha_first", "first match", inbox, false, 100)
	second := seedStatsDoc(t, s.DB, 1, "sha_second", "second match", inbox, false, 200)
	newest := seedStatsDoc(t, s.DB, 1, "sha_newest", "not a match", inbox, false, 300)
	if _, err := s.DB.Write.ExecContext(context.Background(), `
		INSERT INTO tags(id, name, slug, created_at, updated_at) VALUES
			(1, 'one', 'one', 0, 0),
			(2, 'two', 'two', 0, 0);
		INSERT INTO document_tags(document_id, tag_id) VALUES
			(?, 1), (?, 2), (?, 1), (?, 2), (?, 1)
	`, first, first, second, second, newest); err != nil {
		t.Fatal(err)
	}

	_, rows, count := doList(t, s,
		"/api/documents/?tags__id__in=1,2,1&page_size=1", adminPrincipal(1))
	if count != 2 || len(rows) != 1 || rows[0].ID != second {
		t.Fatalf("page 1 count=%d rows=%+v", count, rows)
	}
	_, rows, count = doList(t, s,
		"/api/documents/?tags__id__in=1,2,1&page_size=1&page=2", adminPrincipal(1))
	if count != 2 || len(rows) != 1 || rows[0].ID != first {
		t.Fatalf("page 2 count=%d rows=%+v", count, rows)
	}
}

func TestListDocuments_FacetIDLimits(t *testing.T) {
	s := newListServer(t)
	request := func(field, raw string) *httptest.ResponseRecorder {
		t.Helper()
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/api/documents/?"+field+"="+raw, nil)
		req = req.WithContext(auth.WithPrincipal(req.Context(), adminPrincipal(1)))
		s.ListDocuments(rec, req)
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
			if rec := request(test.field, raw); rec.Code != 200 {
				t.Fatalf("%s accepted boundary status=%d body=%s", test.field, rec.Code, rec.Body.String())
			}
		}
		for _, raw := range []string{testCSVRange(101), testRepeatedCSV(1, 101)} {
			rec := request(test.field, raw)
			if rec.Code != 400 || !strings.Contains(rec.Body.String(), `"code":"`+test.code+`"`) {
				t.Fatalf("%s oversized status=%d body=%s", test.field, rec.Code, rec.Body.String())
			}
		}
	}
}

func TestListDocuments_MalformedFTSQueryReturnsBadRequest(t *testing.T) {
	s := newListServer(t)
	inbox := seedStatsJDInbox(t, s.DB)
	seedStatsDoc(t, s.DB, 1, "search-sha", "searchable", inbox, false, 1)
	code, _, _ := doList(t, s, "/api/documents/?q=%22", adminPrincipal(1))
	if code != 400 {
		t.Fatalf("status=%d", code)
	}
}

func TestListDocuments_OrderingAllowList(t *testing.T) {
	s := newListServer(t)
	inbox := seedStatsJDInbox(t, s.DB)
	seedStatsDoc(t, s.DB, 1, "sha_a", "aardvark", inbox, false, 100)
	seedStatsDoc(t, s.DB, 1, "sha_z", "zebra", inbox, false, 200)

	// Title ASC.
	_, rows, _ := doList(t, s, "/api/documents/?ordering=title", adminPrincipal(1))
	if len(rows) != 2 || rows[0].Title != "aardvark" {
		t.Errorf("ordering=title failed: %+v", rows)
	}
	// Title DESC.
	_, rows, _ = doList(t, s, "/api/documents/?ordering=-title", adminPrincipal(1))
	if len(rows) != 2 || rows[0].Title != "zebra" {
		t.Errorf("ordering=-title failed: %+v", rows)
	}
	// Unknown ordering falls back to -created_at (newer first).
	_, rows, _ = doList(t, s, "/api/documents/?ordering=nope", adminPrincipal(1))
	if len(rows) != 2 || rows[0].Title != "zebra" {
		t.Errorf("bad ordering should fall back to -created_at: %+v", rows)
	}
}
