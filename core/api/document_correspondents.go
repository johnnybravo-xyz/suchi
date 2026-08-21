package api

import (
	"database/sql"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/johnnybravo-xyz/suchi/core/audit"
	"github.com/johnnybravo-xyz/suchi/core/auth"
	"github.com/johnnybravo-xyz/suchi/core/authz"
	"github.com/johnnybravo-xyz/suchi/core/render/view"
	"github.com/johnnybravo-xyz/suchi/core/slug"
)

// Roles for document_correspondents. Kept as constants + a small
// closed vocabulary so the API and rules engine don't drift.
const (
	RoleSender    = "sender"
	RoleRecipient = "recipient"
	RoleCC        = "cc"
	RoleOther     = "other"
)

// DocCorrespondent is the JSON projection of one document_correspondents
// row plus the correspondent's name for the UI.
type DocCorrespondent struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
	Role string `json:"role"`
}

// AddDocCorrespondent — POST /api/documents/{id}/correspondents/. Body:
// {"name": "...", "role": "sender|recipient|cc|other"}. Upserts the
// correspondent by name and adds the junction row (or updates the
// primary FK when role=sender is the first sender for the doc, so
// existing code paths still see something reasonable).
func (s *Server) AddDocCorrespondent(w http.ResponseWriter, r *http.Request) {
	if !auth.RequireScope(w, r, auth.ScopeDocumentsWrite) {
		return
	}
	p := auth.FromContext(r.Context())
	docID, err := parseIDPath(r)
	if err != nil {
		s.writeError(w, http.StatusBadRequest, "bad_id", err.Error())
		return
	}
	if !s.authorize(w, r, p, authz.KindDocument, docID, authz.PermChange) {
		return
	}
	var req struct {
		Name string `json:"name"`
		Role string `json:"role"`
	}
	if err := decodeJSON(r, &req); err != nil {
		s.writeError(w, http.StatusBadRequest, "bad_body", err.Error())
		return
	}
	if req.Name == "" {
		s.writeError(w, http.StatusBadRequest, "missing_name", "name is required")
		return
	}
	if !validRole(req.Role) {
		s.writeError(w, http.StatusBadRequest, "bad_role",
			"role must be one of sender/recipient/cc/other")
		return
	}

	var corID int64
	err = s.DB.WriteTx(r.Context(), func(tx *sql.Tx) error {
		// Confirm doc still exists (authorize passed already but we
		// need to fail cleanly if a race trashed the doc between then
		// and here).
		var owner int64
		row := tx.QueryRowContext(r.Context(),
			`SELECT owner_id FROM documents WHERE id = ? AND trashed_at IS NULL`,
			docID)
		if err := row.Scan(&owner); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return errNotFound
			}
			return err
		}

		now := time.Now().Unix()
		// Upsert correspondent by name.
		if _, err := tx.ExecContext(r.Context(), `
			INSERT INTO correspondents(name, slug, created_at, updated_at)
			VALUES (?, ?, ?, ?)
			ON CONFLICT(name) DO UPDATE SET updated_at = excluded.updated_at
		`, req.Name, slug.Make(req.Name), now, now); err != nil {
			return err
		}
		if err := tx.QueryRowContext(r.Context(),
			`SELECT id FROM correspondents WHERE name = ?`, req.Name).Scan(&corID); err != nil {
			return err
		}
		// Junction row — no-op on duplicate role.
		if _, err := tx.ExecContext(r.Context(), `
			INSERT OR IGNORE INTO document_correspondents(document_id, correspondent_id, role)
			VALUES (?, ?, ?)
		`, docID, corID, req.Role); err != nil {
			return err
		}
		// If role=sender AND doc has no primary sender yet, mirror to
		// the FK so existing single-correspondent code paths still
		// find the sender.
		if req.Role == RoleSender {
			if _, err := tx.ExecContext(r.Context(), `
				UPDATE documents
				SET correspondent_id = ?, updated_at = ?
				WHERE id = ? AND correspondent_id IS NULL
			`, corID, now, docID); err != nil {
				return err
			}
		}
		// Enqueue a storage-path re-render — correspondent is a
		// template-visible field, so the symlink may need to move.
		return view.EnqueueMove(r.Context(), tx, docID)
	})
	switch {
	case errors.Is(err, errNotFound):
		s.writeError(w, http.StatusNotFound, "not_found", "document not found")
		return
	case errors.Is(err, errForbidden):
		s.writeError(w, http.StatusForbidden, "forbidden", "not this document's owner")
		return
	case err != nil:
		s.Log.Error("api.correspondents.add", "err", err.Error())
		s.writeError(w, http.StatusInternalServerError, "db_write", err.Error())
		return
	}
	audit.Log(r.Context(), s.DB, s.Log, audit.Event{
		Actor: p, Action: "document.correspondent.add",
		ObjectKind: "document", ObjectID: docID,
		After: map[string]any{"correspondent_id": corID, "role": req.Role},
	})
	s.writeJSON(w, http.StatusCreated,
		DocCorrespondent{ID: corID, Name: req.Name, Role: req.Role})
}

// ListDocCorrespondents — GET /api/documents/{id}/correspondents/.
func (s *Server) ListDocCorrespondents(w http.ResponseWriter, r *http.Request) {
	principal := auth.FromContext(r.Context())
	if principal == nil {
		s.writeError(w, http.StatusUnauthorized, "unauthorized", "auth required")
		return
	}
	docID, err := parseIDPath(r)
	if err != nil {
		s.writeError(w, http.StatusBadRequest, "bad_id", err.Error())
		return
	}
	if !s.authorize(w, r, principal, authz.KindDocument, docID, authz.PermView) {
		return
	}
	rows, err := s.DB.Read.QueryContext(r.Context(), `
		SELECT c.id, c.name, dc.role
		FROM document_correspondents dc
		JOIN correspondents c ON c.id = dc.correspondent_id
		WHERE dc.document_id = ?
		ORDER BY
			CASE dc.role
				WHEN 'sender'    THEN 0
				WHEN 'recipient' THEN 1
				WHEN 'cc'        THEN 2
				ELSE 3 END,
			dc.position, c.name
	`, docID)
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, "db_read", err.Error())
		return
	}
	defer rows.Close()
	var out []DocCorrespondent
	for rows.Next() {
		var c DocCorrespondent
		if err := rows.Scan(&c.ID, &c.Name, &c.Role); err != nil {
			s.writeError(w, http.StatusInternalServerError, "db_read", err.Error())
			return
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		s.writeError(w, http.StatusInternalServerError, "db_read", err.Error())
		return
	}
	if out == nil {
		out = []DocCorrespondent{}
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"results": out})
}

// RemoveDocCorrespondent — DELETE /api/documents/{id}/correspondents/{cid}/{role}.
// Removes exactly one junction row. Does NOT clear
// documents.correspondent_id — if you remove the sender, the primary
// FK stays as a historical reference until a new sender is added.
func (s *Server) RemoveDocCorrespondent(w http.ResponseWriter, r *http.Request) {
	if !auth.RequireScope(w, r, auth.ScopeDocumentsWrite) {
		return
	}
	p := auth.FromContext(r.Context())
	docID, err := parseIDPath(r)
	if err != nil {
		s.writeError(w, http.StatusBadRequest, "bad_id", err.Error())
		return
	}
	if !s.authorize(w, r, p, authz.KindDocument, docID, authz.PermChange) {
		return
	}
	cid, err := strconv.ParseInt(r.PathValue("cid"), 10, 64)
	if err != nil {
		s.writeError(w, http.StatusBadRequest, "bad_cid", "invalid correspondent id")
		return
	}
	role := r.PathValue("role")
	if !validRole(role) {
		s.writeError(w, http.StatusBadRequest, "bad_role", "invalid role")
		return
	}
	var affected int64
	err = s.DB.WriteTx(r.Context(), func(tx *sql.Tx) error {
		res, err := tx.ExecContext(r.Context(), `
			DELETE FROM document_correspondents
			WHERE document_id = ? AND correspondent_id = ? AND role = ?
		`, docID, cid, role)
		if err != nil {
			return err
		}
		affected, err = res.RowsAffected()
		if err != nil || affected == 0 {
			return err
		}
		return view.EnqueueMove(r.Context(), tx, docID)
	})
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, "db_write", err.Error())
		return
	}
	if affected == 0 {
		s.writeError(w, http.StatusNotFound, "not_found", "no such junction row")
		return
	}
	audit.Log(r.Context(), s.DB, s.Log, audit.Event{
		Actor: p, Action: "document.correspondent.remove",
		ObjectKind: "document", ObjectID: docID,
		Before: map[string]any{"correspondent_id": cid, "role": role},
	})
	w.WriteHeader(http.StatusNoContent)
}

// validRole guards the closed vocabulary at the API boundary; SQLite
// CHECK enforces it too, but a clean 400 beats the CHECK error text.
func validRole(r string) bool {
	switch r {
	case RoleSender, RoleRecipient, RoleCC, RoleOther:
		return true
	}
	return false
}

// Sentinels for the correspondent handlers — returned by the closure so
// the outer switch can pick the right HTTP status.
var (
	errNotFound  = errors.New("not_found")
	errForbidden = errors.New("forbidden")
)
