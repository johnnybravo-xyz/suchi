// /api/events/ — cursor-based activity feed over audit_events.
//
// The notification drawer in the SPA (and any agent that wants a
// change stream without polling documents-list) reads this endpoint
// with ?since_id=<id> to receive rows created after the last one it
// saw. Each row carries a pre-rendered summary so the client never
// needs to join names or handle content.
//
// Design notes:
//
//   - Backing store: audit_events. No new table. The PK is a
//     monotonic cursor by construction (SQLite INTEGER PRIMARY KEY
//     aliases ROWID, strictly increasing across inserts).
//
//   - Feed horizon = AUDIT_RETENTION_DAYS. Audit rows are pruned on
//     the backup ticker (default 20d, cap 100d). since_id remains a
//     valid cursor after a purge — IDs only grow — but a client that
//     comes back after the retention window has elapsed will see a
//     clean empty diff, not "the history it missed". Don't build a
//     full-history client on this endpoint; use `audit_events` via a
//     SIEM sink for that.
//
//   - Visibility: rows with object_kind='document' are filtered
//     through the same DocVisibilityWhere fragment the list endpoints
//     use — an ACL-restricted user can't see events about docs they
//     couldn't read. Rows with operational kinds (job.dead,
//     backup.written) require admin. Everything else is available to
//     any authed caller with events:read.
//
//   - Kind filter: ?kinds=a,b,c narrows the SELECT so a drawer that
//     only wants the operational tail (job.dead, document.ingested)
//     doesn't pay for every document.update the archive produces.
//     Unknown kinds simply return no rows (no error) so a client
//     rolling out a new kind name isn't blocked by a server upgrade.
//
//   - Summaries are built server-side because the client would
//     otherwise need a title lookup per row. The privacy invariant
//     from docs/privacy.mdx applies: summary contains titles and
//     kinds only, never document content or OCR text.
//
//   - Phase 2 (SSE) is not implemented here. When it lands, this
//     handler stays and the streaming version calls the same
//     projection helpers.

package api

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/johnnybravo-xyz/suchi/core/auth"
	"github.com/johnnybravo-xyz/suchi/core/authz"
)

// EventRow is one entry in the /api/events/ result set.
type EventRow struct {
	ID        int64  `json:"id"`
	Kind      string `json:"kind"`
	CreatedAt int64  `json:"created_at"`
	DocID     *int64 `json:"doc_id,omitempty"`
	Summary   string `json:"summary"`
}

// EventsResponse is the endpoint's envelope. Not the DRF pagination
// envelope because this is a cursor, not a page — count/next/prev
// don't apply. latest_id is the cursor the client saves so the next
// call gets only new rows.
type EventsResponse struct {
	Results  []EventRow `json:"results"`
	LatestID int64      `json:"latest_id"`
}

// operationalKinds are audit actions that reveal server-internal
// state (dead jobs, snapshot activity) and thus require admin.
// Extending this list is a security decision — see docs/api.mdx.
var operationalKinds = map[string]bool{
	"job.dead":       true,
	"backup.written": true,
}

// feedHiddenKinds are audit actions that get recorded (for the
// durable trail and operator observability via the raw table) but
// never surface in the notification feed — they're server-lifecycle
// or janitor noise, not user-actionable events. Hidden for every
// caller, admin or not; anyone chasing them has the audit_events
// table and structured logs.
var feedHiddenKinds = map[string]bool{
	"server.start":   true,
	"audit.pruned":   true,
	"jobs.reclaimed": true,
}

// docCentricKinds are events whose object_id points at a documents
// row — these get filtered through DocVisibilityWhere. Everything
// else (task_created for a workflow_task, share_link.*, etc.) is
// visible to any authed caller with events:read; we take the audit
// author's judgment on whether the surface was safe to record.
var docCentricKinds = map[string]bool{
	"document.create":               true,
	"document.restore":              true,
	"document.trash":                true,
	"document.update":               true,
	"document.upload.conflict":      true,
	"document.ingested":             true,
	"document.decrypt":              true,
	"document.correspondent.add":    true,
	"document.correspondent.remove": true,
	"document.custom_field.set":     true,
	"document.version.create":       true,
}

// ListEvents — GET /api/events/?since_id=<id>&kinds=a,b&limit=100.
//
// Empty since_id returns the tail of the log up to limit. Empty
// kinds returns every kind the caller is allowed to see.
func (s *Server) ListEvents(w http.ResponseWriter, r *http.Request) {
	if !auth.RequireScope(w, r, auth.ScopeEventsRead) {
		return
	}
	p := auth.FromContext(r.Context())
	// p is guaranteed non-nil here — RequireScope returns 401
	// otherwise.

	sinceID, err := parseNonNegative(r.URL.Query().Get("since_id"))
	if err != nil {
		s.writeError(w, http.StatusBadRequest, "bad_since_id",
			"since_id must be a non-negative integer")
		return
	}
	limit, err := parseNonNegative(r.URL.Query().Get("limit"))
	if err != nil || limit == 0 {
		limit = 100
	}
	if limit > 200 {
		limit = 200
	}

	requestedKinds := parseKindsCSV(r.URL.Query().Get("kinds"))
	isAdmin := p.Role == "admin"

	// Drop operational kinds from the filter list for non-admins and
	// drop feed-hidden kinds for everyone. A member who asks for
	// job.dead silently gets 0 rows for that kind — better than a 403
	// that breaks the whole drawer. Same treatment for server.start
	// and other lifecycle noise.
	kinds := make([]string, 0, len(requestedKinds))
	for _, k := range requestedKinds {
		if feedHiddenKinds[k] {
			continue
		}
		if operationalKinds[k] && !isAdmin {
			continue
		}
		kinds = append(kinds, k)
	}

	var whereKind string
	var kindArgs []any
	switch {
	case len(kinds) > 0:
		placeholders := strings.Repeat("?,", len(kinds)-1) + "?"
		whereKind = "AND action IN (" + placeholders + ")"
		for _, k := range kinds {
			kindArgs = append(kindArgs, k)
		}
	case len(requestedKinds) > 0:
		// Every requested kind was filtered out for lack of admin.
		s.writeJSON(w, http.StatusOK, EventsResponse{
			Results: []EventRow{}, LatestID: sinceID,
		})
		return
	default:
		// No include filter — build a NOT IN of the kinds this caller
		// isn't allowed / meant to see. Feed-hidden kinds are always
		// excluded; operational kinds are excluded for non-admins.
		hide := make([]string, 0, len(feedHiddenKinds)+len(operationalKinds))
		for k := range feedHiddenKinds {
			hide = append(hide, k)
		}
		if !isAdmin {
			for k := range operationalKinds {
				hide = append(hide, k)
			}
		}
		placeholders := strings.Repeat("?,", len(hide)-1) + "?"
		whereKind = "AND action NOT IN (" + placeholders + ")"
		for _, k := range hide {
			kindArgs = append(kindArgs, k)
		}
	}

	groups, err := s.principalGroups(r.Context(), p.UserID)
	if err != nil {
		s.serverErr(w, "events.load_groups", err)
		return
	}

	// Pull limit*2 candidate rows so the visibility filter can drop
	// some without emptying the response. Cap at 400 to keep the
	// read bounded.
	sqlLimit := limit * 2
	if sqlLimit > 400 {
		sqlLimit = 400
	}
	q := `SELECT id, ts, action, object_kind, object_id
	      FROM audit_events
	      WHERE id > ? ` + whereKind + `
	      ORDER BY id
	      LIMIT ?`
	args := append([]any{sinceID}, kindArgs...)
	args = append(args, sqlLimit)

	rows, err := s.DB.Read.QueryContext(r.Context(), q, args...)
	if err != nil {
		s.serverErr(w, "events.query", err)
		return
	}
	defer rows.Close()

	type rawRow struct {
		id, ts     int64
		action     string
		objectKind string
		objectID   sql.NullInt64
	}
	var raw []rawRow
	for rows.Next() {
		var r rawRow
		if err := rows.Scan(&r.id, &r.ts, &r.action, &r.objectKind, &r.objectID); err != nil {
			s.serverErr(w, "events.scan", err)
			return
		}
		raw = append(raw, r)
	}
	if err := rows.Err(); err != nil {
		s.serverErr(w, "events.rows", err)
		return
	}

	// Collect the doc IDs referenced by these rows to render summaries
	// with real titles. One SELECT per response, not per row.
	docIDs := make([]int64, 0, len(raw))
	seen := map[int64]bool{}
	for _, rr := range raw {
		if rr.objectKind == "document" && rr.objectID.Valid && !seen[rr.objectID.Int64] {
			seen[rr.objectID.Int64] = true
			docIDs = append(docIDs, rr.objectID.Int64)
		}
	}
	titles := loadDocTitles(r.Context(), s.DB.Read, docIDs)

	out := make([]EventRow, 0, limit)
	latestID := sinceID
	for _, rr := range raw {
		latestID = rr.id
		if docCentricKinds[rr.action] && rr.objectID.Valid {
			if !isAdmin && !s.visibleDoc(r.Context(), p.UserID, groups, rr.objectID.Int64) {
				continue
			}
		}
		row := EventRow{
			ID:        rr.id,
			Kind:      rr.action,
			CreatedAt: rr.ts,
			Summary:   renderSummary(rr.action, rr.objectID, titles),
		}
		if rr.objectKind == "document" && rr.objectID.Valid {
			id := rr.objectID.Int64
			row.DocID = &id
		}
		out = append(out, row)
		if int64(len(out)) >= limit {
			break
		}
	}
	s.writeJSON(w, http.StatusOK, EventsResponse{
		Results: out, LatestID: latestID,
	})
}

// visibleDoc runs the doc-visibility WHERE against one specific
// document id. Sub-millisecond at homelab scale — SQLite's read pool
// serves it from the WAL cache in almost every case.
func (s *Server) visibleDoc(ctx context.Context, userID int64, groups []int64, docID int64) bool {
	if userID == 0 {
		return false
	}
	frag, args := authz.DocVisibilityWhere(userID, groups)
	q := "SELECT 1 FROM documents d WHERE d.id = ? AND " + frag + " LIMIT 1"
	call := append([]any{docID}, args...)
	var one int
	err := s.DB.Read.QueryRowContext(ctx, q, call...).Scan(&one)
	return err == nil
}

// loadDocTitles batches a single SELECT over documents for every
// distinct object_id referenced by the current window. Missing docs
// (trashed hard-delete, wrong object_kind) simply aren't in the map;
// renderSummary falls back to "document #N".
func loadDocTitles(ctx context.Context, rdb *sql.DB, ids []int64) map[int64]string {
	if len(ids) == 0 {
		return nil
	}
	placeholders := strings.Repeat("?,", len(ids)-1) + "?"
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	rows, err := rdb.QueryContext(ctx,
		"SELECT id, title FROM documents WHERE id IN ("+placeholders+")", args...)
	if err != nil {
		return nil
	}
	defer rows.Close()
	out := make(map[int64]string, len(ids))
	for rows.Next() {
		var (
			id    int64
			title sql.NullString
		)
		if err := rows.Scan(&id, &title); err != nil {
			return out
		}
		out[id] = title.String
	}
	return out
}

// renderSummary produces the human-readable one-liner the drawer
// displays. Kept in one function so a new kind adds one case and the
// privacy invariant (no content, only titles/kinds) is enforceable
// by review.
func renderSummary(action string, objID sql.NullInt64, titles map[int64]string) string {
	docLabel := ""
	if objID.Valid {
		docLabel = titles[objID.Int64]
		if docLabel == "" {
			docLabel = fmt.Sprintf("document #%d", objID.Int64)
		}
	}
	switch action {
	case "document.create":
		return "Uploaded " + docLabel
	case "document.ingested":
		return "Ingested " + docLabel
	case "document.restore":
		return "Restored " + docLabel
	case "document.trash":
		return "Trashed " + docLabel
	case "document.update":
		return "Updated " + docLabel
	case "document.upload.conflict":
		return "Skipped duplicate of " + docLabel
	case "document.ingest.skipped":
		return "Skipped an ingest (see server log)"
	case "document.decrypt":
		return "Decrypted " + docLabel
	case "document.correspondent.add":
		return "Added correspondent on " + docLabel
	case "document.correspondent.remove":
		return "Removed correspondent from " + docLabel
	case "document.custom_field.set":
		return "Set a custom field on " + docLabel
	case "document.version.create":
		return "Uploaded a new version of " + docLabel
	case "approval.task_created":
		return "Approval requested"
	case "job.dead":
		return "A background job gave up after retries"
	case "backup.written":
		return "Wrote a database snapshot"
	default:
		return action
	}
}

func parseNonNegative(s string) (int64, error) {
	if s == "" {
		return 0, nil
	}
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, err
	}
	if v < 0 {
		return 0, fmt.Errorf("negative")
	}
	return v, nil
}

func parseKindsCSV(s string) []string {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	seen := map[string]bool{}
	for _, p := range parts {
		k := strings.TrimSpace(p)
		if k == "" || seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, k)
	}
	return out
}
