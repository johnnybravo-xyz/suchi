// Admin UI for custom-field definitions. Sibling to automations.go +
// groups.go — same shape: table + modal editor, wired via fetch to
// /api/custom_fields/*. Admin-only.
//
// Per-document field VALUES are edited on the doc detail page via
// detail-actions.js — this file is only about the schema.

package ui

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/suchi-dms/suchi/core/auth"
)

func strconvFormatFloat(f float64) string {
	return strconv.FormatFloat(f, 'f', -1, 64)
}

func dateEpochToISO(epoch int64) string {
	return time.Unix(epoch, 0).UTC().Format("2006-01-02")
}

// customFieldRow is the projection the /admin/custom-fields template
// renders. Choices are the JSON array pulled out of extra_data for
// select/multi types (empty for others).
type customFieldRow struct {
	ID         int64
	Name       string
	DataType   string
	ExtraJSON  string
	Choices    []string
	UsageCount int
}

// CustomFieldsPage — GET /admin/custom-fields. Admin-only.
func (s *Server) CustomFieldsPage(w http.ResponseWriter, r *http.Request) {
	p := auth.FromContext(r.Context())
	if p == nil || p.Role != "admin" {
		http.Error(w, "admin required", http.StatusForbidden)
		return
	}

	rows, err := s.DB.Read.QueryContext(r.Context(), `
		SELECT cf.id, cf.name, cf.data_type, COALESCE(cf.extra_data, '{}'),
		       (SELECT COUNT(*) FROM document_custom_field_values v
		        WHERE v.field_id = cf.id)
		FROM custom_fields cf
		ORDER BY cf.name
	`)
	if err != nil {
		s.Log.Warn("ui.customfields.list", "err", err.Error())
		http.Error(w, "list failed", http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	var list []customFieldRow
	for rows.Next() {
		var f customFieldRow
		var extra sql.NullString
		if err := rows.Scan(&f.ID, &f.Name, &f.DataType, &extra, &f.UsageCount); err != nil {
			s.Log.Warn("ui.customfields.scan", "err", err.Error())
			continue
		}
		if extra.Valid {
			f.ExtraJSON = extra.String
			f.Choices = extractChoices(extra.String)
		}
		list = append(list, f)
	}

	s.render(w, r, "customfields", map[string]any{
		"Fields": list,
	})
}

// extractChoices pulls the "choices" array from extra_data if present.
// Best-effort — malformed JSON returns nil rather than blowing up the
// page render.
func extractChoices(raw string) []string {
	var wrap struct {
		Choices []string `json:"choices"`
	}
	if err := json.Unmarshal([]byte(raw), &wrap); err != nil {
		return nil
	}
	return wrap.Choices
}

// rawFieldValue returns the type-native value from a
// document_custom_field_values row, formatted for pre-filling an
// HTML input:
//
//	text / select / url / documentlink → value_text as-is
//	number / monetary                   → value_number as a decimal string
//	date                                → YYYY-MM-DD (from value_date epoch)
//	bool                                → "true" / "false"
//	multi                               → the JSON-array in value_text
//
// Empty when the doc has no value yet (all columns null).
func rawFieldValue(dataType string, text sql.NullString, num sql.NullFloat64,
	i sql.NullInt64, b sql.NullInt64, date sql.NullInt64) string {
	switch dataType {
	case "text", "select", "url", "documentlink", "multi":
		if text.Valid {
			return text.String
		}
	case "number", "monetary":
		if num.Valid {
			// %g strips trailing zeros; consistent with the number input.
			return trimFloat(num.Float64)
		}
	case "date":
		if date.Valid {
			return dateEpochToISO(date.Int64)
		}
	case "bool":
		if b.Valid {
			if b.Int64 != 0 {
				return "true"
			}
			return "false"
		}
	}
	return ""
}

func trimFloat(f float64) string {
	// strconv.FormatFloat with -1 precision gives us the shortest
	// round-tripping form.
	return strconvFormatFloat(f)
}
