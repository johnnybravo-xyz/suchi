// SPDX-License-Identifier: AGPL-3.0-or-later

package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/johnnybravo-xyz/suchi/core/auth"
	"github.com/johnnybravo-xyz/suchi/core/authz"
	"github.com/johnnybravo-xyz/suchi/core/customfield"
	"github.com/johnnybravo-xyz/suchi/core/jd/systems"
	pluginapi "github.com/johnnybravo-xyz/suchi/plugin-api"
)

// DocumentReferenceSummary is the bounded exact-document projection used for
// outgoing document-link values and computed backlinks.
type DocumentReferenceSummary struct {
	ID             int64  `json:"id"`
	SystemCode     string `json:"system_code,omitempty"`
	JDAddress      string `json:"jd_address,omitempty"`
	Title          string `json:"title"`
	MIME           string `json:"mime_type,omitempty"`
	JDCategoryCode int64  `json:"jd_category_code,omitempty"`
	CreatedAt      int64  `json:"created_at"`
	IsLatest       bool   `json:"is_latest"`
}

// DocumentCustomFieldValue preserves the field's declared type. Document-link
// values contain a DocumentReferenceSummary instead of a raw target ID.
type DocumentCustomFieldValue struct {
	FieldID  int64  `json:"field_id"`
	Name     string `json:"name"`
	DataType string `json:"data_type"`
	Value    any    `json:"value"`
}

// DocumentBacklink identifies both the visible source document and the named
// field that points to the requested exact document.
type DocumentBacklink struct {
	DocumentReferenceSummary
	FieldID   int64  `json:"field_id"`
	FieldName string `json:"field_name"`
}

type storedCustomFieldValue struct {
	FieldID  int64
	Name     string
	DataType string
	Row      customfield.ValueRow
}

func (s *Server) loadCustomFieldValues(ctx context.Context, p *pluginapi.Principal, documentID int64) ([]DocumentCustomFieldValue, error) {
	rows, err := s.DB.Read.QueryContext(ctx, `
		SELECT f.id, f.name, f.data_type,
		       v.value_text, v.value_number, v.value_int, v.value_bool, v.value_date
		FROM document_custom_field_values v
		JOIN custom_fields f ON f.id = v.field_id
		WHERE v.document_id = ?
		ORDER BY f.name, f.id
	`, documentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	stored := make([]storedCustomFieldValue, 0)
	targetIDs := make([]int64, 0)
	for rows.Next() {
		var value storedCustomFieldValue
		if err := rows.Scan(&value.FieldID, &value.Name, &value.DataType,
			&value.Row.Text, &value.Row.Number, &value.Row.Int, &value.Row.Bool, &value.Row.Date); err != nil {
			return nil, err
		}
		stored = append(stored, value)
		if value.DataType == "documentlink" && value.Row.Int.Valid {
			targetIDs = append(targetIDs, value.Row.Int.Int64)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	targets, err := s.loadReferenceSummaries(ctx, p, targetIDs)
	if err != nil {
		return nil, err
	}
	out := make([]DocumentCustomFieldValue, 0, len(stored))
	for _, value := range stored {
		if value.DataType == "documentlink" {
			if !value.Row.Int.Valid {
				continue
			}
			target, ok := targets[value.Row.Int.Int64]
			if !ok {
				continue
			}
			out = append(out, DocumentCustomFieldValue{
				FieldID: value.FieldID, Name: value.Name, DataType: value.DataType, Value: target,
			})
			continue
		}
		typed, ok, err := typedCustomFieldValue(value.DataType, value.Row)
		if err != nil {
			return nil, fmt.Errorf("custom field %d: %w", value.FieldID, err)
		}
		if ok {
			out = append(out, DocumentCustomFieldValue{
				FieldID: value.FieldID, Name: value.Name, DataType: value.DataType, Value: typed,
			})
		}
	}
	return out, nil
}

func typedCustomFieldValue(dataType string, row customfield.ValueRow) (any, bool, error) {
	switch dataType {
	case "text", "url", "select":
		return row.Text.String, row.Text.Valid, nil
	case "multi":
		if !row.Text.Valid {
			return nil, false, nil
		}
		var values []string
		if err := json.Unmarshal([]byte(row.Text.String), &values); err != nil {
			return nil, false, err
		}
		return values, true, nil
	case "number", "monetary":
		return row.Number.Float64, row.Number.Valid, nil
	case "date":
		return row.Date.Int64, row.Date.Valid, nil
	case "bool":
		return row.Bool.Int64 != 0, row.Bool.Valid, nil
	default:
		return row.Text.String, row.Text.Valid, nil
	}
}

func (s *Server) loadReferenceSummaries(ctx context.Context, p *pluginapi.Principal, ids []int64) (map[int64]DocumentReferenceSummary, error) {
	out := make(map[int64]DocumentReferenceSummary)
	if len(ids) == 0 {
		return out, nil
	}
	intrinsicCtx := context.WithValue(ctx, systemContextKey{}, int64(0))
	decisions, err := s.documentPermissionDecisions(intrinsicCtx, nil, p, ids, authz.PermView)
	if err != nil {
		return nil, err
	}
	allowed := make([]int64, 0, len(ids))
	seen := make(map[int64]struct{}, len(ids))
	for _, id := range ids {
		if !decisions[id] {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		allowed = append(allowed, id)
	}
	if len(allowed) == 0 {
		return out, nil
	}
	args := make([]any, 0, len(allowed))
	for _, id := range allowed {
		args = append(args, id)
	}
	rows, err := s.DB.Read.QueryContext(ctx, `
		SELECT d.id, d.title, js.code, COALESCE(d.mime_type, ''),
		       COALESCE(jc.code, 0), d.created_at
		FROM documents d
		JOIN jd_systems js ON js.id = d.system_id
		LEFT JOIN jd_categories jc ON jc.id = d.jd_category_id
		WHERE d.trashed_at IS NULL AND d.id IN (`+placeholders(len(allowed))+`)
	`, args...)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var summary DocumentReferenceSummary
		if err := rows.Scan(&summary.ID, &summary.Title, &summary.SystemCode, &summary.MIME,
			&summary.JDCategoryCode, &summary.CreatedAt); err != nil {
			rows.Close()
			return nil, err
		}
		summary.JDAddress = systems.Address(summary.SystemCode, int(summary.JDCategoryCode), summary.ID)
		summary.IsLatest = true
		out[summary.ID] = summary
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if err := s.markReferenceLatest(intrinsicCtx, p, out); err != nil {
		return nil, err
	}
	return out, nil
}

func (s *Server) markReferenceLatest(ctx context.Context, p *pluginapi.Principal, summaries map[int64]DocumentReferenceSummary) error {
	if len(summaries) == 0 {
		return nil
	}
	args := make([]any, 0, len(summaries))
	for id := range summaries {
		args = append(args, id)
	}
	rows, err := s.DB.Read.QueryContext(ctx, `
		SELECT base.id, newer.id
		FROM documents base
		JOIN documents newer
		  ON base.version_family_key IS NOT NULL
		 AND newer.system_id = base.system_id
		 AND newer.version_family_key = base.version_family_key
		 AND newer.id > base.id
		 AND newer.trashed_at IS NULL
		WHERE base.id IN (`+placeholders(len(args))+`)
	`, args...)
	if err != nil {
		return err
	}
	newerByBase := make(map[int64][]int64)
	newerIDs := make([]int64, 0)
	for rows.Next() {
		var baseID, newerID int64
		if err := rows.Scan(&baseID, &newerID); err != nil {
			rows.Close()
			return err
		}
		newerByBase[baseID] = append(newerByBase[baseID], newerID)
		newerIDs = append(newerIDs, newerID)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	decisions, err := s.documentPermissionDecisions(ctx, nil, p, newerIDs, authz.PermView)
	if err != nil {
		return err
	}
	for baseID, ids := range newerByBase {
		for _, newerID := range ids {
			if !decisions[newerID] {
				continue
			}
			summary := summaries[baseID]
			summary.IsLatest = false
			summaries[baseID] = summary
			break
		}
	}
	return nil
}

// ListReferencedBy serves GET /api/documents/{id}/referenced-by/.
func (s *Server) ListReferencedBy(w http.ResponseWriter, r *http.Request) {
	if !auth.RequireScope(w, r, auth.ScopeDocumentsRead) {
		return
	}
	p := auth.FromContext(r.Context())
	anchorID, err := parseIDPath(r)
	if err != nil {
		s.writeError(w, http.StatusBadRequest, "bad_id", "invalid id")
		return
	}
	if !s.authorize(w, r, p, authz.KindDocument, anchorID, authz.PermView) {
		return
	}
	groups, err := s.principalGroups(r.Context(), p.UserID)
	if err != nil {
		s.serverErr(w, "references.groups", err)
		return
	}
	visibility, visibilityArgs := intrinsicDocumentVisibilityWhereAlias(p, groups, "d")
	fromSQL := `document_custom_field_values v
		JOIN custom_fields f ON f.id = v.field_id AND f.data_type = 'documentlink'
		JOIN documents d ON d.id = v.document_id`
	whereSQL := `v.value_int = ? AND EXISTS (
		SELECT 1 FROM documents target WHERE target.id = ? AND target.trashed_at IS NULL
	) AND d.trashed_at IS NULL AND (` + visibility + `)`
	countArgs := append([]any{anchorID, anchorID}, visibilityArgs...)
	var total int
	if err := s.DB.Read.QueryRowContext(r.Context(), `SELECT COUNT(*) FROM `+fromSQL+` WHERE `+whereSQL, countArgs...).Scan(&total); err != nil {
		s.serverErr(w, "references.count", err)
		return
	}

	pp := ParsePageParams(r, 50, 200)
	rowArgs := append([]any{}, countArgs...)
	rowArgs = append(rowArgs, pp.PageSize, pp.Offset())
	rows, err := s.DB.Read.QueryContext(r.Context(), `
		SELECT d.id, d.title, js.code, COALESCE(d.mime_type, ''),
		       COALESCE(jc.code, 0), d.created_at, f.id, f.name
		FROM `+fromSQL+`
		JOIN jd_systems js ON js.id = d.system_id
		LEFT JOIN jd_categories jc ON jc.id = d.jd_category_id
		WHERE `+whereSQL+`
		ORDER BY d.id DESC, f.name, f.id
		LIMIT ? OFFSET ?
	`, rowArgs...)
	if err != nil {
		s.serverErr(w, "references.list", err)
		return
	}
	defer rows.Close()
	out := make([]DocumentBacklink, 0, pp.PageSize)
	summaries := make(map[int64]DocumentReferenceSummary)
	for rows.Next() {
		var item DocumentBacklink
		if err := rows.Scan(&item.ID, &item.Title, &item.SystemCode, &item.MIME,
			&item.JDCategoryCode, &item.CreatedAt, &item.FieldID, &item.FieldName); err != nil {
			s.serverErr(w, "references.scan", err)
			return
		}
		item.JDAddress = systems.Address(item.SystemCode, int(item.JDCategoryCode), item.ID)
		item.IsLatest = true
		out = append(out, item)
		summaries[item.ID] = item.DocumentReferenceSummary
	}
	if err := rows.Err(); err != nil {
		s.serverErr(w, "references.rows", err)
		return
	}
	intrinsicCtx := context.WithValue(r.Context(), systemContextKey{}, int64(0))
	if err := s.markReferenceLatest(intrinsicCtx, p, summaries); err != nil {
		s.serverErr(w, "references.latest", err)
		return
	}
	for i := range out {
		out[i].DocumentReferenceSummary = summaries[out[i].ID]
	}
	s.writeJSON(w, http.StatusOK, BuildEnvelope(r, total, pp, out))
}
