// Package bundle: workflow migration.
//
// Decodes documents.workflow / workflowtrigger / workflowaction rows from
// an exporter manifest and writes suchi automations.
//
// Why-not-what: paperless-ngx workflows are a superset of suchi
// automations — SCHEDULED triggers, EMAIL / WEBHOOK / PASSWORD_REMOVAL
// actions, storage-path / all-of / not / custom-field trigger filters
// have no target-side equivalent yet. Rather than partial-implementing
// each one silently, we classify every entry as FULL / PARTIAL / FAILED
// through MigrationReport and record a followup feature name so the
// post-import review surfaces the exact gaps.
//
// Idempotency: automations.name is UNIQUE; ON CONFLICT DO NOTHING makes
// re-runs no-ops for names already present. There is no legacy_id column
// on automations (they are user-authored, not doc-shaped), so a re-run
// after a name change would insert a duplicate — deliberate: the source
// name is the identity here.
//
// See docs/importer.mdx for the canonical trigger / action / filter
// mapping tables.
//
// Version pin: this decoder targets paperless-ngx v3.0.5. Migration
// is a one-shot event, not a supported compatibility surface — we do
// NOT chase paperless minor releases. Unknown trigger.type / action.type
// integers fall into the default branch and surface as FAILED with a
// "paperless version drift" Followup so the maintainer can decide
// whether the delta is worth a bump.
package bundle

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/johnnybravo-xyz/suchi/core/db"
)

// Model constants for workflow objects. Distinct from manifest.go's
// document-side constants to keep the workflow migration self-contained.
const (
	ModelWorkflow              = "documents.workflow"
	ModelWorkflowTrigger       = "documents.workflowtrigger"
	ModelWorkflowAction        = "documents.workflowaction"
	ModelWorkflowActionEmail   = "documents.workflowactionemail"
	ModelWorkflowActionWebhook = "documents.workflowactionwebhook"
)

// Source-side field shapes. Kept local (not in manifest.go) so the
// workflow migration is one-file-regenerable.

type WorkflowFields struct {
	Name     string  `json:"name"`
	Order    int     `json:"order"`
	Enabled  bool    `json:"enabled"`
	Triggers []int64 `json:"triggers"`
	Actions  []int64 `json:"actions"`
}

type WorkflowTriggerFields struct {
	Type                          int             `json:"type"`            // 1 CONSUMPTION, 2 DOCUMENT_ADDED, 3 DOCUMENT_UPDATED, 4 SCHEDULED
	Sources                       json.RawMessage `json:"sources"`         // v3 emits comma-separated string; v2 was int array — accept both, we don't use downstream
	FilterFilename                *string         `json:"filter_filename"` // may be null
	FilterPath                    *string         `json:"filter_path"`
	FilterMailrule                *int64          `json:"filter_mailrule"`
	Match                         string          `json:"match"`
	MatchingAlgorithm             int             `json:"matching_algorithm"`
	IsInsensitive                 bool            `json:"is_insensitive"`
	FilterHasTags                 []int64         `json:"filter_has_tags"`
	FilterHasAllTags              []int64         `json:"filter_has_all_tags"`
	FilterHasNotTags              []int64         `json:"filter_has_not_tags"`
	FilterHasCorrespondent        *int64          `json:"filter_has_correspondent"`
	FilterHasDocumentType         *int64          `json:"filter_has_document_type"`
	FilterHasStoragePath          *int64          `json:"filter_has_storage_path"`
	FilterCustomFieldQuery        string          `json:"filter_custom_field_query"`
	ScheduleOffsetDays            int             `json:"schedule_offset_days"`
	ScheduleIsRecurring           bool            `json:"schedule_is_recurring"`
	ScheduleRecurringIntervalDays int             `json:"schedule_recurring_interval_days"`
	ScheduleDateField             string          `json:"schedule_date_field"`
}

type WorkflowActionFields struct {
	Order                    int             `json:"order"`
	Type                     int             `json:"type"` // 1 ASSIGN, 2 REMOVE, 3 EMAIL, 4 WEBHOOK, 5 PASSWORD_REMOVAL, 6 MOVE_TO_TRASH
	AssignTitle              string          `json:"assign_title"`
	AssignTags               []int64         `json:"assign_tags"`
	AssignDocumentType       *int64          `json:"assign_document_type"`
	AssignCorrespondent      *int64          `json:"assign_correspondent"`
	AssignStoragePath        *int64          `json:"assign_storage_path"`
	AssignOwner              *int64          `json:"assign_owner"`
	AssignCustomFields       []int64         `json:"assign_custom_fields"`
	AssignCustomFieldsValues json.RawMessage `json:"assign_custom_fields_values"`
	RemoveTags               []int64         `json:"remove_tags"`
	RemoveDocumentTypes      []int64         `json:"remove_document_types"`
	RemoveCorrespondents     []int64         `json:"remove_correspondents"`
	RemoveStoragePaths       []int64         `json:"remove_storage_paths"`
	RemoveOwners             []int64         `json:"remove_owners"`
	RemoveCustomFields       []int64         `json:"remove_custom_fields"`
	RemoveAllTags            bool            `json:"remove_all_tags"`
	RemoveAllCorrespondents  bool            `json:"remove_all_correspondents"`
	Email                    *int64          `json:"email"`
	Webhook                  *int64          `json:"webhook"`
}

// Source trigger type codes.
const (
	srcTriggerConsumption     = 1
	srcTriggerDocumentAdded   = 2
	srcTriggerDocumentUpdated = 3
	srcTriggerScheduled       = 4
)

// Source action type codes.
const (
	srcActionAssign          = 1
	srcActionRemove          = 2
	srcActionEmail           = 3
	srcActionWebhook         = 4
	srcActionPasswordRemoval = 5
	srcActionMoveToTrash     = 6
)

// srcTriggerTypeToSuchi maps a source trigger type to the suchi
// automation_triggers.type string. Missing entries indicate the type is
// not representable on the target and the caller must fail the workflow.
var srcTriggerTypeToSuchi = map[int]string{
	srcTriggerConsumption:     "consumption",
	srcTriggerDocumentAdded:   "document_added",
	srcTriggerDocumentUpdated: "document_updated",
}

// mappedAction is one suchi action produced from one source assign/remove row.
type mappedAction struct {
	kind   string
	params map[string]any
}

// suchiTrigger is the target-side row shape before it hits the DB. Named
// (not anonymous) so builders and the writer share one type.
type suchiTrigger struct {
	typ            string
	filterPath     string
	filterFilename string
	filterMailrule int64
	filterTagID    int64
	filterCorrID   int64
	filterDocType  int64
	filterContent  string
}

// ImportWorkflows reads workflow / workflowtrigger / workflowaction /
// workflowactionemail / workflowactionwebhook objects from the manifest
// and writes suchi automations rows. Uses the *Map arguments for PK
// translation. dry: skip DB writes.
//
// Returns the number of workflows *considered* (full + partial + failed).
func ImportWorkflows(
	ctx context.Context,
	d *db.DB,
	log *slog.Logger,
	objs []Object,
	dry bool,
	tagMap, corMap, dtMap, spMap, cfMap map[int64]int64,
	report *MigrationReport,
) (int, error) {
	// Bucket by model. workflowactionemail / workflowactionwebhook are
	// not decoded — their presence-or-absence drives PARTIAL classification
	// on the parent action row, which already carries the FK pointer.
	var workflowObjs, triggerObjs, actionObjs []Object
	for _, o := range objs {
		switch o.Model {
		case ModelWorkflow:
			workflowObjs = append(workflowObjs, o)
		case ModelWorkflowTrigger:
			triggerObjs = append(triggerObjs, o)
		case ModelWorkflowAction:
			actionObjs = append(actionObjs, o)
		}
	}

	// Decode triggers / actions once so per-workflow lookup is O(1).
	triggers := make(map[int64]WorkflowTriggerFields, len(triggerObjs))
	for _, o := range triggerObjs {
		var f WorkflowTriggerFields
		if err := json.Unmarshal(o.Fields, &f); err != nil {
			return 0, fmt.Errorf("decode workflow_trigger pk=%d: %w", o.PK, err)
		}
		triggers[o.PK] = f
	}
	actions := make(map[int64]WorkflowActionFields, len(actionObjs))
	for _, o := range actionObjs {
		var f WorkflowActionFields
		if err := json.Unmarshal(o.Fields, &f); err != nil {
			return 0, fmt.Errorf("decode workflow_action pk=%d: %w", o.PK, err)
		}
		actions[o.PK] = f
	}

	considered := 0
	for _, o := range workflowObjs {
		var wf WorkflowFields
		if err := json.Unmarshal(o.Fields, &wf); err != nil {
			return considered, fmt.Errorf("decode workflow pk=%d: %w", o.PK, err)
		}
		considered++
		if err := importOneWorkflow(ctx, d, log, dry, wf, triggers, actions,
			tagMap, corMap, dtMap, spMap, cfMap, report); err != nil {
			return considered, err
		}
	}
	return considered, nil
}

// importOneWorkflow builds the suchi rows for one source workflow and
// commits them in a single transaction. Classification is recorded on
// report; only structural / DB errors bubble up.
func importOneWorkflow(
	ctx context.Context,
	d *db.DB,
	log *slog.Logger,
	dry bool,
	wf WorkflowFields,
	triggers map[int64]WorkflowTriggerFields,
	actions map[int64]WorkflowActionFields,
	tagMap, corMap, dtMap, spMap, cfMap map[int64]int64,
	report *MigrationReport,
) error {
	source := wf.Name
	target := wf.Name

	// Build triggers. A single SCHEDULED trigger fails the whole workflow.
	var suchiTriggers []suchiTrigger
	partialReasons := []string{}

	for _, tPK := range wf.Triggers {
		tf, ok := triggers[tPK]
		if !ok {
			// Missing FK — treat as partial signal, skip this trigger.
			partialReasons = append(partialReasons,
				fmt.Sprintf("trigger pk=%d missing from manifest", tPK))
			continue
		}
		typ, ok := srcTriggerTypeToSuchi[tf.Type]
		if !ok {
			// Only unmapped type in current spec is SCHEDULED (4).
			report.Failed(KindWorkflow, source, "scheduled trigger unsupported")
			report.Followup("scheduled workflow trigger")
			log.Info("import.workflow", "name", source, "outcome", "failed",
				"reason", "scheduled trigger unsupported")
			return nil
		}

		st := suchiTrigger{typ: typ}
		if tf.FilterPath != nil {
			st.filterPath = *tf.FilterPath
		}
		if tf.FilterFilename != nil {
			st.filterFilename = *tf.FilterFilename
		}
		if tf.FilterMailrule != nil {
			st.filterMailrule = *tf.FilterMailrule
		}
		if len(tf.FilterHasTags) > 0 {
			if id, ok := tagMap[tf.FilterHasTags[0]]; ok {
				st.filterTagID = id
			}
			if len(tf.FilterHasTags) > 1 {
				partialReasons = append(partialReasons,
					"any-of tags list truncated to first")
			}
		}
		if tf.FilterHasCorrespondent != nil {
			if id, ok := corMap[*tf.FilterHasCorrespondent]; ok {
				st.filterCorrID = id
			}
		}
		if tf.FilterHasDocumentType != nil {
			if id, ok := dtMap[*tf.FilterHasDocumentType]; ok {
				st.filterDocType = id
			}
		}
		if tf.FilterHasStoragePath != nil {
			partialReasons = append(partialReasons,
				"storage-path filter unsupported in suchi trigger")
			report.Followup("storage-path trigger filter")
		}
		if len(tf.FilterHasAllTags) > 0 {
			partialReasons = append(partialReasons,
				"all-of tags filter unsupported")
			report.Followup("all-of tags trigger filter")
		}
		if len(tf.FilterHasNotTags) > 0 {
			partialReasons = append(partialReasons,
				"not-tags filter unsupported")
			report.Followup("not-tags trigger filter")
		}
		if tf.FilterCustomFieldQuery != "" {
			partialReasons = append(partialReasons,
				"custom-field trigger filter unsupported")
			report.Followup("custom-field trigger filter")
		}
		// MatchingAlgorithm: 0 NONE, 1 ANY, others (ALL/LITERAL/REGEX/FUZZY/AUTO)
		// have no direct suchi equivalent on triggers — the target has one
		// filter_content_re column. Only carry match when the algorithm is
		// NONE/ANY (i.e. simple), otherwise drop and flag PARTIAL.
		if tf.Match != "" {
			if tf.MatchingAlgorithm == 0 || tf.MatchingAlgorithm == 1 {
				st.filterContent = tf.Match
			} else {
				partialReasons = append(partialReasons,
					"non-trivial match algorithm dropped")
				report.Followup("non-trivial trigger match algorithms")
			}
		}
		suchiTriggers = append(suchiTriggers, st)
	}

	// Build actions.
	var suchiActions []mappedAction
	for _, aPK := range wf.Actions {
		af, ok := actions[aPK]
		if !ok {
			partialReasons = append(partialReasons,
				fmt.Sprintf("action pk=%d missing from manifest", aPK))
			continue
		}
		mapped, reasons, followups := mapAction(af, tagMap, corMap, dtMap, spMap, cfMap)
		suchiActions = append(suchiActions, mapped...)
		partialReasons = append(partialReasons, reasons...)
		for _, fu := range followups {
			report.Followup(fu)
		}
	}

	// Write.
	if !dry {
		if err := writeWorkflow(ctx, d, wf, suchiTriggers, suchiActions); err != nil {
			return fmt.Errorf("write workflow %q: %w", wf.Name, err)
		}
	}

	// Classify.
	if len(partialReasons) == 0 {
		report.Full(KindWorkflow, source, target)
		log.Info("import.workflow", "name", source, "outcome", "full",
			"triggers", len(suchiTriggers), "actions", len(suchiActions))
	} else {
		reason := joinReasons(partialReasons)
		report.Partial(KindWorkflow, source, target, reason)
		log.Info("import.workflow", "name", source, "outcome", "partial",
			"triggers", len(suchiTriggers), "actions", len(suchiActions),
			"reason", reason)
	}
	return nil
}

// mapAction fans one source action into zero-or-more suchi actions and
// returns the classification signals (partial reasons + followup features)
// the caller must record.
func mapAction(
	af WorkflowActionFields,
	tagMap, corMap, dtMap, spMap, cfMap map[int64]int64,
) (out []mappedAction, reasons []string, followups []string) {
	base := af.Order
	switch af.Type {
	case srcActionAssign:
		if af.AssignTitle != "" {
			out = append(out, mappedAction{
				kind:   "assign_title",
				params: map[string]any{"template": af.AssignTitle},
			})
		}
		if len(af.AssignTags) > 0 {
			ids := remapIDs(af.AssignTags, tagMap)
			if len(ids) > 0 {
				out = append(out, mappedAction{
					kind:   "assign_tags",
					params: map[string]any{"tag_ids": ids},
				})
			}
		}
		if af.AssignCorrespondent != nil {
			if id, ok := corMap[*af.AssignCorrespondent]; ok {
				out = append(out, mappedAction{
					kind:   "assign_correspondent",
					params: map[string]any{"correspondent_id": id},
				})
			}
		}
		if af.AssignDocumentType != nil {
			if id, ok := dtMap[*af.AssignDocumentType]; ok {
				out = append(out, mappedAction{
					kind:   "assign_document_type",
					params: map[string]any{"document_type_id": id},
				})
			}
		}
		if af.AssignStoragePath != nil {
			if id, ok := spMap[*af.AssignStoragePath]; ok {
				out = append(out, mappedAction{
					kind:   "assign_storage_path",
					params: map[string]any{"storage_path_id": id},
				})
			}
		}
		if af.AssignOwner != nil {
			// Owner PKs are user rows — no PK remap in this signature,
			// so pass through verbatim. If the users table has been
			// re-numbered on the target the reconciler will notice.
			out = append(out, mappedAction{
				kind:   "assign_owner",
				params: map[string]any{"owner_id": *af.AssignOwner},
			})
		}
		if len(af.AssignCustomFields) > 0 {
			// Custom-field values arrive as a raw JSON object keyed by
			// source field PK. Emit one assign_custom_field per field.
			var values map[string]any
			validValues := true
			if len(af.AssignCustomFieldsValues) > 0 {
				if err := json.Unmarshal(af.AssignCustomFieldsValues, &values); err != nil {
					reasons = append(reasons, "custom-field assignment values are invalid JSON")
					validValues = false
				}
			}
			if !validValues {
				break
			}
			for _, srcFID := range af.AssignCustomFields {
				id, ok := cfMap[srcFID]
				if !ok {
					continue
				}
				var v any
				if values != nil {
					v = values[fmt.Sprintf("%d", srcFID)]
				}
				out = append(out, mappedAction{
					kind: "assign_custom_field",
					params: map[string]any{
						"field_id": id,
						"value":    v,
					},
				})
			}
		}
	case srcActionRemove:
		if len(af.RemoveTags) > 0 {
			ids := remapIDs(af.RemoveTags, tagMap)
			if len(ids) > 0 {
				out = append(out, mappedAction{
					kind:   "remove_tags",
					params: map[string]any{"tag_ids": ids},
				})
			}
		}
		if len(af.RemoveCorrespondents) > 0 {
			// suchi remove_correspondents wants a list of ids; pick first
			// only when the spec constrains us to one. We honor the plan
			// literal ("only ONE correspondent per suchi action, use first").
			if id, ok := corMap[af.RemoveCorrespondents[0]]; ok {
				out = append(out, mappedAction{
					kind:   "remove_correspondents",
					params: map[string]any{"correspondent_ids": []int64{id}},
				})
			}
			if len(af.RemoveCorrespondents) > 1 {
				reasons = append(reasons,
					"remove_correspondents list truncated to first")
			}
		}
		if len(af.RemoveDocumentTypes) > 0 {
			// suchi's remove_document_type takes no params — target has
			// a single doctype. Emit one such action; the multi-value
			// intent is a partial.
			out = append(out, mappedAction{
				kind:   "remove_document_type",
				params: map[string]any{},
			})
			if len(af.RemoveDocumentTypes) > 1 {
				reasons = append(reasons,
					"remove_document_types list collapsed to single remove")
			}
		}
		if len(af.RemoveStoragePaths) > 0 {
			out = append(out, mappedAction{
				kind:   "remove_storage_path",
				params: map[string]any{},
			})
			if len(af.RemoveStoragePaths) > 1 {
				reasons = append(reasons,
					"remove_storage_paths list collapsed to single remove")
			}
		}
		if len(af.RemoveOwners) > 0 {
			reasons = append(reasons,
				"remove_owner action dropped because Suchi documents always require an owner")
			followups = append(followups, "remove_owner action")
		}
		if len(af.RemoveCustomFields) > 0 {
			for _, srcFID := range af.RemoveCustomFields {
				id, ok := cfMap[srcFID]
				if !ok {
					continue
				}
				out = append(out, mappedAction{
					kind:   "remove_custom_field",
					params: map[string]any{"field_id": id},
				})
			}
		}
	case srcActionEmail:
		reasons = append(reasons,
			"email action dropped — suchi has no notify_email verb")
		followups = append(followups, "notify_email action")
	case srcActionWebhook:
		reasons = append(reasons,
			"webhook action dropped — suchi has no webhook verb")
		followups = append(followups, "webhook action")
	case srcActionPasswordRemoval:
		reasons = append(reasons,
			"password_removal action dropped — no suchi equivalent")
		followups = append(followups, "password_removal action")
	case srcActionMoveToTrash:
		out = append(out, mappedAction{
			kind:   "discard",
			params: map[string]any{},
		})
		reasons = append(reasons,
			"move_to_trash mapped to discard (semantic diverges from soft-delete)")
		followups = append(followups, "move_to_trash action")
	default:
		reasons = append(reasons,
			fmt.Sprintf("unknown action type %d dropped", af.Type))
	}

	// Preserve source order into a stable base — the caller-side loop
	// assigns order_index by position, so we just carry base into params
	// for observability. base intentionally unused in params today.
	_ = base
	return out, reasons, followups
}

// writeWorkflow inserts the automations row + child triggers + child actions
// in one transaction. ON CONFLICT(name) DO NOTHING makes re-runs safe.
func writeWorkflow(
	ctx context.Context,
	d *db.DB,
	wf WorkflowFields,
	triggers []suchiTrigger,
	actions []mappedAction,
) error {
	now := time.Now().Unix()
	return d.WriteTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `
			INSERT INTO automations(name, order_index, enabled, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?)
			ON CONFLICT(name) DO NOTHING
		`, wf.Name, wf.Order, boolInt(wf.Enabled), now, now)
		if err != nil {
			return fmt.Errorf("insert automation: %w", err)
		}
		affected, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if affected == 0 {
			// Existing row — do not touch children either. Re-runs are
			// idempotent by name.
			return nil
		}
		autoID, err := res.LastInsertId()
		if err != nil {
			return err
		}

		for _, t := range triggers {
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO automation_triggers(
					automation_id, type,
					filter_path, filter_filename, filter_mailrule_id,
					filter_tag_id, filter_corr_id, filter_doctype_id,
					filter_content_re, created_at
				) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			`,
				autoID, t.typ,
				nullIfEmpty(t.filterPath), nullIfEmpty(t.filterFilename), nullIfZero(t.filterMailrule),
				nullIfZero(t.filterTagID), nullIfZero(t.filterCorrID), nullIfZero(t.filterDocType),
				nullIfEmpty(t.filterContent), now,
			); err != nil {
				return fmt.Errorf("insert trigger: %w", err)
			}
		}

		for i, a := range actions {
			params, err := json.Marshal(a.params)
			if err != nil {
				return fmt.Errorf("marshal action params: %w", err)
			}
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO automation_actions(
					automation_id, order_index, kind, params_json, created_at
				) VALUES (?, ?, ?, ?, ?)
			`, autoID, i, a.kind, string(params), now); err != nil {
				return fmt.Errorf("insert action: %w", err)
			}
		}
		return nil
	})
}

// remapIDs translates source PKs through m, dropping unknowns.
func remapIDs(src []int64, m map[int64]int64) []int64 {
	out := make([]int64, 0, len(src))
	for _, pk := range src {
		if id, ok := m[pk]; ok {
			out = append(out, id)
		}
	}
	return out
}

// joinReasons collapses a reason slice into one semicolon-separated line
// so MigrationReport's markdown table stays one-row-per-entry.
func joinReasons(rs []string) string {
	if len(rs) == 0 {
		return ""
	}
	return strings.Join(rs, "; ")
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func nullIfZero(i int64) any {
	if i == 0 {
		return nil
	}
	return i
}
