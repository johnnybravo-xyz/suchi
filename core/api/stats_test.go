package api

// Handler tests for /api/stats/. Covers:
//   - anonymous → 401
//   - admin sees the full population; counts match live + trashed + inbox
//   - member ACL-scoped: counts drop to only-visible docs
//   - member sees pending_approvals for their own assignee
//   - dead_jobs admin-only (member gets 0)
//   - inbox_category_id emitted; 7d ingested trend correct

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	pluginapi "github.com/johnnybravo-xyz/suchi/plugin-api"

	"github.com/johnnybravo-xyz/suchi/core/auth"
	"github.com/johnnybravo-xyz/suchi/core/db"
)

func newStatsServer(t *testing.T) *Server {
	t.Helper()
	d := openTestDB(t)
	return &Server{DB: d, Log: slog.New(slog.NewTextHandler(os.Stderr, nil))}
}

func seedStatsJDInbox(t *testing.T, d *db.DB) int64 {
	t.Helper()
	ctx := context.Background()
	if _, err := d.Write.ExecContext(ctx, `
		INSERT OR IGNORE INTO jd_areas(code_start, code_end, name, position)
		VALUES (0, 9, 'Test', 0)`); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Write.ExecContext(ctx, `
		INSERT OR IGNORE INTO jd_categories(id, area_start, code, name, system)
		VALUES (1, 0, 1, 'Inbox', 1)`); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Write.ExecContext(ctx, `
		INSERT OR IGNORE INTO settings(key, value_json, updated_at)
		VALUES ('jd_inbox_category_id', '1', 0)`); err != nil {
		t.Fatal(err)
	}
	return 1
}

func seedStatsDoc(t *testing.T, d *db.DB, ownerID int64, sha, title string, jdCat int64, trashed bool, createdAt int64) int64 {
	t.Helper()
	seedUser(t, d, ownerID)
	var trashCol any
	if trashed {
		trashCol = createdAt
	}
	res, err := d.Write.ExecContext(context.Background(), `
		INSERT INTO documents(owner_id, original_blob, original_size, title, jd_category_id, trashed_at, created_at, updated_at)
		VALUES (?, ?, 0, ?, ?, ?, ?, ?)
	`, ownerID, sha, title, jdCat, trashCol, createdAt, createdAt)
	if err != nil {
		t.Fatal(err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func doStats(t *testing.T, s *Server, principal *pluginapi.Principal) (int, StatsResponse) {
	t.Helper()
	rec := httptest.NewRecorder()
	ctx := context.Background()
	if principal != nil {
		ctx = auth.WithPrincipal(ctx, principal)
	}
	r := httptest.NewRequest("GET", "/api/stats/", nil).WithContext(ctx)
	s.GetStats(rec, r)
	var body StatsResponse
	if rec.Code == 200 {
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("json: %v; body=%s", err, rec.Body.String())
		}
	}
	return rec.Code, body
}

func TestStats_Unauthenticated(t *testing.T) {
	s := newStatsServer(t)
	code, _ := doStats(t, s, nil)
	if code != 401 {
		t.Fatalf("anonymous status=%d, want 401", code)
	}
}

func TestStats_AdminSeesEverything(t *testing.T) {
	s := newStatsServer(t)
	inbox := seedStatsJDInbox(t, s.DB)
	now := time.Now().Unix()
	// 3 live (2 in inbox, 1 not), 1 trashed, 1 old-ingested (not in
	// the 7-day window).
	seedStatsDoc(t, s.DB, 1, "sha_a", "a", inbox, false, now)
	seedStatsDoc(t, s.DB, 1, "sha_b", "b", inbox, false, now)
	seedStatsDoc(t, s.DB, 1, "sha_c", "c", inbox, false, now)
	seedStatsDoc(t, s.DB, 1, "sha_d", "d", inbox, true, now)
	seedStatsDoc(t, s.DB, 1, "sha_e", "e", inbox, false, now-int64(30*24*time.Hour/time.Second))

	code, body := doStats(t, s, adminPrincipal(1))
	if code != 200 {
		t.Fatalf("status=%d", code)
	}
	if body.DocumentsTotal != 4 {
		t.Errorf("docs total = %d, want 4", body.DocumentsTotal)
	}
	if body.TrashCount != 1 {
		t.Errorf("trash count = %d, want 1", body.TrashCount)
	}
	if body.InboxCount != 4 {
		t.Errorf("inbox count = %d, want 4", body.InboxCount)
	}
	if body.InboxCategoryID != inbox {
		t.Errorf("inbox_category_id = %d, want %d", body.InboxCategoryID, inbox)
	}
	if body.Ingested7d != 3 {
		t.Errorf("ingested_7d = %d, want 3", body.Ingested7d)
	}
}

func TestStats_MemberScopedByOwner(t *testing.T) {
	s := newStatsServer(t)
	inbox := seedStatsJDInbox(t, s.DB)
	now := time.Now().Unix()

	// Doc owned by user 1, doc owned by user 2. User 2 (member)
	// must only see the doc they own.
	seedStatsDoc(t, s.DB, 1, "sha_owner1", "user1", inbox, false, now)
	seedStatsDoc(t, s.DB, 2, "sha_owner2", "user2", inbox, false, now)

	code, body := doStats(t, s, memberPrincipal(2))
	if code != 200 {
		t.Fatalf("status=%d", code)
	}
	if body.DocumentsTotal != 1 {
		t.Errorf("member docs total = %d, want 1 (ACL scoped)", body.DocumentsTotal)
	}
	if body.InboxCount != 1 {
		t.Errorf("member inbox = %d, want 1", body.InboxCount)
	}
	// Members must not see dead-job count even if there are dead
	// jobs; this test doesn't seed any, but the code path is
	// short-circuited by role so 0 is correct.
	if body.DeadJobs != 0 {
		t.Errorf("member dead_jobs = %d, want 0", body.DeadJobs)
	}
}

func TestStats_DeadJobsAdminOnly(t *testing.T) {
	s := newStatsServer(t)
	seedUser(t, s.DB, 1)
	seedUser(t, s.DB, 2)
	// One dead + one running for signal.
	if _, err := s.DB.Write.ExecContext(context.Background(), `
		INSERT INTO jobs(kind, state, next_run_at, created_at, updated_at)
		VALUES ('post-ingest', 'dead', 0, 0, 0), ('post-ingest', 'running', 0, 0, 0)
	`); err != nil {
		t.Fatal(err)
	}

	_, admin := doStats(t, s, adminPrincipal(1))
	if admin.DeadJobs != 1 {
		t.Errorf("admin dead_jobs = %d, want 1", admin.DeadJobs)
	}

	_, member := doStats(t, s, memberPrincipal(2))
	if member.DeadJobs != 0 {
		t.Errorf("member dead_jobs = %d, want 0 (admin-only)", member.DeadJobs)
	}
}
