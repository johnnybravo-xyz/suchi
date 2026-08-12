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
// Named for what they do. Distinct from core/approvals/, which owns
// the state-machine "approvals" engine (routing/sign-off), exposed
// at /api/approvals/*. Automations live at /api/automations/*.
package automations

// TriggerType names the event that fires an automation. Values match
// the on-disk `automation_triggers.type` column and the compat API's
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

// Automation is one row in `automations`.
//
// System = true marks an automation that suchi seeded on first boot
// ("Auto-file from archive" is the first of these). System rows are
// undeletable but otherwise identical: the operator can toggle
// enabled, rename them, and tune their action params. SystemSlug is
// the seed's stable identifier; the seeder INSERT ... ON CONFLICT's
// on it so re-runs are no-ops.
type Automation struct {
	ID         int64     `json:"id"`
	Name       string    `json:"name"`
	OrderIndex int       `json:"order"`
	Enabled    bool      `json:"enabled"`
	System     bool      `json:"system,omitempty"`
	SystemSlug string    `json:"system_slug,omitempty"`
	Triggers   []Trigger `json:"triggers"`
	Actions    []Action  `json:"actions"`
	CreatedAt  int64     `json:"created_at"`
	UpdatedAt  int64     `json:"updated_at"`
}

// Trigger is one row in `automation_triggers`.
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

// Action is one row in `automation_actions`. Params shape depends on
// Kind — see automation_actions in 0001_baseline.sql for the per-kind schema.
type Action struct {
	ID         int64          `json:"id"`
	OrderIndex int            `json:"order"`
	Kind       string         `json:"type"`
	Params     map[string]any `json:"params"`
}

// AutomationPatch is a sparse update — nil pointers leave the field
// untouched. PATCH /api/automations/{id} decodes into this so the SPA
// can flip `enabled` without resending the whole automation. Triggers /
// Actions replace wholesale when their pointer is non-nil (empty slice
// clears the list); leave nil to keep the current rows.
type AutomationPatch struct {
	Name       *string    `json:"name,omitempty"`
	OrderIndex *int       `json:"order,omitempty"`
	Enabled    *bool      `json:"enabled,omitempty"`
	Triggers   *[]Trigger `json:"triggers,omitempty"`
	Actions    *[]Action  `json:"actions,omitempty"`
}
