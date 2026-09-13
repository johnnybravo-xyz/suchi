package api

import (
	"errors"
	"net/http"
	"time"

	"github.com/johnnybravo-xyz/suchi/core/auth"
	"github.com/johnnybravo-xyz/suchi/core/authz"
	"github.com/johnnybravo-xyz/suchi/core/logx"
	"github.com/johnnybravo-xyz/suchi/core/trash"
)

// TrashRow is the compact projection used by the Trash interface.
type TrashRow struct {
	ID        int64  `json:"id"`
	Title     string `json:"title"`
	MIME      string `json:"mime_type,omitempty"`
	Size      int64  `json:"original_size"`
	CreatedAt int64  `json:"created_at"`
	TrashedAt int64  `json:"trashed_at"`
	DeletesAt int64  `json:"deletes_at"`
}

// ListTrash returns the caller's recoverable documents. Members see their own
// Trash; administrators see every trashed document in the archive.
func (s *Server) ListTrash(w http.ResponseWriter, r *http.Request) {
	p := auth.FromContext(r.Context())
	if p == nil {
		s.writeError(w, http.StatusUnauthorized, "unauthorized", "auth required")
		return
	}
	systemID, ok := s.requireSystem(w, r, p)
	if !ok {
		return
	}
	where := "trashed_at IS NOT NULL AND system_id = ?"
	args := []any{systemID}
	if p.Role != "admin" {
		where += " AND owner_id = ?"
		args = append(args, p.UserID)
	}

	var total int
	if err := s.DB.Read.QueryRowContext(r.Context(),
		"SELECT COUNT(*) FROM documents WHERE "+where, args...).Scan(&total); err != nil {
		s.serverErr(w, "trash.count", err)
		return
	}
	pp := ParsePageParams(r, 50, 200)
	query := `SELECT id, title, COALESCE(mime_type, ''), original_size,
	              created_at, COALESCE(trashed_at, 0)
	      FROM documents
	      WHERE ` + where + `
	      ORDER BY trashed_at DESC, id DESC
	      LIMIT ? OFFSET ?`
	args = append(args, pp.PageSize, pp.Offset())
	rows, err := s.DB.Read.QueryContext(r.Context(), query, args...)
	if err != nil {
		s.serverErr(w, "trash.list", err)
		return
	}
	defer rows.Close()
	var out []TrashRow
	for rows.Next() {
		var row TrashRow
		if err := rows.Scan(&row.ID, &row.Title, &row.MIME, &row.Size,
			&row.CreatedAt, &row.TrashedAt); err != nil {
			s.serverErr(w, "trash.scan", err)
			return
		}
		row.DeletesAt = row.TrashedAt + int64(trash.Retention/time.Second)
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		s.serverErr(w, "trash.iterate", err)
		return
	}
	if out == nil {
		out = []TrashRow{}
	}
	s.writeJSON(w, http.StatusOK, BuildEnvelope(r, total, pp, out))
}

// PurgeTrashDocument permanently deletes one document that is already in Trash.
func (s *Server) PurgeTrashDocument(w http.ResponseWriter, r *http.Request) {
	if !auth.RequireScope(w, r, auth.ScopeDocumentsWrite) {
		return
	}
	p := auth.FromContext(r.Context())
	id, err := parseIDPath(r)
	if err != nil {
		s.writeError(w, http.StatusBadRequest, "bad_id", err.Error())
		return
	}
	if !s.authorize(w, r, p, authz.KindDocument, id, authz.PermDelete) {
		return
	}
	if s.trash == nil {
		s.serverErr(w, "trash.unavailable", errors.New("trash service is not configured"))
		return
	}
	_, err = s.trash.PurgeOne(r.Context(), selectedSystemID(r.Context()), id, p, logx.RequestID(r.Context()))
	if errors.Is(err, trash.ErrNotTrashed) {
		s.writeError(w, http.StatusNotFound, "not_found", "no such trashed document")
		return
	}
	if err != nil {
		s.serverErr(w, "trash.purge", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// EmptyTrash permanently deletes every trashed document in the caller's Trash
// scope for which the caller currently has delete permission.
func (s *Server) EmptyTrash(w http.ResponseWriter, r *http.Request) {
	if !auth.RequireScope(w, r, auth.ScopeDocumentsWrite) {
		return
	}
	p := auth.FromContext(r.Context())
	systemID, ok := s.requireSystem(w, r, p)
	if !ok {
		return
	}
	if s.trash == nil {
		s.serverErr(w, "trash.unavailable", errors.New("trash service is not configured"))
		return
	}
	visibility, args, err := s.collectionVisibility(r.Context(), p)
	if err != nil {
		s.serverErr(w, "trash.visibility", err)
		return
	}
	rows, err := s.DB.Read.QueryContext(r.Context(), `SELECT d.id FROM documents d WHERE d.trashed_at IS NOT NULL AND `+visibility, args...)
	if err != nil {
		s.serverErr(w, "trash.ids", err)
		return
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			s.serverErr(w, "trash.ids", err)
			return
		}
		ids = append(ids, id)
	}
	if err := rows.Close(); err != nil {
		s.serverErr(w, "trash.ids", err)
		return
	}
	if err := rows.Err(); err != nil {
		s.serverErr(w, "trash.ids", err)
		return
	}
	decisions, err := s.documentPermissionDecisions(r.Context(), nil, p, ids, authz.PermDelete)
	if err != nil {
		s.serverErr(w, "trash.authorize", err)
		return
	}
	allowed := make([]int64, 0, len(ids))
	for _, id := range ids {
		if decisions[id] {
			allowed = append(allowed, id)
		}
	}
	report, err := s.trash.PurgeIDs(r.Context(), systemID, allowed, p, logx.RequestID(r.Context()))
	if err != nil {
		if errors.Is(err, trash.ErrNotTrashed) {
			s.writeError(w, http.StatusNotFound, "not_found", "document unavailable")
			return
		}
		s.serverErr(w, "trash.empty", err)
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"purged": report.Purged})
}
