package api

// /api/events/ handler tests. Covers the load-bearing invariants:
//
//   - since_id acts as a cursor (only newer rows return).
//   - kinds= filters at the SQL layer.
//   - Members see documents they may view, assigned approvals, and
//     their own non-document actions; operational metadata stays private.
//   - Anonymous callers get 401.
//   - latest_id echoes the cursor even when no rows come back so a
//     poll loop can't spin.

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	pluginapi "github.com/johnnybravo-xyz/suchi/plugin-api"

	"github.com/johnnybravo-xyz/suchi/core/auth"
	"github.com/johnnybravo-xyz/suchi/core/db"
)

func newEventsServer(t *testing.T) *Server {
	t.Helper()
	d := openTestDB(t)
	return &Server{DB: d, Log: slog.New(slog.NewTextHandler(os.Stderr, nil))}
}

func seedAuditEvent(t *testing.T, d *db.DB, ts int64, action, objKind string, objID int64) int64 {
	t.Helper()
	res, err := d.Write.ExecContext(context.Background(), `
		INSERT INTO audit_events(ts, actor_kind, action, object_kind, object_id)
		VALUES (?, 'system', ?, ?, ?)
	`, ts, action, objKind, objID)
	if err != nil {
		t.Fatal(err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func seedUserAuditEvent(t *testing.T, d *db.DB, ts, userID int64, action, objKind string, objID int64) int64 {
	t.Helper()
	res, err := d.Write.ExecContext(context.Background(), `
		INSERT INTO audit_events(ts, actor_kind, actor_id, action, object_kind, object_id)
		VALUES (?, 'user', ?, ?, ?, ?)
	`, ts, userID, action, objKind, objID)
	if err != nil {
		t.Fatal(err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func seedEventsDoc(t *testing.T, d *db.DB, ownerID int64, title, sha string) int64 {
	t.Helper()
	// documents.owner_id + documents.jd_category_id have NOT NULL
	// FKs, so seed the caller and a placeholder JD row once. Idempotent
	// via INSERT OR IGNORE.
	seedUser(t, d, ownerID)
	ctx := context.Background()
	if _, err := d.Write.ExecContext(ctx, `
		INSERT OR IGNORE INTO jd_areas(code_start, code_end, name, position)
		VALUES (0, 9, 'Test', 0)
	`); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Write.ExecContext(ctx, `
		INSERT OR IGNORE INTO jd_categories(id, area_start, code, name, system)
		VALUES (1, 0, 1, 'Inbox', 1)
	`); err != nil {
		t.Fatal(err)
	}
	res, err := d.Write.ExecContext(ctx, `
		INSERT INTO documents(owner_id, original_blob, original_size, title, jd_category_id, created_at, updated_at)
		VALUES (?, ?, 0, ?, 1, 0, 0)
	`, ownerID, sha, title)
	if err != nil {
		t.Fatal(err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func doListEvents(t *testing.T, s *Server, path string, principal *pluginapi.Principal) (int, EventsResponse) {
	t.Helper()
	rec := httptest.NewRecorder()
	ctx := context.Background()
	if principal != nil {
		ctx = auth.WithPrincipal(ctx, principal)
	}
	r := httptest.NewRequest("GET", path, nil).WithContext(ctx)
	s.ListEvents(rec, r)
	var body EventsResponse
	if rec.Code == 200 {
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("json: %v; body=%s", err, rec.Body.String())
		}
	}
	return rec.Code, body
}

func TestListEvents_Unauthenticated(t *testing.T) {
	s := newEventsServer(t)
	code, _ := doListEvents(t, s, "/api/events/", nil)
	if code != 401 {
		t.Fatalf("anonymous should get 401, got %d", code)
	}
}

func TestListEvents_SinceIDCursor(t *testing.T) {
	s := newEventsServer(t)
	docID := seedEventsDoc(t, s.DB, 1, "ITR-1", "sha_itr")
	e1 := seedAuditEvent(t, s.DB, 100, "document.create", "document", docID)
	e2 := seedAuditEvent(t, s.DB, 200, "document.ingested", "document", docID)
	e3 := seedAuditEvent(t, s.DB, 300, "document.update", "document", docID)

	// No since_id returns the newest tail, in chronological order.
	code, body := doListEvents(t, s, "/api/events/?limit=2", adminPrincipal(1))
	if code != 200 {
		t.Fatalf("status=%d", code)
	}
	if len(body.Results) != 2 {
		t.Fatalf("want 2 rows, got %d", len(body.Results))
	}
	if body.Results[0].ID != e2 || body.Results[1].ID != e3 {
		t.Errorf("tail order=%v, want [%d %d]", body.Results, e2, e3)
	}
	if body.LatestID != e3 {
		t.Errorf("latest_id=%d, want %d", body.LatestID, e3)
	}

	// A cursor continues forwards rather than returning another tail.
	code, body = doListEvents(t, s, "/api/events/?limit=1&since_id="+itoa(e1), adminPrincipal(1))
	if code != 200 {
		t.Fatalf("status=%d", code)
	}
	if len(body.Results) != 1 || body.Results[0].ID != e2 {
		t.Errorf("cursor filter failed: %+v", body.Results)
	}

	// since_id=e3: empty, latest_id echoes the cursor.
	code, body = doListEvents(t, s, "/api/events/?since_id="+itoa(e3), adminPrincipal(1))
	if code != 200 {
		t.Fatalf("status=%d", code)
	}
	if len(body.Results) != 0 {
		t.Errorf("expected 0 rows, got %d", len(body.Results))
	}
	if body.LatestID != e3 {
		t.Errorf("empty response should echo cursor, got %d want %d", body.LatestID, e3)
	}
}

func TestListEvents_KindsFilter(t *testing.T) {
	s := newEventsServer(t)
	docID := seedEventsDoc(t, s.DB, 1, "x", "sha_x")
	_ = seedAuditEvent(t, s.DB, 100, "document.create", "document", docID)
	_ = seedAuditEvent(t, s.DB, 200, "document.update", "document", docID)
	_ = seedAuditEvent(t, s.DB, 300, "document.ingested", "document", docID)

	code, body := doListEvents(t, s, "/api/events/?kinds=document.ingested", adminPrincipal(1))
	if code != 200 {
		t.Fatalf("status=%d", code)
	}
	if len(body.Results) != 1 || body.Results[0].Kind != "document.ingested" {
		t.Errorf("kinds filter leaked: %+v", body.Results)
	}
}

func TestListEvents_OperationalKindsAdminOnly(t *testing.T) {
	s := newEventsServer(t)
	seedUser(t, s.DB, 1)
	seedUser(t, s.DB, 2)
	_ = seedAuditEvent(t, s.DB, 100, "job.dead", "job", 42)
	other := seedUserAuditEvent(t, s.DB, 101, 1, "tag.create", "tag", 1)
	own := seedUserAuditEvent(t, s.DB, 102, 2, "tag.create", "tag", 2)

	// Member asking explicitly for job.dead gets an empty set — the
	// kind is silently dropped from the filter.
	code, body := doListEvents(t, s,
		"/api/events/?kinds=job.dead", memberPrincipal(2))
	if code != 200 {
		t.Fatalf("member status=%d", code)
	}
	if len(body.Results) != 0 {
		t.Errorf("member saw operational kind: %+v", body.Results)
	}

	// Admin sees it.
	code, body = doListEvents(t, s, "/api/events/?kinds=job.dead", adminPrincipal(1))
	if code != 200 {
		t.Fatalf("admin status=%d", code)
	}
	if len(body.Results) != 1 || body.Results[0].Kind != "job.dead" {
		t.Errorf("admin missed operational kind: %+v", body.Results)
	}

	// Member with NO filter also doesn't see job.dead.
	code, body = doListEvents(t, s, "/api/events/", memberPrincipal(2))
	if code != 200 {
		t.Fatalf("member/nofilter status=%d", code)
	}
	for _, r := range body.Results {
		if r.Kind == "job.dead" {
			t.Errorf("member/nofilter leaked job.dead: %+v", r)
		}
		if r.ID == other {
			t.Errorf("member/nofilter leaked another user's event: %+v", r)
		}
	}
	if len(body.Results) != 1 || body.Results[0].ID != own {
		t.Errorf("member should see only their own non-document event: %+v", body.Results)
	}
}

func TestListEvents_FeedHiddenKinds(t *testing.T) {
	// server.start / audit.pruned / jobs.reclaimed are ops-log noise,
	// not user notifications — the drawer must skip them for every
	// caller, admin included, whether or not they're named in ?kinds=.
	s := newEventsServer(t)
	seedUser(t, s.DB, 2)
	_ = seedAuditEvent(t, s.DB, 100, "server.start", "server", 0)
	_ = seedAuditEvent(t, s.DB, 101, "audit.pruned", "audit", 0)
	_ = seedAuditEvent(t, s.DB, 102, "jobs.reclaimed", "job", 0)
	visibleID := seedUserAuditEvent(t, s.DB, 103, 2, "tag.create", "tag", 1)

	for _, who := range []struct {
		name string
		p    *pluginapi.Principal
	}{{"admin", adminPrincipal(1)}, {"member", memberPrincipal(2)}} {
		_, body := doListEvents(t, s, "/api/events/", who.p)
		for _, r := range body.Results {
			if feedHiddenKinds[r.Kind] {
				t.Errorf("%s leaked hidden kind %q in nofilter feed", who.name, r.Kind)
			}
		}
		var seen bool
		for _, r := range body.Results {
			if r.ID == visibleID {
				seen = true
			}
		}
		if !seen {
			t.Errorf("%s missing tag.create row in nofilter feed", who.name)
		}

		// Explicit ?kinds=server.start must return 0 even for admin.
		_, body = doListEvents(t, s, "/api/events/?kinds=server.start", who.p)
		if len(body.Results) != 0 {
			t.Errorf("%s got hidden kind via explicit filter: %+v", who.name, body.Results)
		}
	}
}

func TestListEvents_DocVisibility(t *testing.T) {
	s := newEventsServer(t)
	// Doc A owned by user 1, doc B owned by user 2. User 2 must
	// only see events about doc B.
	docA := seedEventsDoc(t, s.DB, 1, "user-1 secret", "sha_a")
	docB := seedEventsDoc(t, s.DB, 2, "user-2 doc", "sha_b")
	// An unlisted action must still follow the object kind's authorization.
	_ = seedAuditEvent(t, s.DB, 100, "heuristics.autoapply", "document", docA)
	eB := seedAuditEvent(t, s.DB, 200, "document.create", "document", docB)

	code, body := doListEvents(t, s, "/api/events/", memberPrincipal(2))
	if code != 200 {
		t.Fatalf("status=%d", code)
	}
	if len(body.Results) != 1 || body.Results[0].ID != eB {
		t.Errorf("visibility leaked: %+v", body.Results)
	}
	if !strings.Contains(body.Results[0].Summary, "user-2 doc") {
		t.Errorf("summary missing doc title: %q", body.Results[0].Summary)
	}
}

func TestListEvents_SummaryShape(t *testing.T) {
	s := newEventsServer(t)
	docID := seedEventsDoc(t, s.DB, 1, "March rent", "sha_r")
	seedUser(t, s.DB, 2)
	_, _, taskID := seedApprovalTask(t, s.DB, "user:2", "open")
	_ = seedAuditEvent(t, s.DB, 100, "document.ingested", "document", docID)
	_ = seedAuditEvent(t, s.DB, 200, "approval.task_created", "approval_task", taskID)

	_, body := doListEvents(t, s, "/api/events/", adminPrincipal(1))
	if len(body.Results) != 2 {
		t.Fatalf("want 2 rows, got %d", len(body.Results))
	}
	for _, r := range body.Results {
		if r.Summary == "" {
			t.Errorf("row %d missing summary", r.ID)
		}
	}
	// doc_id must be on the document row, not the task row.
	var docRow, taskRow EventRow
	for _, r := range body.Results {
		switch r.Kind {
		case "document.ingested":
			docRow = r
		case "approval.task_created":
			taskRow = r
		}
	}
	if docRow.DocID == nil || *docRow.DocID != docID {
		t.Errorf("document.ingested doc_id missing/wrong: %+v", docRow)
	}
	if taskRow.DocID != nil {
		t.Errorf("workflow_task shouldn't have doc_id set: %+v", taskRow)
	}
	if !strings.Contains(docRow.Summary, "March rent") {
		t.Errorf("summary missing title: %q", docRow.Summary)
	}

	// The assignee sees the approval but not another user's document.
	_, memberBody := doListEvents(t, s, "/api/events/", memberPrincipal(2))
	if len(memberBody.Results) != 1 || memberBody.Results[0].Kind != "approval.task_created" {
		t.Errorf("assignee feed=%+v, want only approval", memberBody.Results)
	}
	// The document owner sees their document event but not someone
	// else's approval.
	_, ownerBody := doListEvents(t, s, "/api/events/", memberPrincipal(1))
	if len(ownerBody.Results) != 1 || ownerBody.Results[0].Kind != "document.ingested" {
		t.Errorf("owner feed=%+v, want only document", ownerBody.Results)
	}
}

// ---------- helpers ----------

func adminPrincipal(id int64) *pluginapi.Principal {
	return &pluginapi.Principal{Kind: "user", UserID: id, Email: "a@example.com", Role: "admin"}
}

func memberPrincipal(id int64) *pluginapi.Principal {
	return &pluginapi.Principal{Kind: "user", UserID: id, Email: "m@example.com", Role: "member"}
}
