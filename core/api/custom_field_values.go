// Custom-field value writer and deleter endpoints. Per-document values
// have no read projection yet; future reads must recheck document-link
// target visibility. Typed validation lives in core/customfield.
//
// Endpoints:
//   PUT    /api/documents/{id}/custom_fields/{field}   body: {"value": <any>}
//   DELETE /api/documents/{id}/custom_fields/{field}
//
// {field} accepts the numeric field id OR the field name (URL-decoded).
// Trailing enqueue: any successful write enqueues a "render" job so
// storage-path templates that reference the field can update the
// symlink tree.

package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/johnnybravo-xyz/suchi/core/audit"
	"github.com/johnnybravo-xyz/suchi/core/auth"
	"github.com/johnnybravo-xyz/suchi/core/authz"
	"github.com/johnnybravo-xyz/suchi/core/customfield"
	"github.com/johnnybravo-xyz/suchi/core/render/view"
)

type setCustomFieldRequest struct {
	Value any `json:"value"`
}

// SetCustomField writes one typed custom-field value. Idempotent
// upsert — repeat calls with the same value are no-ops from the
// caller's perspective (the DB writes a fresh row but the render job
// dedupes to a no-op move).
func (s *Server) SetCustomField(w http.ResponseWriter, r *http.Request) {
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
	fieldRef := r.PathValue("field")
	if fieldRef == "" {
		s.writeError(w, http.StatusBadRequest, "bad_field", "field ref required")
		return
	}

	var req setCustomFieldRequest
	if err := decodeJSON(r, &req); err != nil {
		s.writeError(w, http.StatusBadRequest, "bad_body", "invalid JSON: "+err.Error())
		return
	}

	fieldID, dataType, extra, err := s.resolveField(r, fieldRef)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			s.writeError(w, http.StatusNotFound, "not_found", "no such custom field")
			return
		}
		s.writeError(w, http.StatusInternalServerError, "db_read", err.Error())
		return
	}

	handler := customfield.Lookup(dataType)
	typed, err := handler.Validate(extra, req.Value)
	if err != nil {
		s.writeError(w, http.StatusBadRequest, "bad_value", err.Error())
		return
	}

	err = s.DB.WriteTx(r.Context(), func(tx *sql.Tx) error {
		if ok, err := s.authorized(r.Context(), tx, p, authz.KindDocument, docID, authz.PermChange); err != nil {
			return err
		} else if !ok {
			return errForbidden
		}
		if dataType == "documentlink" {
			targetID := typed.(int64)
			if targetID != 0 {
				// Resolve the target intrinsically; preserve the token ceiling.
				targetContext := context.WithValue(r.Context(), systemContextKey{}, int64(0))
				if ok, err := s.authorized(targetContext, tx, p, authz.KindDocument, targetID, authz.PermView); err != nil {
					return err
				} else if !ok {
					return errForbidden
				}
			}
			if err := customfield.WriteDocumentLinkInTx(r.Context(), tx, docID, fieldID, targetID, true); err != nil {
				return err
			}
		} else if err := handler.Write(r.Context(), tx, docID, fieldID, typed); err != nil {
			return err
		}
		return view.EnqueueMove(r.Context(), tx, docID)
	})
	switch {
	case errors.Is(err, sql.ErrNoRows):
		s.writeError(w, http.StatusNotFound, "not_found", "document not found")
		return
	case errors.Is(err, errForbidden):
		s.writeError(w, http.StatusNotFound, "not_found", "document not found")
		return
	case err != nil:
		s.Log.Error("api.customfield.set", "err", err.Error())
		s.writeError(w, http.StatusInternalServerError, "db_write", err.Error())
		return
	}
	audit.Log(r.Context(), s.DB, s.Log, audit.Event{
		SystemID: selectedSystemID(r.Context()),
		Actor:    p, Action: "document.custom_field.set",
		ObjectKind: "document", ObjectID: docID,
		After: map[string]any{"field_id": fieldID, "data_type": dataType},
	})
	w.WriteHeader(http.StatusNoContent)
}

// DeleteCustomField removes the value row for a (doc, field) pair.
func (s *Server) DeleteCustomField(w http.ResponseWriter, r *http.Request) {
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
	fieldRef := r.PathValue("field")
	fieldID, _, _, err := s.resolveField(r, fieldRef)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			s.writeError(w, http.StatusNotFound, "not_found", "no such custom field")
			return
		}
		s.writeError(w, http.StatusInternalServerError, "db_read", err.Error())
		return
	}
	err = s.DB.WriteTx(r.Context(), func(tx *sql.Tx) error {
		if ok, err := s.authorized(r.Context(), tx, p, authz.KindDocument, docID, authz.PermChange); err != nil {
			return err
		} else if !ok {
			return errForbidden
		}
		if _, err := tx.ExecContext(r.Context(),
			`DELETE FROM document_custom_field_values WHERE document_id = ? AND field_id = ?`,
			docID, fieldID); err != nil {
			return err
		}
		return view.EnqueueMove(r.Context(), tx, docID)
	})
	switch {
	case errors.Is(err, sql.ErrNoRows):
		s.writeError(w, http.StatusNotFound, "not_found", "document not found")
	case errors.Is(err, errForbidden):
		s.writeError(w, http.StatusNotFound, "not_found", "document not found")
	case err != nil:
		s.Log.Error("api.customfield.delete", "err", err.Error())
		s.writeError(w, http.StatusInternalServerError, "db_write", err.Error())
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}

// resolveField accepts either a numeric field id or a field name and
// returns (id, data_type, extra_data JSON, err). Kept in one place so
// the two handlers agree on the resolution rules.
func (s *Server) resolveField(r *http.Request, ref string) (int64, string, json.RawMessage, error) {
	var (
		id       int64
		dataType string
		extra    string
	)
	if n, err := strconv.ParseInt(ref, 10, 64); err == nil {
		row := s.DB.Read.QueryRowContext(r.Context(),
			`SELECT id, data_type, extra_data FROM custom_fields WHERE system_id = ? AND id = ?`, selectedSystemID(r.Context()), n)
		if err := row.Scan(&id, &dataType, &extra); err != nil {
			return 0, "", nil, err
		}
		return id, dataType, json.RawMessage(extra), nil
	}
	// Name form.
	row := s.DB.Read.QueryRowContext(r.Context(),
		`SELECT id, data_type, extra_data FROM custom_fields WHERE system_id = ? AND name = ?`,
		selectedSystemID(r.Context()), strings.TrimSpace(ref))
	if err := row.Scan(&id, &dataType, &extra); err != nil {
		return 0, "", nil, err
	}
	return id, dataType, json.RawMessage(extra), nil
}
