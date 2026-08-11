// /api/stats/ — one-shot dashboard counters.
//
// Replaces the client-side composition of four page_size=1 list probes
// + two task lists. One round trip, one small handful of index-backed
// COUNT(*)s, honest per-principal visibility. The SPA drops its
// /inbox/i regex once the inbox_category_id field starts landing here.

package api

import (
	"context"
	"database/sql"
	"net/http"
	"strconv"

	"github.com/johnnybravo-xyz/suchi/core/auth"
	"github.com/johnnybravo-xyz/suchi/core/authz"
	"github.com/johnnybravo-xyz/suchi/core/jd"
)

// StatsResponse is the dashboard's one-round-trip payload. Every count
// is either owner-scoped (documents) or admin-scoped (jobs). Missing
// scope silently zeroes the operational counters — no 403 that would
// break a member's dashboard.
type StatsResponse struct {
	DocumentsTotal   int64 `json:"documents_total"`
	TrashCount       int64 `json:"trash_count"`
	InboxCount       int64 `json:"inbox_count"`
	InboxCategoryID  int64 `json:"inbox_category_id,omitempty"`
	PendingApprovals int64 `json:"pending_approvals"`
	DeadJobs         int64 `json:"dead_jobs"`
	Ingested7d       int64 `json:"ingested_7d"`
}

// GetStats serves GET /api/stats/. Requires documents:read.
func (s *Server) GetStats(w http.ResponseWriter, r *http.Request) {
	if !auth.RequireScope(w, r, auth.ScopeDocumentsRead) {
		return
	}
	p := auth.FromContext(r.Context())
	ctx := r.Context()
	// Demo-anon visitors see the same shared corpus as admins for
	// document read purposes (documents_list.go treats them the same
	// way). Without seeCorpus, the dashboard doc tiles report 0 while
	// the docs list shows 81.
	//
	// isAdmin stays strict: approvals + dead-jobs are admin-only
	// operational counters and demo-anon must not see the real admin's
	// pending queue.
	isAdmin := p.Role == "admin"
	seeCorpus := isAdmin || p.Kind == PrincipalKindDemoAnon

	groups, err := s.principalGroups(ctx, p.UserID)
	if err != nil {
		s.serverErr(w, "stats.load_groups", err)
		return
	}

	var out StatsResponse

	inbox, _ := jd.InboxCategoryID(ctx, s.DB)
	out.InboxCategoryID = inbox

	// Doc counts. Members splice the visibility WHERE onto every
	// document query; admins bypass it. The fragment references
	// alias `d` (see authz.DocVisibilityWhere).
	docWhere := "d.trashed_at IS NULL"
	trashWhere := "d.trashed_at IS NOT NULL"
	weekWhere := "d.trashed_at IS NULL AND d.created_at >= unixepoch('now', '-7 days')"
	inboxWhere := "d.jd_category_id = ? AND d.trashed_at IS NULL"

	var (
		docArgs, trashArgs, weekArgs []any
		inboxArgs                    = []any{inbox}
	)
	if !seeCorpus {
		vf, vargs := authz.DocVisibilityWhere(p.UserID, groups)
		docWhere += " AND " + vf
		trashWhere += " AND " + vf
		weekWhere += " AND " + vf
		inboxWhere += " AND " + vf
		docArgs = append(docArgs, vargs...)
		trashArgs = append(trashArgs, vargs...)
		weekArgs = append(weekArgs, vargs...)
		inboxArgs = append(inboxArgs, vargs...)
	}

	if err := scanCount(ctx, s.DB.Read,
		"SELECT COUNT(*) FROM documents d WHERE "+docWhere, docArgs, &out.DocumentsTotal); err != nil {
		s.serverErr(w, "stats.docs_total", err)
		return
	}
	if err := scanCount(ctx, s.DB.Read,
		"SELECT COUNT(*) FROM documents d WHERE "+trashWhere, trashArgs, &out.TrashCount); err != nil {
		s.serverErr(w, "stats.trash", err)
		return
	}
	if err := scanCount(ctx, s.DB.Read,
		"SELECT COUNT(*) FROM documents d WHERE "+weekWhere, weekArgs, &out.Ingested7d); err != nil {
		s.serverErr(w, "stats.ingested_7d", err)
		return
	}
	if inbox > 0 {
		if err := scanCount(ctx, s.DB.Read,
			"SELECT COUNT(*) FROM documents d WHERE "+inboxWhere, inboxArgs, &out.InboxCount); err != nil {
			s.serverErr(w, "stats.inbox", err)
			return
		}
	}

	// Approvals — the count MUST match what the Tasks page shows for
	// this caller. Same WHERE clause as approvalTasksForUser in
	// tasks.go: `user:<me>` always, plus `role:admin` when the caller
	// is admin. Otherwise a task assigned to another role/user bumps
	// the dashboard counter but never surfaces on the page the counter
	// links to — a classic "number won't stop nagging me" bug.
	me := "user:" + strconv.FormatInt(p.UserID, 10)
	if isAdmin {
		_ = s.DB.Read.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM approval_tasks
			 WHERE assignee IN (?, 'role:admin') AND status IN ('open','claimed')`,
			me).Scan(&out.PendingApprovals)
	} else {
		_ = s.DB.Read.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM approval_tasks WHERE assignee = ? AND status IN ('open','claimed')`,
			me).Scan(&out.PendingApprovals)
	}

	// Dead jobs — admin only. Members see 0 so the dashboard doesn't
	// leak "the archive is broken" to non-admins.
	if isAdmin {
		_ = s.DB.Read.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM jobs WHERE state='dead'`).Scan(&out.DeadJobs)
	}

	s.writeJSON(w, http.StatusOK, out)
}

// scanCount runs a scalar COUNT(*) query into out. Every counter on
// this endpoint uses the same shape so a bug in scan/args order fixes
// once, not per counter.
func scanCount(ctx context.Context, db *sql.DB, q string, args []any, out *int64) error {
	return db.QueryRowContext(ctx, q, args...).Scan(out)
}
