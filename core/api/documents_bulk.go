// POST /api/documents/bulk_edit — one-shot metadata mutation across
// N documents in a single write transaction.
//
// Wire shape:
//
//	{ "documents": [1, 2, 3],
//	  "method": "set_correspondent" | "set_document_type" | "set_storage_path"
//	          | "add_tag" | "remove_tag" | "modify_tags"
//	          | "delete" | "restore"
//	          | "set_sensitivity" | "set_jd_category"
//	          | "rescan_enqueue",
//	  "parameters": { ... method-specific ... } }
//
// Every id is ACL-checked; the response array carries a per-id
// {id, ok, code?} so a partial-permission call surfaces which ids
// succeeded and which were refused. One audit_events row is written
// for the whole batch (documents.bulk_edit) with the count + method,
// not per doc — the response array carries the granular result.
//
// Method names use a bounded verb_object vocabulary so browser and external
// clients share one contract.

package api

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/johnnybravo-xyz/suchi/core/audit"
	"github.com/johnnybravo-xyz/suchi/core/auth"
	"github.com/johnnybravo-xyz/suchi/core/authz"
	"github.com/johnnybravo-xyz/suchi/core/rescan"
	"github.com/johnnybravo-xyz/suchi/core/trash"
)

// BulkEditRequest is the wire input.
type BulkEditRequest struct {
	Documents  []int64        `json:"documents"`
	Method     string         `json:"method"`
	Parameters map[string]any `json:"parameters,omitempty"`
}

// BulkEditItemResult is one row in the response array.
type BulkEditItemResult struct {
	ID   int64  `json:"id"`
	OK   bool   `json:"ok"`
	Code string `json:"code,omitempty"` // populated on failure
}

// BulkEditResponse is what /bulk_edit returns.
type BulkEditResponse struct {
	Method  string               `json:"method"`
	Total   int                  `json:"total"`
	Applied int                  `json:"applied"`
	Results []BulkEditItemResult `json:"results"`
}

// bulkEditMaxDocuments bounds the batch to keep one WriteTx from
// dominating the writer thread. 500 docs = ~500 UPDATE rows in one
// transaction — SQLite handles that in tens of ms.
const bulkEditMaxDocuments = 500

// BulkEdit serves POST /api/documents/bulk_edit. Requires
// documents:write. Owner + admin can see every doc's outcome; a
// non-admin caller sees only-visible ids succeed and the rest
// refused with code="forbidden".
func (s *Server) BulkEdit(w http.ResponseWriter, r *http.Request) {
	if !auth.RequireScope(w, r, auth.ScopeDocumentsWrite) {
		return
	}
	p := auth.FromContext(r.Context())
	systemID, ok := s.requireSystem(w, r, p)
	if !ok {
		return
	}

	var body BulkEditRequest
	if err := decodeJSON(r, &body); err != nil {
		s.writeError(w, http.StatusBadRequest, "bad_json", err.Error())
		return
	}
	if len(body.Documents) == 0 {
		s.writeError(w, http.StatusBadRequest, "no_documents",
			"documents[] must contain at least one id")
		return
	}
	if len(body.Documents) > bulkEditMaxDocuments {
		s.writeError(w, http.StatusRequestEntityTooLarge, "too_many_documents",
			fmt.Sprintf("bulk_edit accepts at most %d documents per call", bulkEditMaxDocuments))
		return
	}
	body.Method = strings.TrimSpace(body.Method)
	if body.Method == "" {
		s.writeError(w, http.StatusBadRequest, "missing_method", "method is required")
		return
	}

	// Build per-doc outcome shell up front so authorization refusals
	// keep the request→response id ordering.
	requiredPerm := authz.PermChange
	if body.Method == "trash" || body.Method == "delete" {
		requiredPerm = authz.PermDelete
	}
	decisions, err := s.documentPermissionDecisions(r.Context(), nil, p, body.Documents, requiredPerm)
	if err != nil {
		s.serverErr(w, "bulk_edit.authorize", err)
		return
	}
	results := make([]BulkEditItemResult, len(body.Documents))
	authorized := make([]int64, 0, len(body.Documents))
	for i, id := range body.Documents {
		results[i].ID = id
		if !decisions[id] {
			results[i].Code = "forbidden"
			continue
		}
		authorized = append(authorized, id)
	}

	// Execute the method in one WriteTx across the authorized set.
	// Mark each id ok=true after the tx commits — a per-id error
	// inside the tx aborts the whole batch by design; partial writes
	// would leave the archive in an unrecoverable half-state.
	err = s.DB.WriteTx(r.Context(), func(tx *sql.Tx) error {
		current, err := s.currentWriterPrincipal(r.Context(), tx, p, systemID)
		if err != nil {
			return err
		}
		decisions, err := s.documentPermissionDecisions(r.Context(), tx, current, authorized, requiredPerm)
		if err != nil {
			return err
		}
		for _, id := range authorized {
			if !decisions[id] {
				return errSystemUnavailable
			}
		}
		return s.applyBulkEdit(r, tx, systemID, body.Method, body.Parameters, authorized)
	})
	if err != nil {
		if errors.Is(err, errBadMethod) {
			s.writeError(w, http.StatusBadRequest, "bad_method",
				"unknown method "+body.Method)
			return
		}
		if errors.Is(err, errBadParams) {
			s.writeError(w, http.StatusBadRequest, "bad_parameters", err.Error())
			return
		}
		s.serverErr(w, "bulk_edit.apply", err)
		return
	}
	authorizedSet := map[int64]bool{}
	for _, id := range authorized {
		authorizedSet[id] = true
	}
	applied := 0
	for i := range results {
		if authorizedSet[results[i].ID] {
			results[i].OK = true
			applied++
		}
	}

	audit.Log(r.Context(), s.DB, s.Log, audit.Event{
		Actor: p, SystemID: systemID, Action: "documents.bulk_edit", ObjectKind: "documents",
		After: map[string]any{
			"method":  body.Method,
			"total":   len(body.Documents),
			"applied": applied,
		},
	})

	s.writeJSON(w, http.StatusOK, BulkEditResponse{
		Method: body.Method, Total: len(body.Documents),
		Applied: applied, Results: results,
	})
}

var (
	errBadMethod = errors.New("bulk_edit: unknown method")
	errBadParams = errors.New("bulk_edit: bad parameters")
)

// applyBulkEdit mutates only the IDs authorized by the caller's writer transaction.
func (s *Server) applyBulkEdit(r *http.Request, tx *sql.Tx, systemID int64, method string, params map[string]any, ids []int64) error {
	if len(ids) == 0 {
		return nil
	}
	now := time.Now().Unix()
	placeholders := strings.Repeat("?,", len(ids)-1) + "?"
	args := make([]any, 0, len(ids)+2)
	switch method {
	case "set_correspondent", "set_document_type", "set_storage_path", "set_jd_category":
		var column, table string
		switch method {
		case "set_correspondent":
			column, table = "correspondent_id", "correspondents"
		case "set_document_type":
			column, table = "document_type_id", "document_types"
		case "set_storage_path":
			column, table = "storage_path_id", "storage_paths"
		case "set_jd_category":
			column, table = "jd_category_id", "jd_categories"
		}
		value, err := paramInt64(params, column)
		if err != nil {
			return err
		}
		var target any
		if value != 0 {
			var exists bool
			if err := tx.QueryRowContext(r.Context(), "SELECT EXISTS (SELECT 1 FROM "+table+" WHERE id=? AND system_id=?)", value, systemID).Scan(&exists); err != nil {
				return err
			}
			if !exists {
				return fmt.Errorf("%w: %s is unavailable in this filing system", errBadParams, column)
			}
			target = value
		} else if method == "set_jd_category" {
			return fmt.Errorf("%w: a filing category is required", errBadParams)
		}
		args = append(args, target, now)
		for _, id := range ids {
			args = append(args, id)
		}
		_, err = tx.ExecContext(r.Context(), "UPDATE documents SET "+column+"=?, updated_at=? WHERE id IN ("+placeholders+")", args...)
		return err
	case "set_sensitivity":
		value, ok := params["sensitivity"].(string)
		if !ok || !SensitivityLevels[value] {
			return fmt.Errorf("%w: sensitivity must be one of the allowed levels", errBadParams)
		}
		var target any
		if value != "" {
			target = value
		}
		args = append(args, target, now)
		for _, id := range ids {
			args = append(args, id)
		}
		_, err := tx.ExecContext(r.Context(), "UPDATE documents SET sensitivity=?, updated_at=? WHERE id IN ("+placeholders+")", args...)
		return err
	case "add_tag", "remove_tag":
		value, err := paramInt64(params, "tag_id")
		if err != nil {
			return err
		}
		var exists bool
		if err := tx.QueryRowContext(r.Context(), "SELECT EXISTS (SELECT 1 FROM tags WHERE id=? AND system_id=?)", value, systemID).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			return fmt.Errorf("%w: tag is unavailable in this filing system", errBadParams)
		}
		if method == "add_tag" {
			for _, id := range ids {
				if _, err := tx.ExecContext(r.Context(), `INSERT INTO document_tags(document_id,tag_id) VALUES (?,?)
					ON CONFLICT(document_id,tag_id) DO UPDATE SET classifier_owned=0`, id, value); err != nil {
					return err
				}
			}
			return nil
		}
		args = append(args, value)
		for _, id := range ids {
			args = append(args, id)
		}
		_, err = tx.ExecContext(r.Context(), "DELETE FROM document_tags WHERE tag_id=? AND document_id IN ("+placeholders+")", args...)
		return err
	case "trash", "delete":
		args = append(args, now, now)
		for _, id := range ids {
			args = append(args, id)
		}
		_, err := tx.ExecContext(r.Context(), "UPDATE documents SET trashed_at=?, updated_at=? WHERE id IN ("+placeholders+") AND trashed_at IS NULL", args...)
		return err
	case "restore":
		cutoff := time.Now().Add(-trash.Retention).Unix()
		checkArgs := []any{cutoff}
		for _, id := range ids {
			checkArgs = append(checkArgs, id)
		}
		var expired bool
		if err := tx.QueryRowContext(r.Context(), "SELECT EXISTS (SELECT 1 FROM documents WHERE trashed_at <= ? AND id IN ("+placeholders+"))", checkArgs...).Scan(&expired); err != nil {
			return err
		}
		if expired {
			return fmt.Errorf("%w: a selected document's 30-day recovery window has expired", errBadParams)
		}
		args = append(args, now)
		for _, id := range ids {
			args = append(args, id)
		}
		_, err := tx.ExecContext(r.Context(), "UPDATE documents SET trashed_at=NULL, updated_at=? WHERE id IN ("+placeholders+") AND trashed_at IS NOT NULL", args...)
		return err
	case "rescan_enqueue":
		_, err := rescan.EnqueueInTx(r.Context(), tx, rescan.Options{SystemID: systemID, IDs: ids})
		return err
	}
	return errBadMethod
}

// paramInt64 coerces a JSON number or numeric string into int64. JSON numbers
// decode as float64 by default. Zero is allowed; it means
// "unset the FK" for set_* operations.
func paramInt64(params map[string]any, key string) (int64, error) {
	raw, ok := params[key]
	if !ok {
		return 0, fmt.Errorf("%w: parameters.%s is required", errBadParams, key)
	}
	switch v := raw.(type) {
	case json.Number:
		out, err := v.Int64()
		if err != nil {
			return 0, fmt.Errorf("%w: parameters.%s must be an integer", errBadParams, key)
		}
		return out, nil
	case int64:
		return v, nil
	case int:
		return int64(v), nil
	case string:
		out, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
		if err != nil {
			return 0, fmt.Errorf("%w: parameters.%s must parse as an integer", errBadParams, key)
		}
		return out, nil
	}
	return 0, fmt.Errorf("%w: parameters.%s type not supported", errBadParams, key)
}
