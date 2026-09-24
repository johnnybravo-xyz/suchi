// SPDX-License-Identifier: AGPL-3.0-or-later

package api

// Bulk-edit surface. Load-bearing invariants:
//   - anonymous → 401; missing scope → 403 (RequireScope path)
//   - unknown method → 400 (bad_method)
//   - empty documents[] → 400
//   - happy path (set_correspondent, add_tag, trash) mutates the row
//   - one audit event per call (not N)
//   - authorized set is empty → 200 with all rows forbidden and
//     zero db writes (no false-positive audit trail)

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"testing"
	"time"

	pluginapi "github.com/johnnybravo-xyz/suchi/plugin-api"

	"github.com/johnnybravo-xyz/suchi/core/approvals"
	"github.com/johnnybravo-xyz/suchi/core/auth"
	"github.com/johnnybravo-xyz/suchi/core/authz"
	"github.com/johnnybravo-xyz/suchi/core/documentstate"
	"github.com/johnnybravo-xyz/suchi/core/trash"
)

func newBulkServer(t *testing.T) *Server {
	t.Helper()
	d := openTestDB(t)
	return &Server{Actions: testActions(t),
		DB:    d,
		Log:   slog.New(slog.NewTextHandler(os.Stderr, nil)),
		Authz: authz.ACLAuthorizer{DB: d},
	}
}

func doBulkEdit(t *testing.T, s *Server, body any, p *pluginapi.Principal) (int, BulkEditResponse) {
	t.Helper()
	raw, _ := json.Marshal(body)
	rec := httptest.NewRecorder()
	ctx := context.Background()
	if p != nil {
		ctx = auth.WithPrincipal(ctx, p)
	}
	r := httptest.NewRequest("POST", "/api/documents/bulk_edit", bytes.NewReader(raw)).WithContext(ctx)
	r.Header.Set("Content-Type", "application/json")
	s.BulkEdit(rec, r)
	var out BulkEditResponse
	if rec.Code == 200 {
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
	}
	return rec.Code, out
}

func TestBulkEdit_Anonymous(t *testing.T) {
	s := newBulkServer(t)
	code, _ := doBulkEdit(t, s, map[string]any{"documents": []int64{1}, "method": "trash"}, nil)
	if code != 401 {
		t.Fatalf("anonymous status=%d, want 401", code)
	}
}

func TestBulkEdit_BadMethod(t *testing.T) {
	s := newBulkServer(t)
	docA := seedStatsDoc(t, s.DB, 1, "sha_a", "a", seedStatsJDInbox(t, s.DB), false, 0)
	code, _ := doBulkEdit(t, s,
		map[string]any{"documents": []int64{docA}, "method": "nuke"},
		adminPrincipal(1))
	if code != 400 {
		t.Errorf("unknown-method status=%d, want 400", code)
	}
}

func TestBulkEdit_TrashHappyPath(t *testing.T) {
	s := newBulkServer(t)
	d := s.DB
	inbox := seedStatsJDInbox(t, s.DB)
	a := seedStatsDoc(t, s.DB, 1, "sha_a", "a", inbox, false, 0)
	b := seedStatsDoc(t, s.DB, 1, "sha_b", "b", inbox, false, 0)

	code, body := doBulkEdit(t, s,
		map[string]any{"documents": []int64{a, b}, "method": "trash"},
		adminPrincipal(1))
	if code != 200 {
		t.Fatalf("status=%d", code)
	}
	if body.Applied != 2 || body.Total != 2 {
		t.Errorf("applied=%d total=%d, want 2/2 (body=%+v)", body.Applied, body.Total, body)
	}
	// Both docs must now be trashed.
	var trashed int
	_ = d.Read.QueryRow(
		`SELECT COUNT(*) FROM documents WHERE id IN (?,?) AND trashed_at IS NOT NULL`,
		a, b).Scan(&trashed)
	if trashed != 2 {
		t.Errorf("trashed count = %d, want 2", trashed)
	}
	// Exactly one audit row, not two.
	var n int
	_ = d.Read.QueryRow(
		`SELECT COUNT(*) FROM audit_events WHERE action = 'documents.bulk_edit'`).Scan(&n)
	if n != 1 {
		t.Errorf("bulk_edit audit rows = %d, want 1", n)
	}
}

func TestBulkRestoreRejectsExpiredDocumentsAtomically(t *testing.T) {
	s := newBulkServer(t)
	inbox := seedStatsJDInbox(t, s.DB)
	recoverable := seedStatsDoc(t, s.DB, 1, "recoverable-sha", "Recoverable", inbox, false, 0)
	expired := seedStatsDoc(t, s.DB, 1, "expired-sha", "Expired", inbox, false, 0)
	now := time.Now().Unix()
	if _, err := s.DB.Write.Exec(`UPDATE documents SET trashed_at = CASE id WHEN ? THEN ? ELSE ? END
		WHERE id IN (?, ?)`, recoverable, now, time.Now().Add(-trash.Retention-time.Hour).Unix(), recoverable, expired); err != nil {
		t.Fatal(err)
	}
	code, _ := doBulkEdit(t, s, map[string]any{
		"documents": []int64{recoverable, expired}, "method": "restore",
	}, adminPrincipal(1))
	if code != http.StatusBadRequest {
		t.Fatalf("expired batch status=%d, want 400", code)
	}
	var stillTrashed, audits int
	if err := s.DB.Read.QueryRow(`SELECT COUNT(*) FROM documents WHERE id IN (?, ?) AND trashed_at IS NOT NULL`, recoverable, expired).Scan(&stillTrashed); err != nil {
		t.Fatal(err)
	}
	if err := s.DB.Read.QueryRow(`SELECT COUNT(*) FROM audit_events WHERE action='documents.bulk_edit'`).Scan(&audits); err != nil {
		t.Fatal(err)
	}
	if stillTrashed != 2 || audits != 0 {
		t.Fatalf("failed restore changed state: trashed=%d audits=%d", stillTrashed, audits)
	}
	code, response := doBulkEdit(t, s, map[string]any{
		"documents": []int64{recoverable}, "method": "restore",
	}, adminPrincipal(1))
	if code != http.StatusOK || response.Applied != 1 {
		t.Fatalf("recoverable restore status=%d response=%+v", code, response)
	}
}

func TestBulkEditAddTagTakesOwnershipOfClassifierReview(t *testing.T) {
	s := newBulkServer(t)
	docID := seedStatsDoc(t, s.DB, 1, "review-sha", "Review", seedStatsJDInbox(t, s.DB), false, 0)
	if _, err := s.DB.Write.ExecContext(context.Background(), `
		INSERT INTO tags(system_id, id, name, slug, created_at, updated_at)
		VALUES (1, 99, 'needs-review', 'needs-review', 0, 0);
		INSERT INTO document_tags(document_id, tag_id, classifier_owned) VALUES (?, 99, 1)
	`, docID); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		code, body := doBulkEdit(t, s, map[string]any{
			"documents": []int64{docID}, "method": "add_tag",
			"parameters": map[string]any{"tag_id": 99},
		}, adminPrincipal(1))
		if code != 200 || body.Applied != 1 {
			t.Fatalf("status=%d body=%+v", code, body)
		}
	}
	var count, owned int
	if err := s.DB.Read.QueryRow(`
		SELECT COUNT(*), SUM(classifier_owned) FROM document_tags WHERE document_id = ?
	`, docID).Scan(&count, &owned); err != nil {
		t.Fatal(err)
	}
	if count != 1 || owned != 0 {
		t.Fatalf("tags=%d classifier_owned=%d, want 1/0", count, owned)
	}
}

func TestPatchDocumentClearsOnlyClassifierReviewTag(t *testing.T) {
	s := newBulkServer(t)
	category := seedStatsJDInbox(t, s.DB)
	automatic := seedStatsDoc(t, s.DB, 1, "review-auto-sha", "Automatic", category, false, 0)
	manual := seedStatsDoc(t, s.DB, 1, "review-manual-sha", "Manual", category, false, 0)
	if _, err := s.DB.Write.ExecContext(context.Background(), `
		INSERT INTO tags(system_id, id, name, slug, created_at, updated_at)
		VALUES (1, 99, 'needs-review', 'needs-review', 0, 0);
		INSERT INTO document_tags(document_id, tag_id, classifier_owned)
		VALUES (?, 99, 1), (?, 99, 0)
	`, automatic, manual); err != nil {
		t.Fatal(err)
	}
	for _, id := range []int64{automatic, manual} {
		req := httptest.NewRequest(http.MethodPatch, "/api/documents/"+strconv.FormatInt(id, 10),
			bytes.NewBufferString(`{"title":"Reviewed"}`)).
			WithContext(auth.WithPrincipal(context.Background(), adminPrincipal(1)))
		req.SetPathValue("id", strconv.FormatInt(id, 10))
		rec := httptest.NewRecorder()
		s.PatchDocument(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("patch document %d: status=%d body=%s", id, rec.Code, rec.Body.String())
		}
	}
	var remaining, owned int64
	if err := s.DB.Read.QueryRow(`
		SELECT document_id, classifier_owned FROM document_tags WHERE tag_id=99
	`).Scan(&remaining, &owned); err != nil {
		t.Fatal(err)
	}
	if remaining != manual || owned != 0 {
		t.Fatalf("review tag remains on document %d with classifier_owned=%d, want only manual document %d", remaining, owned, manual)
	}
}

func TestBulkMetadataAndTagEditsClearOnlyClassifierReviewTag(t *testing.T) {
	s := newBulkServer(t)
	category := seedStatsJDInbox(t, s.DB)
	metadata := seedStatsDoc(t, s.DB, 1, "bulk-review-metadata", "Metadata", category, false, 0)
	tagged := seedStatsDoc(t, s.DB, 1, "bulk-review-tagged", "Tagged", category, false, 0)
	manual := seedStatsDoc(t, s.DB, 1, "bulk-review-manual", "Manual", category, false, 0)
	if _, err := s.DB.Write.ExecContext(context.Background(), `
		INSERT INTO tags(system_id, id, name, slug, created_at, updated_at)
		VALUES (1, 99, 'needs-review', 'needs-review', 0, 0),
		       (1, 100, 'Travel', 'travel', 0, 0);
		INSERT INTO document_tags(document_id, tag_id, classifier_owned)
		VALUES (?, 99, 1), (?, 99, 1), (?, 99, 0)
	`, metadata, tagged, manual); err != nil {
		t.Fatal(err)
	}
	for _, edit := range []struct {
		ids    []int64
		method string
		params map[string]any
	}{
		{[]int64{metadata, manual}, "set_sensitivity", map[string]any{"sensitivity": "restricted"}},
		{[]int64{tagged}, "add_tag", map[string]any{"tag_id": 100}},
	} {
		code, result := doBulkEdit(t, s, map[string]any{
			"documents": edit.ids, "method": edit.method, "parameters": edit.params,
		}, adminPrincipal(1))
		if code != http.StatusOK || result.Applied != len(edit.ids) {
			t.Fatalf("%s: status=%d response=%+v", edit.method, code, result)
		}
	}
	var remaining, owned int64
	if err := s.DB.Read.QueryRow(`
		SELECT document_id, classifier_owned FROM document_tags WHERE tag_id=99
	`).Scan(&remaining, &owned); err != nil {
		t.Fatal(err)
	}
	if remaining != manual || owned != 0 {
		t.Fatalf("review tag remains on document %d with classifier_owned=%d, want only manual document %d", remaining, owned, manual)
	}
}

func TestBulkEdit_TrashRequiresDeletePermission(t *testing.T) {
	s := newBulkServer(t)
	inbox := seedStatsJDInbox(t, s.DB)
	docID := seedStatsDoc(t, s.DB, 1, "shared-sha", "shared", inbox, false, 0)
	seedUser(t, s.DB, 2)
	if _, err := s.DB.Write.ExecContext(context.Background(), `
		INSERT INTO object_acls(
			object_kind, object_id, principal_kind, principal_id,
			perm_bits, created_at, created_by
		) VALUES ('document', ?, 'user', 2, ?, 0, 1)
	`, docID, int(authz.PermView|authz.PermChange)); err != nil {
		t.Fatal(err)
	}

	code, body := doBulkEdit(t, s,
		map[string]any{"documents": []int64{docID}, "method": "trash"},
		memberPrincipal(2))
	if code != 200 || body.Applied != 0 || body.Results[0].Code != "forbidden" {
		t.Fatalf("trash status=%d body=%+v", code, body)
	}
	code, body = doBulkEdit(t, s,
		map[string]any{
			"documents":  []int64{docID},
			"method":     "set_sensitivity",
			"parameters": map[string]any{"sensitivity": "internal"},
		}, memberPrincipal(2))
	if code != 200 || body.Applied != 1 {
		t.Fatalf("metadata edit status=%d body=%+v", code, body)
	}
}

func TestBulkEdit_AuthorizerErrorFailsRequest(t *testing.T) {
	s := newBulkServer(t)
	inbox := seedStatsJDInbox(t, s.DB)
	docID := seedStatsDoc(t, s.DB, 1, "auth-error-sha", "auth error", inbox, false, 0)
	if _, err := s.DB.Write.Exec(`UPDATE documents SET sensitivity='public' WHERE id=?`, docID); err != nil {
		t.Fatal(err)
	}
	s.Authz = authorizerFunc(func(context.Context, authz.Principal, authz.Kind, int64, authz.Perm) error {
		return errors.New("authorizer unavailable")
	})

	code, _ := doBulkEdit(t, s,
		map[string]any{
			"documents": []int64{docID},
			"method":    "set_sensitivity",
			"parameters": map[string]any{
				"sensitivity": "internal",
			},
		}, adminPrincipal(1))
	if code != http.StatusInternalServerError {
		t.Fatalf("status=%d, want 500", code)
	}
	var sensitivity string
	if err := s.DB.Read.QueryRow(`SELECT sensitivity FROM documents WHERE id=?`, docID).Scan(&sensitivity); err != nil {
		t.Fatal(err)
	}
	if sensitivity != "public" {
		t.Fatalf("authorization failure mutated document sensitivity to %q", sensitivity)
	}
}

func TestBulkEdit_SetJDCategory(t *testing.T) {
	s := newBulkServer(t)
	d := s.DB
	inbox := seedStatsJDInbox(t, s.DB)
	a := seedStatsDoc(t, s.DB, 1, "sha_a", "a", inbox, false, 0)

	// Seed a second JD category to move to.
	_, err := d.Write.ExecContext(context.Background(), `
		INSERT INTO jd_categories(system_id, id, area_start, code, name)
		VALUES (1, 2, 0, 2, 'Tax')`)
	if err != nil {
		t.Fatal(err)
	}

	code, body := doBulkEdit(t, s,
		map[string]any{
			"documents":  []int64{a},
			"method":     "set_jd_category",
			"parameters": map[string]any{"jd_category_id": 2},
		},
		adminPrincipal(1))
	if code != 200 || body.Applied != 1 {
		t.Fatalf("status=%d body=%+v", code, body)
	}
	var got int64
	_ = d.Read.QueryRow(`SELECT jd_category_id FROM documents WHERE id = ?`, a).Scan(&got)
	if got != 2 {
		t.Errorf("jd_category_id = %d, want 2", got)
	}
}

func TestBulkRemovingAbsentTagInvalidatesInFlightSuggestion(t *testing.T) {
	s := newBulkServer(t)
	docID := seedStatsDoc(t, s.DB, 1, "tag-intent-source", "Source", seedStatsJDInbox(t, s.DB), false, 0)
	if _, err := s.DB.Write.Exec(`INSERT INTO tags(system_id,id,name,slug,created_at,updated_at) VALUES(1,301,'Suggested','suggested',0,0)`); err != nil {
		t.Fatal(err)
	}
	baseline, err := documentstate.Load(t.Context(), s.DB.Read, docID)
	if err != nil {
		t.Fatal(err)
	}
	code, result := doBulkEdit(t, s, map[string]any{
		"documents": []int64{docID}, "method": "remove_tag",
		"parameters": map[string]any{"tag_id": 301},
	}, adminPrincipal(1))
	if code != http.StatusOK || result.Applied != 1 {
		t.Fatalf("remove absent tag: status=%d result=%+v", code, result)
	}
	err = s.DB.WriteTx(t.Context(), func(tx *sql.Tx) error {
		return approvals.ProposeDocumentChangeInTx(t.Context(), tx, docID, approvals.DocumentChange{
			Field: "tag", ValueID: 301, Confidence: 1, Source: "llm", Baseline: &baseline,
		})
	})
	if !errors.Is(err, approvals.ErrStaleProposal) {
		t.Fatalf("inference survived explicit removal: %v", err)
	}
}
