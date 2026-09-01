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
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	pluginapi "github.com/johnnybravo-xyz/suchi/plugin-api"

	"github.com/johnnybravo-xyz/suchi/core/auth"
	"github.com/johnnybravo-xyz/suchi/core/authz"
)

func newBulkServer(t *testing.T) *Server {
	t.Helper()
	d := openTestDB(t)
	return &Server{
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
	s.Authz = &recordingAuthorizer{errors: map[int64]error{
		docID: errors.New("authorizer unavailable"),
	}}

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
}

func TestBulkEdit_SetJDCategory(t *testing.T) {
	s := newBulkServer(t)
	d := s.DB
	inbox := seedStatsJDInbox(t, s.DB)
	a := seedStatsDoc(t, s.DB, 1, "sha_a", "a", inbox, false, 0)

	// Seed a second JD category to move to.
	_, err := d.Write.ExecContext(context.Background(), `
		INSERT INTO jd_categories(id, area_start, code, name)
		VALUES (2, 0, 2, 'Tax')`)
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
