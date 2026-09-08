package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/johnnybravo-xyz/suchi/core/auth"
	"github.com/johnnybravo-xyz/suchi/core/authz"
	"github.com/johnnybravo-xyz/suchi/core/intelligence"
	pluginapi "github.com/johnnybravo-xyz/suchi/plugin-api"
)

func seedDateIntelligence(t *testing.T, s *Server, documentID int64, status, date string) int64 {
	t.Helper()
	candidate, err := intelligence.NewDateCandidate(
		"renewal", date, "day", date, "Renews on "+date, 0.9,
	)
	if err != nil {
		t.Fatal(err)
	}
	result, err := s.DB.Write.ExecContext(context.Background(), `
		INSERT INTO document_intelligence(
			document_id, intelligence_type, role, value_json, sort_value,
			raw_text, evidence_text, confidence, status, extractor,
			extraction_version, created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, 'test', 1, 0, 0)
	`, documentID, candidate.Type, candidate.Role, candidate.ValueJSON,
		candidate.SortValue, candidate.RawText, candidate.EvidenceText,
		candidate.Confidence, status)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := result.LastInsertId()
	return id
}

func doIntelligenceRequest(t *testing.T, s *Server, method, path, body string, p *pluginapi.Principal) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if p != nil {
		req = req.WithContext(auth.WithPrincipal(req.Context(), p))
	}
	rec := httptest.NewRecorder()
	switch {
	case method == http.MethodGet && path == "/api/intelligence/schema":
		s.GetIntelligenceSchema(rec, req)
	case method == http.MethodGet:
		s.ListIntelligence(rec, req)
	case strings.HasSuffix(path, "/extract"):
		s.ExtractIntelligence(rec, req)
	default:
		s.ResolveIntelligence(rec, req)
	}
	return rec
}

func newIntelligenceTestServer(t *testing.T) *Server {
	t.Helper()
	s := newChatTestServer(t)
	s.Authz = authz.ACLAuthorizer{DB: s.DB}
	if _, err := s.DB.Write.ExecContext(context.Background(),
		`UPDATE users SET capabilities = '["archive_intelligence"]' WHERE id = 3`); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestIntelligenceListAppliesCapabilityACLAndStatus(t *testing.T) {
	s := newIntelligenceTestServer(t)
	seedChatDoc(t, s, 40, 2, "Visible policy", "Renews 2026-09-01", "internal", false)
	seedChatDoc(t, s, 41, 1, "Hidden policy", "Renews 2026-10-01", "internal", false)
	seedDateIntelligence(t, s, 40, "accepted", "2026-09-01")
	seedDateIntelligence(t, s, 40, "pending", "2027-09-01")
	seedDateIntelligence(t, s, 41, "accepted", "2026-10-01")
	if _, err := s.DB.Write.ExecContext(context.Background(), `
		INSERT INTO object_acls(object_kind, object_id, principal_kind, principal_id, perm_bits, created_at)
		VALUES ('document', 40, 'user', 3, 1, 0)
	`); err != nil {
		t.Fatal(err)
	}

	if rec := doIntelligenceRequest(t, s, http.MethodGet, "/api/intelligence/?type=date", "", memberPrincipal(2)); rec.Code != http.StatusForbidden {
		t.Fatalf("member without capability status=%d body=%s", rec.Code, rec.Body.String())
	}
	rec := doIntelligenceRequest(t, s, http.MethodGet, "/api/intelligence/?type=date", "", memberPrincipal(3))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var envelope struct {
		Count   int               `json:"count"`
		Results []IntelligenceRow `json:"results"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.Count != 1 || len(envelope.Results) != 1 || envelope.Results[0].DocumentID != 40 {
		t.Fatalf("visible intelligence=%+v", envelope)
	}
}

func TestDemoCalendarIsReadOnlyAndUsesCorpusVisibility(t *testing.T) {
	s := newIntelligenceTestServer(t)
	if _, err := s.DB.Write.Exec(`UPDATE users SET email = ? WHERE id = 1`, authz.DemoCorpusOwnerEmail); err != nil {
		t.Fatal(err)
	}
	seedChatDoc(t, s, 40, 1, "Corpus", "Dates", "internal", false)
	seedChatDoc(t, s, 41, 2, "Other visitor", "Dates", "internal", false)
	seedChatDoc(t, s, 42, 3, "Own upload", "Dates", "internal", false)
	seedChatDoc(t, s, 43, 1, "Trashed corpus", "Dates", "internal", true)
	seedUser(t, s.DB, 4)
	seedChatDoc(t, s, 44, 4, "Private admin document", "Dates", "internal", false)
	for _, id := range []int64{40, 41, 42, 43, 44} {
		seedDateIntelligence(t, s, id, "accepted", "2026-09-01")
	}
	if _, err := s.DB.Write.Exec(`UPDATE document_intelligence SET extractor = 'demo-corpus'`); err != nil {
		t.Fatal(err)
	}
	// Even visible documents must not expose non-demo extraction output.
	seedDateIntelligence(t, s, 40, "accepted", "2027-09-01")
	seedDateIntelligence(t, s, 40, "pending", "2026-10-01")
	for _, test := range []struct {
		kind   string
		userID int64
		count  int
	}{
		{PrincipalKindDemoAnon, 0, 1},
		{PrincipalKindDemoScratch, 3, 2},
	} {
		t.Run(test.kind, func(t *testing.T) {
			// Demo restrictions must win even over a stale or incorrectly assigned admin role.
			p := &pluginapi.Principal{Kind: test.kind, UserID: test.userID, Role: "admin", Scopes: []string{auth.ScopeDocumentsRead, auth.ScopeDocumentsWrite}}
			rec := doIntelligenceRequest(t, s, http.MethodGet, "/api/intelligence/?type=date&page_size=1", "", p)
			var result struct {
				Count   int               `json:"count"`
				Results []IntelligenceRow `json:"results"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil || rec.Code != http.StatusOK {
				t.Fatalf("list: %d %s", rec.Code, rec.Body.String())
			}
			if result.Count != test.count || len(result.Results) != 1 || result.Results[0].DocumentID != 40 {
				t.Fatalf("visible dates: %+v", result)
			}
			rec = doIntelligenceRequest(t, s, http.MethodGet, "/api/intelligence/?type=date&document_ids=41,43,44", "", p)
			if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil || rec.Code != http.StatusOK || result.Count != 0 || len(result.Results) != 0 {
				t.Fatalf("explicit hidden document scope: %d %s", rec.Code, rec.Body.String())
			}
			for _, status := range []string{"pending", "rejected"} {
				if rec := doIntelligenceRequest(t, s, http.MethodGet, "/api/intelligence/?status="+status, "", p); rec.Code != http.StatusForbidden {
					t.Fatalf("%s: %d %s", status, rec.Code, rec.Body.String())
				}
			}
			for _, path := range []string{"/api/intelligence/extract", "/api/intelligence/resolve"} {
				if rec := doIntelligenceRequest(t, s, http.MethodPost, path, `{}`, p); rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "public_demo_denied") {
					t.Fatalf("%s: %d %s", path, rec.Code, rec.Body.String())
				}
			}
			if rec := doChatRequest(t, s, http.MethodPost, "/api/chat", `{"question":"renewal"}`, p); rec.Code != http.StatusForbidden {
				t.Fatalf("chat: %d %s", rec.Code, rec.Body.String())
			}
		})
	}
}

func TestIntelligenceScopedCalendarPaginationAcrossYears(t *testing.T) {
	s := newIntelligenceTestServer(t)
	seedChatDoc(t, s, 57, 3, "Zulu policy", "Dates", "internal", false)
	seedChatDoc(t, s, 58, 1, "Alpha policy", "Dates", "internal", false)
	seedChatDoc(t, s, 59, 1, "Hidden policy", "Dates", "internal", false)
	seedChatDoc(t, s, 60, 3, "Unrelated policy", "Dates", "internal", false)
	seedChatDoc(t, s, 61, 3, "Trashed policy", "Dates", "internal", true)
	if _, err := s.DB.Write.ExecContext(context.Background(), `
		INSERT INTO object_acls(object_kind, object_id, principal_kind, principal_id, perm_bits, created_at)
		VALUES ('document', 58, 'user', 3, 1, 0)
	`); err != nil {
		t.Fatal(err)
	}

	// Insert out of order; equal dates sort by document title, then fact ID.
	last := seedDateIntelligence(t, s, 57, "accepted", "2028-01-01")
	zulu := seedDateIntelligence(t, s, 57, "accepted", "2026-09-01")
	alpha := seedDateIntelligence(t, s, 58, "accepted", "2026-09-01")
	first := seedDateIntelligence(t, s, 57, "accepted", "2024-12-31")
	if _, err := s.DB.Write.ExecContext(context.Background(),
		`UPDATE document_intelligence SET role = 'issued' WHERE id = ?`, alpha); err != nil {
		t.Fatal(err)
	}
	alphaSecond := seedDateIntelligence(t, s, 58, "accepted", "2026-09-01")
	seedDateIntelligence(t, s, 59, "accepted", "2020-01-01")
	seedDateIntelligence(t, s, 60, "accepted", "2020-01-01")
	seedDateIntelligence(t, s, 61, "accepted", "2020-01-01")
	seedDateIntelligence(t, s, 57, "pending", "2020-01-01")
	seedDateIntelligence(t, s, 57, "rejected", "2020-01-02")

	for _, test := range []struct {
		name, bounds string
		pageSize     int
		want         []int64
	}{
		{"all years across pages", "", 2, []int64{first, alpha, alphaSecond, zulu, last}},
		{"calendar page size", "", 500, []int64{first, alpha, alphaSecond, zulu, last}},
		{"inclusive month bounds", "&sort_from=2026-09-01&sort_to=2026-09-30", 2, []int64{alpha, alphaSecond, zulu}},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := "/api/intelligence/?type=date&status=accepted&document_ids=57,58,59,61&page_size=" + itoa(int64(test.pageSize)) + test.bounds
			for offset := 0; offset < len(test.want); offset += test.pageSize {
				rec := doIntelligenceRequest(t, s, http.MethodGet, path, "", memberPrincipal(3))
				if rec.Code != http.StatusOK {
					t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
				}
				var envelope Envelope[IntelligenceRow]
				if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
					t.Fatal(err)
				}
				want := test.want[offset:min(offset+test.pageSize, len(test.want))]
				if envelope.Count != len(test.want) || len(envelope.Results) != len(want) {
					t.Fatalf("offset=%d count=%d results=%+v", offset, envelope.Count, envelope.Results)
				}
				for i, row := range envelope.Results {
					if row.ID != want[i] {
						t.Fatalf("offset=%d row=%d ID=%d, want %d", offset, i, row.ID, want[i])
					}
				}
				if (envelope.Next != "") != (offset+len(want) < len(test.want)) || (envelope.Previous != "") != (offset > 0) {
					t.Fatalf("offset=%d next=%q previous=%q", offset, envelope.Next, envelope.Previous)
				}
				path = envelope.Next
			}
		})
	}
}

func TestIntelligencePrecisionFiltersBeforeScopedPagination(t *testing.T) {
	s := newIntelligenceTestServer(t)
	seedChatDoc(t, s, 71, 3, "Visible policy", "Dates", "internal", false)
	seedChatDoc(t, s, 72, 1, "Hidden policy", "Dates", "internal", false)
	seedChatDoc(t, s, 73, 3, "Unrelated policy", "Dates", "internal", false)
	// All facts sort on January 1, but only two identify that exact day.
	for _, fact := range []struct {
		id, documentID int64
		precision      string
	}{{101, 71, "month"}, {102, 71, "year"}, {103, 71, "day"}, {104, 71, ""}, {105, 72, "day"}, {106, 73, "day"}} {
		value := map[string]string{"date": "2026-01-01"}
		if fact.precision != "" {
			value["precision"] = fact.precision
		}
		encoded, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.DB.Write.ExecContext(context.Background(), `
			INSERT INTO document_intelligence(
				id, document_id, intelligence_type, role, value_json, sort_value,
				raw_text, evidence_text, confidence, status, extractor, extraction_version,
				created_at, updated_at
			) VALUES (?, ?, 'date', 'renewal', ?, '2026-01-01', 'January',
			          'Recorded date', 0.9, 'accepted', 'test', 1, 0, 0)
		`, fact.id, fact.documentID, string(encoded)); err != nil {
			t.Fatal(err)
		}
	}

	for _, test := range []struct {
		precision string
		want      []int64
	}{
		{"", []int64{101, 102, 103, 104}},
		{"day", []int64{103, 104}},
		{"month", []int64{101}},
		{"year", []int64{102}},
	} {
		t.Run("precision="+test.precision, func(t *testing.T) {
			path := "/api/intelligence/?type=date&document_ids=71,72&sort_from=2026-01-01&sort_to=2026-01-01&page_size=1"
			if test.precision != "" {
				path += "&precision=" + test.precision
			}
			for i, wantID := range test.want {
				rec := doIntelligenceRequest(t, s, http.MethodGet, path, "", memberPrincipal(3))
				if rec.Code != http.StatusOK {
					t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
				}
				var envelope Envelope[IntelligenceRow]
				if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
					t.Fatal(err)
				}
				if envelope.Count != len(test.want) || len(envelope.Results) != 1 || envelope.Results[0].ID != wantID {
					t.Fatalf("page=%d response=%+v", i+1, envelope)
				}
				if (envelope.Next != "") != (i+1 < len(test.want)) {
					t.Fatalf("page=%d next=%q", i+1, envelope.Next)
				}
				path = envelope.Next
			}
		})
	}
}

func TestIntelligenceRejectsInvalidPrecisionFilters(t *testing.T) {
	s := newIntelligenceTestServer(t)
	for _, query := range []string{"precision=day", "type=date&precision=week", "type=amount&precision=day"} {
		rec := doIntelligenceRequest(t, s, http.MethodGet, "/api/intelligence/?"+query, "", adminPrincipal(1))
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("query=%q status=%d body=%s", query, rec.Code, rec.Body.String())
		}
	}
}

func TestIntelligenceRejectsNonemptyDocumentScopeWithoutIDs(t *testing.T) {
	s := newIntelligenceTestServer(t)
	seedChatDoc(t, s, 71, 3, "Visible policy", "Dates", "internal", false)
	seedDateIntelligence(t, s, 71, "accepted", "2026-01-01")
	for _, query := range []string{"document_ids=,,,", "document_ids=%20%20", "document_ids=,%20,"} {
		rec := doIntelligenceRequest(t, s, http.MethodGet, "/api/intelligence/?type=date&"+query, "", memberPrincipal(3))
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), `"code":"bad_document_ids"`) {
			t.Fatalf("malformed scope %q: status=%d body=%s", query, rec.Code, rec.Body.String())
		}
	}
	for _, query := range []string{"", "&document_ids="} {
		rec := doIntelligenceRequest(t, s, http.MethodGet, "/api/intelligence/?type=date"+query, "", memberPrincipal(3))
		if rec.Code != http.StatusOK {
			t.Fatalf("empty scope %q: status=%d body=%s", query, rec.Code, rec.Body.String())
		}
		var envelope Envelope[IntelligenceRow]
		if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
			t.Fatal(err)
		}
		if envelope.Count != 1 || len(envelope.Results) != 1 || envelope.Results[0].DocumentID != 71 {
			t.Fatalf("empty scope %q changed full-calendar behavior: %+v", query, envelope)
		}
	}
}

func TestIntelligenceResolveIsPerDocumentAuthorized(t *testing.T) {
	s := newIntelligenceTestServer(t)
	seedChatDoc(t, s, 50, 2, "Editable policy", "Renews 2026-09-01", "internal", false)
	seedChatDoc(t, s, 51, 1, "Hidden policy", "Renews 2026-10-01", "internal", false)
	visible := seedDateIntelligence(t, s, 50, "pending", "2026-09-01")
	hidden := seedDateIntelligence(t, s, 51, "pending", "2026-10-01")
	if _, err := s.DB.Write.ExecContext(context.Background(), `
		INSERT INTO object_acls(object_kind, object_id, principal_kind, principal_id, perm_bits, created_at)
		VALUES ('document', 50, 'user', 3, 3, 0)
	`); err != nil {
		t.Fatal(err)
	}
	body := `{"candidate_ids":[` + itoa(visible) + `,` + itoa(hidden) + `],"decision":"accepted"}`
	rec := doIntelligenceRequest(t, s, http.MethodPost, "/api/intelligence/resolve", body, memberPrincipal(3))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var response intelligenceMutationResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Applied != 1 || !response.Results[0].OK || response.Results[1].Code != "forbidden" {
		t.Fatalf("response=%+v", response)
	}
	var visibleStatus, hiddenStatus string
	if err := s.DB.Read.QueryRow(`SELECT status FROM document_intelligence WHERE id = ?`, visible).Scan(&visibleStatus); err != nil {
		t.Fatal(err)
	}
	if err := s.DB.Read.QueryRow(`SELECT status FROM document_intelligence WHERE id = ?`, hidden).Scan(&hiddenStatus); err != nil {
		t.Fatal(err)
	}
	if visibleStatus != "accepted" || hiddenStatus != "pending" {
		t.Fatalf("statuses visible=%q hidden=%q", visibleStatus, hiddenStatus)
	}
}

type authorizerFunc func(context.Context, authz.Principal, authz.Kind, int64, authz.Perm) error

func (fn authorizerFunc) Can(ctx context.Context, principal authz.Principal,
	kind authz.Kind, id int64, want authz.Perm) error {
	return fn(ctx, principal, kind, id, want)
}

func TestIntelligenceResolveReportsOnlyItsOwnTransition(t *testing.T) {
	s := newIntelligenceTestServer(t)
	seedChatDoc(t, s, 52, 1, "Concurrent policy", "Renews 2026-11-01", "internal", false)
	candidateID := seedDateIntelligence(t, s, 52, "pending", "2026-11-01")

	// Simulate another reviewer committing after the handler's pre-read.
	s.Authz = authorizerFunc(func(ctx context.Context, _ authz.Principal, _ authz.Kind, _ int64, _ authz.Perm) error {
		_, err := s.DB.ExecWrite(ctx, `
			UPDATE document_intelligence SET status = 'accepted' WHERE id = ?`, candidateID)
		return err
	})
	body := `{"candidate_ids":[` + itoa(candidateID) + `],"decision":"rejected"}`
	rec := doIntelligenceRequest(t, s, http.MethodPost, "/api/intelligence/resolve", body, adminPrincipal(1))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var response intelligenceMutationResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Applied != 0 || len(response.Results) != 1 ||
		response.Results[0].OK || response.Results[0].Code != "already_resolved" {
		t.Fatalf("response=%+v", response)
	}
	var status string
	if err := s.DB.Read.QueryRow(`SELECT status FROM document_intelligence WHERE id = ?`, candidateID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "accepted" {
		t.Fatalf("status=%q, want first reviewer's accepted decision", status)
	}
}

func TestIntelligenceExtractQueuesAuthorizedDocuments(t *testing.T) {
	s := newIntelligenceTestServer(t)
	seedChatDoc(t, s, 60, 2, "Editable policy", "Renews 2026-09-01", "internal", false)
	seedChatDoc(t, s, 61, 1, "Hidden policy", "Renews 2026-10-01", "internal", false)
	if _, err := s.DB.Write.ExecContext(context.Background(), `
		INSERT INTO object_acls(object_kind, object_id, principal_kind, principal_id, perm_bits, created_at)
		VALUES ('document', 60, 'user', 3, 3, 0)
	`); err != nil {
		t.Fatal(err)
	}
	rec := doIntelligenceRequest(t, s, http.MethodPost, "/api/intelligence/extract",
		`{"document_ids":[60,61],"types":["date"]}`, memberPrincipal(3))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var jobs int
	if err := s.DB.Read.QueryRow(`SELECT COUNT(*) FROM jobs WHERE kind = 'post-ingest' AND doc_id = 60`).Scan(&jobs); err != nil {
		t.Fatal(err)
	}
	if jobs != 1 {
		t.Fatalf("authorized jobs=%d", jobs)
	}
}

func TestIntelligenceSavedViewAppliesCompleteLegacyScope(t *testing.T) {
	s := newIntelligenceTestServer(t)
	seedChatDoc(t, s, 70, 1, "Confidential quarterly tax invoice", "invoice quarterly tax", "confidential", false)
	seedChatDoc(t, s, 71, 1, "Public quarterly tax invoice", "invoice quarterly tax", "public", false)
	seedDateIntelligence(t, s, 70, "accepted", "2026-09-30")
	seedDateIntelligence(t, s, 71, "accepted", "2026-09-30")
	result, err := s.DB.Write.ExecContext(context.Background(), `
		INSERT INTO saved_views(owner_id, name, filter_json, display, position, shared, created_at, updated_at)
		VALUES (1, 'Quarterly tax review', '{"q":"invoice","sensitivity":"confidential"}', 'list', 0, 0, 0, 0)
	`)
	if err != nil {
		t.Fatal(err)
	}
	viewID, _ := result.LastInsertId()

	rec := doIntelligenceRequest(t, s, http.MethodGet,
		"/api/intelligence/?type=date&view_id="+itoa(viewID), "", adminPrincipal(1))
	if rec.Code != http.StatusOK {
		t.Fatalf("admin status=%d body=%s", rec.Code, rec.Body.String())
	}
	var envelope struct {
		Count   int               `json:"count"`
		Results []IntelligenceRow `json:"results"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.Count != 1 || len(envelope.Results) != 1 || envelope.Results[0].DocumentID != 70 {
		t.Fatalf("legacy view scope=%+v", envelope)
	}

	rec = doIntelligenceRequest(t, s, http.MethodGet,
		"/api/intelligence/?type=date&view_id="+itoa(viewID), "", memberPrincipal(3))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("private view status=%d body=%s", rec.Code, rec.Body.String())
	}
	if _, err := s.DB.Write.ExecContext(context.Background(), `
		UPDATE saved_views SET shared = 1 WHERE id = ?;
		INSERT INTO object_acls(object_kind, object_id, principal_kind, principal_id, perm_bits, created_at)
		VALUES ('document', 70, 'user', 3, 1, 0)
	`, viewID); err != nil {
		t.Fatal(err)
	}
	rec = doIntelligenceRequest(t, s, http.MethodGet,
		"/api/intelligence/?type=date&view_id="+itoa(viewID), "", memberPrincipal(3))
	if rec.Code != http.StatusOK {
		t.Fatalf("shared view status=%d body=%s", rec.Code, rec.Body.String())
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.Count != 1 || envelope.Results[0].DocumentID != 70 {
		t.Fatalf("shared view ACL scope=%+v", envelope)
	}
}
