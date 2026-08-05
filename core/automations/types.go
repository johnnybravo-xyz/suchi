// Package automations is the trigger→conditions→actions engine.
//
// Automations fire on job events (consumption, document_added,
// document_updated) and apply bulk metadata operations to the doc that
// triggered the event. Rules from core/classify/rules are a simpler
// cousin: they classify. Automations mutate — assign owner, add tags,
// set document_type, etc. — with a filter layer between event and
// action so the operator can say "only for docs tagged 'invoice'".
//
// Design principles:
//   - Data-driven: every automation is rows in three tables. No Go
//     code changes to add a rule.
//   - Fail-soft: an action that errors logs and skips; other actions in
//     the same automation still run.
//   - Restart-safe: applies inside the same db.WriteTx as any downstream
//     work in postingest — a crash rolls back the doc's ingest tail
//     and it retries.
//
// Named for what they do. Distinct from core/workflow/, which owns
// the state-machine "approvals" engine (routing/sign-off), exposed
// at /api/approvals/*. Automations live at /api/automations/*.
package automations

// TriggerType names the event that fires an automation. Values match
// the on-disk `workflow_triggers.type` column and the compat API's
// integer codes below.
type TriggerType string

const (
	TriggerConsumption     TriggerType = "consumption"
	TriggerDocumentAdded   TriggerType = "document_added"
	TriggerDocumentUpdated TriggerType = "document_updated"
)

// Integer wire codes for the trigger type. Mobile clients that model
// this as an enum send the integer; humans can send the string form.
// Keep in sync with docs/api.mdx.
const (
	CodeConsumption     = 1
	CodeDocumentAdded   = 2
	CodeDocumentUpdated = 3
)

func TriggerFromCode(n int) TriggerType {
	switch n {
	case CodeConsumption:
		return TriggerConsumption
	case CodeDocumentAdded:
		return TriggerDocumentAdded
	case CodeDocumentUpdated:
		return TriggerDocumentUpdated
	}
	return ""
}

func TriggerToCode(t TriggerType) int {
	switch t {
	case TriggerConsumption:
		return CodeConsumption
	case TriggerDocumentAdded:
		return CodeDocumentAdded
	case TriggerDocumentUpdated:
		return CodeDocumentUpdated
	}
	return 0
}

// Workflow is one row in `workflows`.
type Workflow struct {
	ID         int64     `json:"id"`
	Name       string    `json:"name"`
	OrderIndex int       `json:"order"`
	Enabled    bool      `json:"enabled"`
	Triggers   []Trigger `json:"triggers"`
	Actions    []Action  `json:"actions"`
	CreatedAt  int64     `json:"created_at"`
	UpdatedAt  int64     `json:"updated_at"`
}

// Trigger is one row in `workflow_triggers`.
type Trigger struct {
	ID               int64       `json:"id"`
	Type             TriggerType `json:"-"`
	TypeCode         int         `json:"type"` // JSON emits integer for wire compat
	FilterPath       string      `json:"filter_path,omitempty"`
	FilterFilename   string      `json:"filter_filename,omitempty"`
	FilterMailRuleID int64       `json:"filter_mailrule,omitempty"`
	FilterTagID      int64       `json:"filter_has_tag,omitempty"`
	FilterCorrID     int64       `json:"filter_has_correspondent,omitempty"`
	FilterDocTypeID  int64       `json:"filter_has_document_type,omitempty"`
	FilterContentRE  string      `json:"filter_content_matching,omitempty"`
}

// Action is one row in `workflow_actions`. Params shape depends on
// Kind — see 0020_automations.sql for the per-kind schema.
type Action struct {
	ID         int64          `json:"id"`
	OrderIndex int            `json:"order"`
	Kind       string         `json:"type"`
	Params     map[string]any `json:"params"`
}
