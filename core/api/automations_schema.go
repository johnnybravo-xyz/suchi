// SPDX-License-Identifier: AGPL-3.0-or-later

// /api/automations/schema describes the triggers and actions the SPA
// builder can expose without duplicating server enums.

package api

import (
	"net/http"

	"github.com/johnnybravo-xyz/suchi/core/automations"
)

// AutomationSchema is the wire shape.
type AutomationSchema struct {
	Triggers []AutomationTrigger `json:"triggers"`
	Actions  []AutomationAction  `json:"actions"`
	Ask      AutomationAsk       `json:"ask"`
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
// object. Types: string, id (foreign key), tag_ids (array of tag ids),
// JSON, and template.
type AutomationActionParam struct {
	Name        string `json:"name"`
	Type        string `json:"type"`
	Required    bool   `json:"required"`
	Description string `json:"description,omitempty"`
	// TargetKind is set for `id` and `tag_ids` params so the SPA knows which
	// picker to render: tag, correspondent, rendered_layout, custom_field, or user.
	TargetKind string `json:"target_kind,omitempty"`
}

// AutomationAsk describes the bounded classifier-backed branch supported by
// the automation editor.
type AutomationAsk struct {
	MaxEnabled  int                   `json:"max_enabled"`
	TriggerType string                `json:"trigger_type"`
	Answers     []AutomationAskAnswer `json:"answers"`
	ActionKinds []string              `json:"action_kinds"`
}

// AutomationAskAnswer names one closed answer vocabulary.
type AutomationAskAnswer struct {
	Type       string `json:"type"`
	Name       string `json:"name"`
	MinChoices int    `json:"min_choices,omitempty"`
	MaxChoices int    `json:"max_choices,omitempty"`
}

// GetAutomationSchema serves GET /api/automations/schema. Any authed
// caller can read it (the SPA builder needs it before create/patch).
// Admin-only writes to /api/automations/ still gate mutations.
func (s *Server) GetAutomationSchema(w http.ResponseWriter, r *http.Request) {
	if s.requireAuth(w, r) == nil {
		return
	}
	s.writeJSON(w, http.StatusOK, automationSchema)
}

// automationSchema is the builder's supported surface. Trigger codes stay in
// lockstep with core/automations/types.go. The JSON editor may use additional
// removal actions accepted by the engine but intentionally omitted here.
var automationSchema = AutomationSchema{
	Triggers: []AutomationTrigger{
		{Code: 1, Type: "consumption", Name: "On consumption (pre-content)"},
		{Code: 2, Type: "document_added", Name: "After a new document lands"},
		{Code: 3, Type: "document_updated", Name: "After a document metadata update"},
	},
	Ask: AutomationAsk{
		MaxEnabled: automations.MaxEnabledAsks, TriggerType: "document_added",
		Answers: []AutomationAskAnswer{
			{Type: "yes_no", Name: "Yes / No"},
			{Type: "choice", Name: "Multiple choice", MinChoices: 2, MaxChoices: automations.MaxAskChoices},
		},
		ActionKinds: []string{"assign_tags", "assign_correspondent", "assign_jd_category", "assign_custom_field"},
	},
	Actions: []AutomationAction{
		{
			Kind: "assign_title", Name: "Set title",
			Description: "Rewrites the document title from a bounded placeholder template.",
			Params: []AutomationActionParam{
				{Name: "template", Type: "template", Required: true,
					Description: "Supports {{title}}, {{correspondent}}, {{tags}}, and {{date}}."},
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
			Kind: "assign_jd_category", Name: "Assign JD category",
			Params: []AutomationActionParam{
				{Name: "jd_category_id", Type: "id", Required: true, TargetKind: "jd_category"},
			},
		},
		{
			Kind: "assign_storage_path", Name: "Assign folder layout",
			Params: []AutomationActionParam{
				{Name: "storage_path_id", Type: "id", Required: true, TargetKind: "rendered_layout"},
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
