// GET /api/documents/{id}/similar — "documents like this" using FTS5
// more-like-this. Implementation lives in core/similar so the
// automations `apply_from_similar` action can share the same code
// path.
//
// Kept thin here: parse the query, authorize, hand off to
// similar.TopDocs, shape the envelope.

package api

import (
	"database/sql"
	"errors"
	"net/http"

	"github.com/johnnybravo-xyz/suchi/core/auth"
	"github.com/johnnybravo-xyz/suchi/core/authz"
	"github.com/johnnybravo-xyz/suchi/core/similar"
)

// SimilarResponse is the endpoint envelope.
type SimilarResponse struct {
	Results []similar.Doc `json:"results"`
	Method  string        `json:"method"` // "fts" today; "vec" when sqlite-vec ships
}

// GetSimilarDocuments serves GET /api/documents/{id}/similar?limit=10.
func (s *Server) GetSimilarDocuments(w http.ResponseWriter, r *http.Request) {
	if !auth.RequireScope(w, r, auth.ScopeDocumentsRead) {
		return
	}
	p := auth.FromContext(r.Context())
	id, err := parseIDPath(r)
	if err != nil {
		s.writeError(w, http.StatusBadRequest, "bad_id", err.Error())
		return
	}
	if !s.authorize(w, r, p, authz.KindDocument, id, authz.PermView) {
		return
	}

	limit := 10
	if q := r.URL.Query().Get("limit"); q != "" {
		if n, err := parseNonNegative(q); err == nil && n > 0 {
			if n > 50 {
				n = 50
			}
			limit = int(n)
		}
	}

	var sp *similar.Principal
	if p != nil {
		// Treat demo-anon as an admin-shaped principal for read
		// visibility — same rationale as documents_list.go.
		role := p.Role
		if p.Kind == "demo-anon" {
			role = "admin"
		}
		sp = &similar.Principal{UserID: p.UserID, Role: role}
		if role != "admin" {
			gs, err := s.principalGroups(r.Context(), p.UserID)
			if err != nil {
				s.serverErr(w, "similar.load_groups", err)
				return
			}
			sp.Groups = gs
		}
	}

	out, err := similar.TopDocs(r.Context(), s.DB, id, limit, sp)
	if errors.Is(err, sql.ErrNoRows) {
		s.writeError(w, http.StatusNotFound, "not_found", "document not found")
		return
	}
	if err != nil {
		s.serverErr(w, "similar.query", err)
		return
	}
	if out == nil {
		out = []similar.Doc{}
	}
	s.writeJSON(w, http.StatusOK, SimilarResponse{Results: out, Method: "fts"})
}
