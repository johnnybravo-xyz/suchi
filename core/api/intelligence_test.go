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
