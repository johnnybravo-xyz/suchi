// document_proposals endpoints — Tasks-inbox surface for the
// "Auto-file from archive" built-in automation.
//
//	GET  /api/documents/{id}/proposals                       list pending for one doc
//	POST /api/documents/{id}/proposals/{proposal_id}/resolve apply | reject one
//	POST /api/proposals/resolve_bulk                         apply | reject N in one tx (SPA bulk bar)
//
// The action layer inserts document_proposals rows when confidence
// falls in the propose tier. These endpoints let the operator (or
// the Tasks-inbox aggregator in tasks.go) drain the queue.

package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	pluginapi "github.com/johnnybravo-xyz/suchi/plugin-api"

	"github.com/johnnybravo-xyz/suchi/core/audit"
	"github.com/johnnybravo-xyz/suchi/core/auth"
	"github.com/johnnybravo-xyz/suchi/core/authz"
	"github.com/johnnybravo-xyz/suchi/core/db"
)

// ProposalRow is one pending proposal — the shape Tasks.svelte
// renders as a chip in the "based on N similar docs" card.
type ProposalRow struct {
	ID         int64   `json:"id"`
	DocumentID int64   `json:"document_id"`
	Field      string  `json:"field"` // "jd_category" | "correspondent" | "document_type" | "tag"
	ValueID    int64   `json:"value_id,omitempty"`
	Label      string  `json:"label"`
	Supporters []int64 `json:"supporters"`
	Confidence float64 `json:"confidence"`
	BasedOn    []int64 `json:"based_on"`
	CreatedAt  int64   `json:"created_at"`
}

// ListDocumentProposals — GET /api/documents/{id}/proposals.
// Returns pending proposals for the doc, owner + ACL:view scoped.
func (s *Server) ListDocumentProposals(w http.ResponseWriter, r *http.Request) {
	p := auth.FromContext(r.Context())
	if p == nil {
		s.writeError(w, http.StatusUnauthorized, "unauthorized", "auth required")
		return
	}
	docID, err := parseIDPath(r)
	if err != nil {
		s.writeError(w, http.StatusBadRequest, "bad_id", err.Error())
		return
	}
	if !s.authorize(w, r, p, authz.KindDocument, docID, authz.PermView) {
		return
	}
	rows, err := loadProposalsForDoc(r.Context(), s.DB, docID)
	if err != nil {
		s.serverErr(w, "proposals.list", err)
		return
	}
	if rows == nil {
		rows = []ProposalRow{}
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"results": rows})
}

// ResolveDocumentProposal — POST /api/documents/{id}/proposals/{proposal_id}/resolve.
// Body {action:"apply"|"reject"}.
func (s *Server) ResolveDocumentProposal(w http.ResponseWriter, r *http.Request) {
	p := auth.FromContext(r.Context())
	if p == nil {
		s.writeError(w, http.StatusUnauthorized, "unauthorized", "auth required")
		return
	}
	docID, err := parseIDPath(r)
	if err != nil {
		s.writeError(w, http.StatusBadRequest, "bad_id", err.Error())
		return
	}
	proposalID, err := strconv.ParseInt(r.PathValue("proposal_id"), 10, 64)
	if err != nil || proposalID <= 0 {
		s.writeError(w, http.StatusBadRequest, "bad_proposal_id", "proposal_id must be a positive integer")
		return
	}
	if !s.authorize(w, r, p, authz.KindDocument, docID, authz.PermChange) {
		return
	}
	var body struct {
		Action string `json:"action"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		s.writeError(w, http.StatusBadRequest, "bad_json", err.Error())
		return
	}
	if body.Action != "apply" && body.Action != "reject" {
		s.writeError(w, http.StatusBadRequest, "bad_action", "action must be 'apply' or 'reject'")
		return
	}
	result, err := s.resolveProposal(r.Context(), p, docID, proposalID, body.Action)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			s.writeError(w, http.StatusNotFound, "not_found", "proposal not found or already resolved")
			return
		}
		s.serverErr(w, "proposals.resolve", err)
		return
	}
	s.writeJSON(w, http.StatusOK, result)
}

// bulkResult is one entry in the resolve_bulk response array.
type bulkResult struct {
	ID   int64  `json:"id"`
	OK   bool   `json:"ok"`
	Code string `json:"code,omitempty"`
	Msg  string `json:"message,omitempty"`
}

// ResolveBulkProposals — POST /api/proposals/resolve_bulk.
// Body {proposal_ids:[..], action:"apply"|"reject"}. Per-id ACL
// check — forbidden entries come back as {id, ok:false,
// code:"forbidden"} while the rest apply. Cap 500 per call. This is
// what the SPA's bulk bar posts.
func (s *Server) ResolveBulkProposals(w http.ResponseWriter, r *http.Request) {
	p := auth.FromContext(r.Context())
	if p == nil {
		s.writeError(w, http.StatusUnauthorized, "unauthorized", "auth required")
		return
	}
	var body struct {
		ProposalIDs []int64 `json:"proposal_ids"`
		Action      string  `json:"action"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		s.writeError(w, http.StatusBadRequest, "bad_json", err.Error())
		return
	}
	if body.Action != "apply" && body.Action != "reject" {
		s.writeError(w, http.StatusBadRequest, "bad_action", "action must be 'apply' or 'reject'")
		return
	}
	if len(body.ProposalIDs) == 0 {
		s.writeError(w, http.StatusBadRequest, "no_ids", "proposal_ids is required")
		return
	}
	if len(body.ProposalIDs) > 500 {
		s.writeError(w, http.StatusBadRequest, "too_many", "cap 500 proposals per call")
		return
	}

	// Cache the caller's groups once for the per-id ACL check.
	groups, err := s.principalGroups(r.Context(), p.UserID)
	if err != nil {
		s.serverErr(w, "proposals.bulk.groups", err)
		return
	}

	results := make([]bulkResult, 0, len(body.ProposalIDs))
	for _, pid := range body.ProposalIDs {
		var docID int64
		var resolvedAt sql.NullInt64
		if err := s.DB.Read.QueryRowContext(r.Context(),
			`SELECT document_id, resolved_at FROM document_proposals WHERE id = ?`, pid,
		).Scan(&docID, &resolvedAt); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				results = append(results, bulkResult{ID: pid, Code: "not_found", Msg: "no such proposal"})
				continue
			}
			results = append(results, bulkResult{ID: pid, Code: "db_read", Msg: err.Error()})
			continue
		}
		if resolvedAt.Valid {
			results = append(results, bulkResult{ID: pid, Code: "already_resolved", Msg: "resolved earlier"})
			continue
		}
		if err := s.Authz.Can(r.Context(), authz.Principal{
			UserID: p.UserID, Role: p.Role, Groups: groups,
		}, authz.KindDocument, docID, authz.PermChange); err != nil {
			var denied *authz.ErrDenied
			if errors.As(err, &denied) {
				results = append(results, bulkResult{ID: pid, Code: "forbidden", Msg: "permission denied"})
				continue
			}
			results = append(results, bulkResult{ID: pid, Code: "authz", Msg: err.Error()})
			continue
		}
		if _, err := s.resolveProposal(r.Context(), p, docID, pid, body.Action); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				results = append(results, bulkResult{ID: pid, Code: "not_found", Msg: "resolved between check and apply"})
				continue
			}
			results = append(results, bulkResult{ID: pid, Code: "resolve", Msg: err.Error()})
			continue
		}
		results = append(results, bulkResult{ID: pid, OK: true})
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"results": results})
}

// resolveResult is the single-resolve response.
type resolveResult struct {
	ID     int64  `json:"id"`
	Action string `json:"action"`
	Field  string `json:"field,omitempty"`
}

// resolveProposal is the shared apply/reject implementation. Runs
// inside one WriteTx: load the row, apply the field (on "apply"),
// mark resolved, audit. Returns sql.ErrNoRows when the proposal was
// already resolved or removed.
func (s *Server) resolveProposal(ctx context.Context, p *pluginapi.Principal, docID, proposalID int64, action string) (*resolveResult, error) {
	var (
		field      string
		valueID    sql.NullInt64
		valueJSON  string
		confidence float64
	)
	if err := s.DB.Read.QueryRowContext(ctx, `
		SELECT field, value_id, value_json, confidence
		  FROM document_proposals
		 WHERE id = ? AND document_id = ? AND resolved_at IS NULL
	`, proposalID, docID).Scan(&field, &valueID, &valueJSON, &confidence); err != nil {
		return nil, err
	}

	err := s.DB.WriteTx(ctx, func(tx *sql.Tx) error {
		if action == "apply" {
			if err := applyProposalField(ctx, tx, docID, field, valueID.Int64, valueJSON); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE document_proposals
			   SET resolved_at = ?, resolution = ?
			 WHERE id = ? AND resolved_at IS NULL
		`, time.Now().Unix(), action+"d", proposalID); err != nil {
			return err
		}
		audit.LogInTx(ctx, tx, s.Log, audit.Event{
			Actor:      p,
			Action:     "heuristics." + action,
			ObjectKind: "document",
			ObjectID:   docID,
			After: map[string]any{
				"field":       field,
				"value_id":    valueID.Int64,
				"proposal_id": proposalID,
				"confidence":  confidence,
			},
		})
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &resolveResult{ID: proposalID, Action: action, Field: field}, nil
}

// applyProposalField is the "apply" write. Uses the same
// UPDATE-with-null-guard the automation action does — if a user
// filled the field between propose and apply, we keep their write.
// Title is the exception: the operator hitting Apply on a title
// proposal is deliberately overwriting whatever's there.
func applyProposalField(ctx context.Context, tx *sql.Tx, docID int64, field string, valueID int64, valueJSON string) error {
	now := time.Now().Unix()
	switch field {
	case "jd_category":
		_, err := tx.ExecContext(ctx,
			`UPDATE documents SET jd_category_id = ?, updated_at = ? WHERE id = ? AND jd_category_id IS NULL`,
			valueID, now, docID)
		return err
	case "correspondent":
		_, err := tx.ExecContext(ctx,
			`UPDATE documents SET correspondent_id = ?, updated_at = ? WHERE id = ? AND correspondent_id IS NULL`,
			valueID, now, docID)
		return err
	case "document_type":
		_, err := tx.ExecContext(ctx,
			`UPDATE documents SET document_type_id = ?, updated_at = ? WHERE id = ? AND document_type_id IS NULL`,
			valueID, now, docID)
		return err
	case "tag":
		_, err := tx.ExecContext(ctx,
			`INSERT OR IGNORE INTO document_tags(document_id, tag_id) VALUES (?, ?)`,
			docID, valueID)
		return err
	case "title":
		var cache struct {
			Label string `json:"label"`
		}
		if err := json.Unmarshal([]byte(valueJSON), &cache); err != nil {
			return fmt.Errorf("proposals: title value_json: %w", err)
		}
		if cache.Label == "" {
			return errors.New("proposals: empty title label")
		}
		// Overwrite whatever's there — the operator's Apply click IS
		// the intent to replace. No null-guard.
		_, err := tx.ExecContext(ctx,
			`UPDATE documents SET title = ?, updated_at = ? WHERE id = ?`,
			cache.Label, now, docID)
		return err
	}
	return fmt.Errorf("proposals: unknown field %q", field)
}

// loadProposalsForDoc fetches the pending set for one doc, hydrating
// the value_json cache back into the wire shape.
func loadProposalsForDoc(ctx context.Context, d *db.DB, docID int64) ([]ProposalRow, error) {
	rows, err := d.Read.QueryContext(ctx, `
		SELECT id, document_id, field, COALESCE(value_id, 0), value_json,
		       confidence, based_on, created_at
		  FROM document_proposals
		 WHERE document_id = ? AND resolved_at IS NULL
		 ORDER BY confidence DESC, id
	`, docID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ProposalRow
	for rows.Next() {
		var p ProposalRow
		var valueJSON, basedOn string
		if err := rows.Scan(&p.ID, &p.DocumentID, &p.Field, &p.ValueID,
			&valueJSON, &p.Confidence, &basedOn, &p.CreatedAt); err != nil {
			return nil, err
		}
		var cache struct {
			Label      string  `json:"label"`
			Supporters []int64 `json:"supporters"`
		}
		if valueJSON != "" {
			_ = json.Unmarshal([]byte(valueJSON), &cache)
		}
		p.Label = cache.Label
		p.Supporters = cache.Supporters
		if basedOn != "" {
			_ = json.Unmarshal([]byte(basedOn), &p.BasedOn)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}
