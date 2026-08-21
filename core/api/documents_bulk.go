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
	results := make([]BulkEditItemResult, len(body.Documents))
	authorized := make([]int64, 0, len(body.Documents))
	for i, id := range body.Documents {
		results[i].ID = id
		if !s.canBulkEditDoc(r, id, requiredPerm) {
			results[i].Code = "forbidden"
			continue
		}
		authorized = append(authorized, id)
	}

	// Execute the method in one WriteTx across the authorized set.
	// Mark each id ok=true after the tx commits — a per-id error
	// inside the tx aborts the whole batch by design; partial writes
	// would leave the archive in an unrecoverable half-state.
	err := s.applyBulkEdit(r, body.Method, body.Parameters, authorized)
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
		Actor: p, Action: "documents.bulk_edit", ObjectKind: "documents",
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

// canBulkEditDoc keeps the ACL rule in authorize while allowing each bulk
// method to select the same permission as its single-document counterpart.
func (s *Server) canBulkEditDoc(r *http.Request, id int64, perm authz.Perm) bool {
	return s.authorize(&discardResponseWriter{}, r,
		auth.FromContext(r.Context()), authz.KindDocument, id, perm)
}

// discardResponseWriter absorbs writes so authorize()'s deny-branch
// (which formats a 403 JSON) doesn't leak onto the real writer.
type discardResponseWriter struct{ hdr http.Header }

func (d *discardResponseWriter) Header() http.Header {
	if d.hdr == nil {
		d.hdr = http.Header{}
	}
	return d.hdr
}
func (d *discardResponseWriter) Write(b []byte) (int, error) { return len(b), nil }
func (d *discardResponseWriter) WriteHeader(int)             {}

var (
	errBadMethod = errors.New("bulk_edit: unknown method")
	errBadParams = errors.New("bulk_edit: bad parameters")
)

// applyBulkEdit dispatches on method and runs a single WriteTx. Adds
// a new method → one case here + one row in the docs. Params are
// pulled from the request map with type assertions; missing/wrong
// types return errBadParams with a targeted message.
func (s *Server) applyBulkEdit(r *http.Request, method string, params map[string]any, ids []int64) error {
	if len(ids) == 0 {
		// Every id was ACL-refused; commit is a no-op.
		return nil
	}
	now := time.Now().Unix()
	placeholders := strings.Repeat("?,", len(ids)-1) + "?"
	args := make([]any, 0, len(ids)+4)

	switch method {
	case "set_correspondent":
		v, err := paramInt64(params, "correspondent_id")
		if err != nil {
			return err
		}
		args = append(args, v, now)
		for _, id := range ids {
			args = append(args, id)
		}
		return s.DB.WriteTx(r.Context(), func(tx *sql.Tx) error {
			_, err := tx.ExecContext(r.Context(),
				"UPDATE documents SET correspondent_id = ?, updated_at = ? WHERE id IN ("+placeholders+")",
				args...)
			return err
		})
	case "set_document_type":
		v, err := paramInt64(params, "document_type_id")
		if err != nil {
			return err
		}
		args = append(args, v, now)
		for _, id := range ids {
			args = append(args, id)
		}
		return s.DB.WriteTx(r.Context(), func(tx *sql.Tx) error {
			_, err := tx.ExecContext(r.Context(),
				"UPDATE documents SET document_type_id = ?, updated_at = ? WHERE id IN ("+placeholders+")",
				args...)
			return err
		})
	case "set_storage_path":
		v, err := paramInt64(params, "storage_path_id")
		if err != nil {
			return err
		}
		args = append(args, v, now)
		for _, id := range ids {
			args = append(args, id)
		}
		return s.DB.WriteTx(r.Context(), func(tx *sql.Tx) error {
			_, err := tx.ExecContext(r.Context(),
				"UPDATE documents SET storage_path_id = ?, updated_at = ? WHERE id IN ("+placeholders+")",
				args...)
			return err
		})
	case "set_jd_category":
		v, err := paramInt64(params, "jd_category_id")
		if err != nil {
			return err
		}
		args = append(args, v, now)
		for _, id := range ids {
			args = append(args, id)
		}
		return s.DB.WriteTx(r.Context(), func(tx *sql.Tx) error {
			_, err := tx.ExecContext(r.Context(),
				"UPDATE documents SET jd_category_id = ?, updated_at = ? WHERE id IN ("+placeholders+")",
				args...)
			return err
		})
	case "set_sensitivity":
		v, ok := params["sensitivity"].(string)
		if !ok {
			return fmt.Errorf("%w: sensitivity must be a string", errBadParams)
		}
		if !SensitivityLevels[v] {
			return fmt.Errorf("%w: sensitivity %q not one of the allowed levels", errBadParams, v)
		}
		var val any
		if v == "" {
			val = sql.NullString{}
		} else {
			val = v
		}
		args = append(args, val, now)
		for _, id := range ids {
			args = append(args, id)
		}
		return s.DB.WriteTx(r.Context(), func(tx *sql.Tx) error {
			_, err := tx.ExecContext(r.Context(),
				"UPDATE documents SET sensitivity = ?, updated_at = ? WHERE id IN ("+placeholders+")",
				args...)
			return err
		})
	case "add_tag":
		v, err := paramInt64(params, "tag_id")
		if err != nil {
			return err
		}
		return s.DB.WriteTx(r.Context(), func(tx *sql.Tx) error {
			for _, id := range ids {
				if _, err := tx.ExecContext(r.Context(),
					"INSERT OR IGNORE INTO document_tags(document_id, tag_id) VALUES (?, ?)",
					id, v); err != nil {
					return err
				}
			}
			return nil
		})
	case "remove_tag":
		v, err := paramInt64(params, "tag_id")
		if err != nil {
			return err
		}
		return s.DB.WriteTx(r.Context(), func(tx *sql.Tx) error {
			args := []any{v}
			for _, id := range ids {
				args = append(args, id)
			}
			_, err := tx.ExecContext(r.Context(),
				"DELETE FROM document_tags WHERE tag_id = ? AND document_id IN ("+placeholders+")",
				args...)
			return err
		})
	case "trash", "delete":
		args = append(args, now, now)
		for _, id := range ids {
			args = append(args, id)
		}
		return s.DB.WriteTx(r.Context(), func(tx *sql.Tx) error {
			_, err := tx.ExecContext(r.Context(),
				"UPDATE documents SET trashed_at = ?, updated_at = ? WHERE id IN ("+placeholders+") AND trashed_at IS NULL",
				args...)
			return err
		})
	case "restore":
		args = append(args, now)
		for _, id := range ids {
			args = append(args, id)
		}
		return s.DB.WriteTx(r.Context(), func(tx *sql.Tx) error {
			_, err := tx.ExecContext(r.Context(),
				"UPDATE documents SET trashed_at = NULL, updated_at = ? WHERE id IN ("+placeholders+")",
				args...)
			return err
		})
	case "rescan_enqueue":
		// Re-run the content-extraction pipeline on the selected docs.
		// Shares the outbox path with the CLI (`suchi rescan`) and the
		// approvals-engine handler — one enqueue path, one place to
		// evolve the job payload. Filter pins to the authorized id list;
		// trashed_at IS NULL is applied inside Select so trashed picks
		// silently drop.
		_, err := rescan.Enqueue(r.Context(), s.DB, rescan.Options{IDs: ids})
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
