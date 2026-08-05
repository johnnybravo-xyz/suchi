// Admin UI for automations. Renders a list page with each automation's
// name, order, enabled flag, trigger types, and action count. Wraps a
// JSON textarea editor for triggers+actions since a proper drag-drop
// editor is out of scope for Phase 5. Enable/disable + delete run
// through the JSON API; the template ships small inline JS.

package ui

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/suchi-dms/suchi/core/auth"
)

// automationRow is what the list template renders.
type automationRow struct {
	ID          int64
	Name        string
	OrderIndex  int
	Enabled     bool
	Triggers    string // comma-joined trigger types
	ActionCount int
	SpecJSON    string // pretty-printed {triggers:[], actions:[]} bag for the editor
}

// AutomationsPage — GET /admin/automations. Admin-only.
func (s *Server) AutomationsPage(w http.ResponseWriter, r *http.Request) {
	p := auth.FromContext(r.Context())
	if p == nil || p.Role != "admin" {
		http.Error(w, "admin required", http.StatusForbidden)
		return
	}

	rows, err := s.DB.Read.QueryContext(r.Context(), `
		SELECT w.id, w.name, w.order_index, w.enabled,
		       COALESCE(GROUP_CONCAT(DISTINCT t.type), ''),
		       (SELECT COUNT(*) FROM workflow_actions a WHERE a.workflow_id = w.id)
		FROM workflows w
		LEFT JOIN workflow_triggers t ON t.workflow_id = w.id
		GROUP BY w.id
		ORDER BY w.order_index, w.id
	`)
	if err != nil {
		s.Log.Warn("ui.automations.list", "err", err.Error())
		http.Error(w, "list failed", http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	var list []automationRow
	for rows.Next() {
		var a automationRow
		var enabled int
		var triggers sql.NullString
		if err := rows.Scan(&a.ID, &a.Name, &a.OrderIndex, &enabled,
			&triggers, &a.ActionCount); err != nil {
			s.Log.Warn("ui.automations.scan", "err", err.Error())
			continue
		}
		a.Enabled = enabled == 1
		a.Triggers = triggers.String
		a.SpecJSON = s.loadSpecJSON(r, a.ID)
		list = append(list, a)
	}

	s.render(w, r, "automations", map[string]any{
		"Automations": list,
	})
}

// loadSpecJSON reads the triggers+actions for one automation and returns
// a pretty JSON blob suitable for the edit textarea. Best-effort — on
// error, returns an empty scaffold so the editor still opens.
func (s *Server) loadSpecJSON(r *http.Request, id int64) string {
	type trig struct {
		Type             string `json:"type"`
		FilterPath       string `json:"filter_path,omitempty"`
		FilterFilename   string `json:"filter_filename,omitempty"`
		FilterMailRuleID int64  `json:"filter_mailrule,omitempty"`
		FilterTagID      int64  `json:"filter_has_tag,omitempty"`
		FilterCorrID     int64  `json:"filter_has_correspondent,omitempty"`
		FilterDocTypeID  int64  `json:"filter_has_document_type,omitempty"`
		FilterContentRE  string `json:"filter_content_matching,omitempty"`
	}
	type act struct {
		Kind   string         `json:"type"`
		Order  int            `json:"order"`
		Params map[string]any `json:"params"`
	}
	body := struct {
		Triggers []trig `json:"triggers"`
		Actions  []act  `json:"actions"`
	}{Triggers: []trig{}, Actions: []act{}}

	trows, err := s.DB.Read.QueryContext(r.Context(), `
		SELECT type,
		       COALESCE(filter_path, ''), COALESCE(filter_filename, ''),
		       COALESCE(filter_mailrule_id, 0),
		       COALESCE(filter_tag_id, 0), COALESCE(filter_corr_id, 0),
		       COALESCE(filter_doctype_id, 0),
		       COALESCE(filter_content_re, '')
		FROM workflow_triggers WHERE workflow_id = ? ORDER BY id
	`, id)
	if err == nil {
		defer trows.Close()
		for trows.Next() {
			var t trig
			if err := trows.Scan(&t.Type, &t.FilterPath, &t.FilterFilename,
				&t.FilterMailRuleID, &t.FilterTagID, &t.FilterCorrID,
				&t.FilterDocTypeID, &t.FilterContentRE); err != nil {
				continue
			}
			body.Triggers = append(body.Triggers, t)
		}
	}

	arows, err := s.DB.Read.QueryContext(r.Context(), `
		SELECT order_index, kind, params_json
		FROM workflow_actions WHERE workflow_id = ? ORDER BY order_index, id
	`, id)
	if err == nil {
		defer arows.Close()
		for arows.Next() {
			var a act
			var raw string
			if err := arows.Scan(&a.Order, &a.Kind, &raw); err != nil {
				continue
			}
			a.Params = map[string]any{}
			if raw != "" {
				_ = json.Unmarshal([]byte(raw), &a.Params)
			}
			body.Actions = append(body.Actions, a)
		}
	}

	b, err := json.MarshalIndent(body, "", "  ")
	if err != nil {
		return `{"triggers":[],"actions":[]}`
	}
	return strings.TrimSpace(string(b))
}
