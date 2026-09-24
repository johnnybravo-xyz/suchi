// SPDX-License-Identifier: AGPL-3.0-or-later

package api

// CRUD roundtrip for POST/PATCH/DELETE /api/tags/. The list endpoint
// already has broader coverage; this pins the write verbs so a
// refactor of the shared helpers (slug.Make, isUniqueViolation)
// can't silently break them.

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"testing"

	pluginapi "github.com/johnnybravo-xyz/suchi/plugin-api"

	"github.com/johnnybravo-xyz/suchi/core/auth"
)

func TestTagCRUD_Roundtrip(t *testing.T) {
	d := openTestDB(t)
	seedUser(t, d, 1)
	s := &Server{DB: d, Log: slog.New(slog.NewTextHandler(os.Stderr, nil))}

	ctx := auth.WithPrincipal(context.Background(),
		&pluginapi.Principal{Kind: "user", UserID: 1, Email: "a@example.com", Role: "admin"})

	// Create.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/tags/",
		bytes.NewReader(mustJSON(t, map[string]any{"name": "tax-2026", "color": "#ff0000"}))).
		WithContext(ctx)
	s.CreateTag(rec, req)
	if rec.Code != 201 {
		t.Fatalf("create: status=%d body=%s", rec.Code, rec.Body.String())
	}
	var out struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || out.ID == 0 {
		t.Fatalf("create: bad response id: %v body=%s", err, rec.Body.String())
	}

	// Duplicate → 409.
	rec = httptest.NewRecorder()
	req = httptest.NewRequest("POST", "/api/tags/",
		bytes.NewReader(mustJSON(t, map[string]any{"name": "tax-2026"}))).WithContext(ctx)
	s.CreateTag(rec, req)
	if rec.Code != 409 {
		t.Fatalf("duplicate create: status=%d, want 409. body=%s", rec.Code, rec.Body.String())
	}

	// Update — rename.
	rec = httptest.NewRecorder()
	req = httptest.NewRequest("PATCH", "/api/tags/"+strconv.FormatInt(out.ID, 10),
		bytes.NewReader(mustJSON(t, map[string]any{"name": "tax-2027"}))).
		WithContext(ctx)
	req.SetPathValue("id", strconv.FormatInt(out.ID, 10))
	s.UpdateTag(rec, req)
	if rec.Code != 200 {
		t.Fatalf("update: status=%d body=%s", rec.Code, rec.Body.String())
	}

	// List — new name shows.
	rec = httptest.NewRecorder()
	req = httptest.NewRequest("GET", "/api/tags/", nil).WithContext(ctx)
	s.ListTags(rec, req)
	if rec.Code != 200 || !bytes.Contains(rec.Body.Bytes(), []byte(`"tax-2027"`)) {
		t.Errorf("list post-rename: status=%d body=%s", rec.Code, rec.Body.String())
	}

	// Delete.
	rec = httptest.NewRecorder()
	req = httptest.NewRequest("DELETE", "/api/tags/"+strconv.FormatInt(out.ID, 10), nil).WithContext(ctx)
	req.SetPathValue("id", strconv.FormatInt(out.ID, 10))
	s.DeleteTag(rec, req)
	if rec.Code != 204 {
		t.Fatalf("delete: status=%d body=%s", rec.Code, rec.Body.String())
	}

	// Delete again → 404.
	rec = httptest.NewRecorder()
	req = httptest.NewRequest("DELETE", "/api/tags/"+strconv.FormatInt(out.ID, 10), nil).WithContext(ctx)
	req.SetPathValue("id", strconv.FormatInt(out.ID, 10))
	s.DeleteTag(rec, req)
	if rec.Code != 404 {
		t.Errorf("delete-again: status=%d, want 404", rec.Code)
	}
}

func TestTagRenameTakesOwnershipOfClassifierReview(t *testing.T) {
	for _, field := range []string{"name", "slug", "color"} {
		t.Run(field, func(t *testing.T) {
			s := newBulkServer(t)
			docID := seedStatsDoc(t, s.DB, 1, "rename-review-sha", "Review", seedStatsJDInbox(t, s.DB), false, 0)
			if _, err := s.DB.Write.ExecContext(context.Background(), `
				INSERT INTO tags(system_id, id, name, slug, created_at, updated_at)
				VALUES (1, 99, 'needs-review', 'needs-review', 0, 0);
				INSERT INTO document_tags(document_id, tag_id, classifier_owned) VALUES (?, 99, 1)
			`, docID); err != nil {
				t.Fatal(err)
			}
			value, wantOwned := "review-myself", 0
			if field == "color" {
				value, wantOwned = "#ff0000", 1
			}
			ctx := auth.WithPrincipal(context.Background(), adminPrincipal(1))
			req := httptest.NewRequest("PATCH", "/api/tags/99",
				bytes.NewReader(mustJSON(t, map[string]any{field: value}))).WithContext(ctx)
			req.SetPathValue("id", "99")
			rec := httptest.NewRecorder()
			s.UpdateTag(rec, req)
			if rec.Code != 200 {
				t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
			}
			var owned int
			if err := s.DB.Read.QueryRow(`SELECT classifier_owned FROM document_tags WHERE document_id = ?`, docID).Scan(&owned); err != nil {
				t.Fatal(err)
			}
			if owned != wantOwned {
				t.Fatalf("classifier_owned=%d, want %d", owned, wantOwned)
			}
		})
	}
}

func TestCreateTag_MemberForbidden(t *testing.T) {
	d := openTestDB(t)
	s := &Server{DB: d, Log: slog.New(slog.NewTextHandler(os.Stderr, nil))}

	ctx := auth.WithPrincipal(context.Background(),
		&pluginapi.Principal{Kind: "user", UserID: 2, Email: "m@example.com", Role: "member"})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/tags/",
		bytes.NewReader(mustJSON(t, map[string]any{"name": "hidden"}))).WithContext(ctx)
	s.CreateTag(rec, req)
	if rec.Code != 403 {
		t.Errorf("member create: status=%d, want 403", rec.Code)
	}
}

func TestDeleteTagsAtomicAndReroots(t *testing.T) {
	d := openTestDB(t)
	seedUser(t, d, 1)
	seedUser(t, d, 2)
	if _, err := d.Write.Exec(`
		UPDATE users SET role='member' WHERE id=2;
		INSERT INTO jd_systems(id,code,name,taxonomy,created_at,updated_at)
			VALUES(2,'S02','Other archive','jd',0,0);
		INSERT INTO tags(id,system_id,name,slug,parent_id,created_at,updated_at) VALUES
			(10,1,'Parent','parent',NULL,0,0),
			(11,1,'Child','child',10,0,0),
			(12,1,'Grandchild','grandchild',11,0,0),
			(13,1,'Single parent','single-parent',NULL,0,0),
			(14,1,'Single child','single-child',13,0,0),
			(15,1,'Valid','valid',NULL,0,0),
			(30,2,'Foreign','foreign',NULL,0,0);
	`); err != nil {
		t.Fatal(err)
	}
	docID := seedStatsDoc(t, d, 1, "delete-tags-fixture", "Tagged", seedStatsJDInbox(t, d), false, 0)
	if _, err := d.Write.Exec(`INSERT INTO document_tags(document_id,tag_id) VALUES
		(?,10),(?,11),(?,12),(?,13),(?,14),(?,15)`,
		docID, docID, docID, docID, docID, docID); err != nil {
		t.Fatal(err)
	}
	s := &Server{DB: d, Log: slog.New(slog.NewTextHandler(os.Stderr, nil))}
	mux := http.NewServeMux()
	s.Register(mux)
	admin := &pluginapi.Principal{Kind: "user", UserID: 1, Role: "admin"}
	member := &pluginapi.Principal{Kind: "user", UserID: 2, Role: "member"}
	request := func(method, path, body string, p *pluginapi.Principal) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(method, path, bytes.NewBufferString(body))
		req = req.WithContext(auth.WithPrincipal(req.Context(), p))
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		return rec
	}
	assertState := func(tagCount, linkCount, batchAudits int) {
		t.Helper()
		for _, tc := range []struct {
			query string
			want  int
		}{
			{`SELECT COUNT(*) FROM tags WHERE id IN (10,11,12,13,14,15,30)`, tagCount},
			{`SELECT COUNT(*) FROM document_tags WHERE document_id = ?`, linkCount},
			{`SELECT COUNT(*) FROM audit_events WHERE action='tags.delete.batch'`, batchAudits},
		} {
			var got int
			var err error
			if tc.query == `SELECT COUNT(*) FROM document_tags WHERE document_id = ?` {
				err = d.Read.QueryRow(tc.query, docID).Scan(&got)
			} else {
				err = d.Read.QueryRow(tc.query).Scan(&got)
			}
			if err != nil || got != tc.want {
				t.Fatalf("%s: count=%d want=%d err=%v", tc.query, got, tc.want, err)
			}
		}
	}
	assertError := func(rec *httptest.ResponseRecorder, status int, code string) {
		t.Helper()
		var body struct {
			Code string `json:"code"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || rec.Code != status || body.Code != code {
			t.Fatalf("status=%d code=%q want=%d/%s body=%s err=%v",
				rec.Code, body.Code, status, code, rec.Body.String(), err)
		}
	}

	for _, tc := range []struct {
		name, body, code string
		status           int
		principal        *pluginapi.Principal
	}{
		{"member", `{"ids":[10]}`, "forbidden", 403, member},
		{"malformed", `{"ids":[`, "bad_json", 400, admin},
		{"missing ids", `{}`, "no_ids", 400, admin},
		{"empty ids", `{"ids":[]}`, "no_ids", 400, admin},
		{"duplicate", `{"ids":[10,10]}`, "bad_ids", 400, admin},
		{"zero", `{"ids":[0]}`, "bad_ids", 400, admin},
		{"negative", `{"ids":[-1]}`, "bad_ids", 400, admin},
		{"missing tag", `{"ids":[10,999]}`, "not_found", 404, admin},
		{"foreign tag", `{"ids":[10,30]}`, "not_found", 404, admin},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertError(request("DELETE", "/api/tags/", tc.body, tc.principal), tc.status, tc.code)
			assertState(7, 6, 0)
			var parent sql.NullInt64
			if err := d.Read.QueryRow(`SELECT parent_id FROM tags WHERE id=11`).Scan(&parent); err != nil || !parent.Valid || parent.Int64 != 10 {
				t.Fatalf("rejected deletion changed child parent=%v err=%v", parent, err)
			}
		})
	}
	tooMany := make([]int, 501)
	for i := range tooMany {
		tooMany[i] = i + 1
	}
	assertError(request("DELETE", "/api/tags/", string(mustJSON(t, map[string]any{"ids": tooMany})), admin), 413, "too_many_tags")
	assertState(7, 6, 0)
	if _, err := d.Write.Exec(`CREATE TRIGGER reject_tag_delete BEFORE DELETE ON tags
		WHEN OLD.id=11 BEGIN SELECT RAISE(ABORT, 'forced delete failure'); END`); err != nil {
		t.Fatal(err)
	}
	assertError(request("DELETE", "/api/tags/", `{"ids":[10,11]}`, admin), 500, "internal")
	assertState(7, 6, 0)
	var unchangedParent sql.NullInt64
	if err := d.Read.QueryRow(`SELECT parent_id FROM tags WHERE id=12`).Scan(&unchangedParent); err != nil || !unchangedParent.Valid || unchangedParent.Int64 != 11 {
		t.Fatalf("rolled-back detachment changed grandchild parent=%v err=%v", unchangedParent, err)
	}
	if _, err := d.Write.Exec(`DROP TRIGGER reject_tag_delete`); err != nil {
		t.Fatal(err)
	}

	rec := request("DELETE", "/api/tags/", `{"ids":[10,11]}`, admin)
	if rec.Code != 204 || rec.Body.Len() != 0 {
		t.Fatalf("batch delete status=%d body=%s", rec.Code, rec.Body.String())
	}
	assertState(5, 4, 1)
	var parent sql.NullInt64
	if err := d.Read.QueryRow(`SELECT parent_id FROM tags WHERE id=12`).Scan(&parent); err != nil || parent.Valid {
		t.Fatalf("grandchild must survive as root, parent=%v err=%v", parent, err)
	}
	var after string
	if err := d.Read.QueryRow(`SELECT after_json FROM audit_events WHERE action='tags.delete.batch'
		AND system_id=1 AND object_kind='tags'`).Scan(&after); err != nil {
		t.Fatal(err)
	}
	var auditPayload struct {
		IDs     []int64 `json:"ids"`
		Deleted int     `json:"deleted"`
	}
	if err := json.Unmarshal([]byte(after), &auditPayload); err != nil ||
		len(auditPayload.IDs) != 2 || auditPayload.IDs[0] != 10 ||
		auditPayload.IDs[1] != 11 || auditPayload.Deleted != 2 {
		t.Fatalf("batch audit after=%s err=%v", after, err)
	}
	rec = request("DELETE", "/api/tags/13", "", admin)
	if rec.Code != 204 {
		t.Fatalf("single delete status=%d body=%s", rec.Code, rec.Body.String())
	}
	if err := d.Read.QueryRow(`SELECT parent_id FROM tags WHERE id=14`).Scan(&parent); err != nil || parent.Valid {
		t.Fatalf("single delete lost child or failed to re-root it, parent=%v err=%v", parent, err)
	}
	assertState(4, 3, 1)
	var singleAudits int
	if err := d.Read.QueryRow(`SELECT COUNT(*) FROM audit_events WHERE action='tag.delete' AND object_id=13`).Scan(&singleAudits); err != nil || singleAudits != 1 {
		t.Fatalf("single delete audits=%d err=%v", singleAudits, err)
	}
	for _, id := range []int{10, 11, 13} {
		var links int
		if err := d.Read.QueryRow(`SELECT COUNT(*) FROM document_tags WHERE tag_id=?`, id).Scan(&links); err != nil || links != 0 {
			t.Fatalf("deleted tag %d links=%d err=%v", id, links, err)
		}
	}
}
