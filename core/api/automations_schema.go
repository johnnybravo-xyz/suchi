// /api/automations/schema — static JSON describing what triggers and
// action kinds the current server understands. The SPA builder reads
// this so a new action kind (or trigger code) appears in the picker
// without a client release.
//
// The alternative is what the SPA does today: hand-copy the enum from
// core/automations/{types,apply}.go into the client. That drifts.
// This endpoint is the fix.
//
// Payload is a plain array-of-triggers + array-of-actions object; no
// envelope, no pagination — bounded populations.

package api

import (
	"net/http"

	"github.com/suchi-dms/suchi/core/auth"
)

// AutomationSchema is the wire shape.
type AutomationSchema struct {
	Triggers []AutomationTrigger `json:"triggers"`
	Actions  []AutomationAction  `json:"actions"`
}

// AutomationTrigger names one event that can fire an automation.
type AutomationTrigger struct {
	Code int    `json:"code"`
	Type string `json:"type"`
	Name string `json:"name"`
}

// AutomationAction is one action kind + its expected params.
type AutomationAction struct {
	Kind        string                  `json:"kind"`
	Name        string                  `json:"name"`
	Description string                  `json:"description,omitempty"`
	Params      []AutomationActionParam `json:"params"`
}

// AutomationActionParam describes one field in the action's params
// object. Types: string, int, id (foreign key), tag_ids (array of
// tag ids), template (Jinja/Gonja template string).
type AutomationActionParam struct {
	Name        string `json:"name"`
	Type        string `json:"type"`
	Required    bool   `json:"required"`
	Description string `json:"description,omitempty"`
	// TargetKind is set for `id` and `tag_ids` params so the SPA
	// knows which picker to render — one of "tag", "correspondent",
	// "document_type", "storage_path", "custom_field", "user".
	TargetKind string `json:"target_kind,omitempty"`
}

// GetAutomationSchema serves GET /api/automations/schema. Any authed
// caller can read it (the SPA builder needs it before create/patch).
// Admin-only writes to /api/automations/ still gate mutations.
func (s *Server) GetAutomationSchema(w http.ResponseWriter, r *http.Request) {
	if auth.FromContext(r.Context()) == nil {
		s.writeError(w, http.StatusUnauthorized, "unauthorized", "auth required")
		return
	}
	s.writeJSON(w, http.StatusOK, automationSchema)
}

// automationSchema is the load-bearing static value. Keep in lockstep
// with core/automations/types.go (trigger codes) and apply.go (action
// switch). A new case in apply.go without a row here means the SPA
// builder won't expose the action.
var automationSchema = AutomationSchema{
	Triggers: []AutomationTrigger{
		{Code: 1, Type: "consumption", Name: "On consumption (pre-content)"},
		{Code: 2, Type: "document_added", Name: "After a new document lands"},
		{Code: 3, Type: "document_updated", Name: "After a document metadata update"},
	},
	Actions: []AutomationAction{
		{
			Kind: "assign_title", Name: "Set title",
			Description: "Rewrites the doc title from a Gonja template. Available: {{correspondent}}, {{title}}, {{doc_type}}, {{created}}.",
			Params: []AutomationActionParam{
				{Name: "template", Type: "template", Required: true},
			},
		},
		{
			Kind: "assign_tags", Name: "Add tags",
			Params: []AutomationActionParam{
				{Name: "tag_ids", Type: "tag_ids", Required: true, TargetKind: "tag"},
			},
		},
		{
			Kind: "assign_correspondent", Name: "Assign correspondent",
			Params: []AutomationActionParam{
				{Name: "correspondent_id", Type: "id", Required: true, TargetKind: "correspondent"},
			},
		},
		{
			Kind: "assign_document_type", Name: "Assign document type",
			Params: []AutomationActionParam{
				{Name: "document_type_id", Type: "id", Required: true, TargetKind: "document_type"},
			},
		},
		{
			Kind: "assign_storage_path", Name: "Assign storage path",
			Params: []AutomationActionParam{
				{Name: "storage_path_id", Type: "id", Required: true, TargetKind: "storage_path"},
			},
		},
		{
			Kind: "assign_owner", Name: "Assign owner",
			Params: []AutomationActionParam{
				{Name: "owner_id", Type: "id", Required: true, TargetKind: "user"},
			},
		},
		{
			Kind: "assign_custom_field", Name: "Set custom field",
			Params: []AutomationActionParam{
				{Name: "field_id", Type: "id", Required: true, TargetKind: "custom_field"},
				{Name: "value", Type: "string", Required: true,
					Description: "Coerced to the field's declared type."},
			},
		},
	},
}
