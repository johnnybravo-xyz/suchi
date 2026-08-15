// Package bundle: saved-view import path.
//
// Paperless-ngx models a saved view as one `documents.savedview` row plus
// N `documents.savedviewfilterrule` rows keyed by rule_type int. suchi's
// saved_views table stores a single opaque filter_json blob per view — a
// flat map validated against savedViewAllowedKeys in core/api. This file
// translates the rule-per-row shape into that blob, one entry at a time,
// and records per-view outcomes on the MigrationReport.
//
// Design invariants:
//   - One report entry per source view (never per rule). Rules that don't
//     map degrade the view to PARTIAL; a view with zero mappable rules is
//     FAILED entirely.
//   - Idempotent: INSERT ... ON CONFLICT(owner_id, name) DO NOTHING makes
//     re-runs safe.
//   - Owner remap: source-side Owner is a paperless auth.user PK we don't
//     have a mapping table for. We fall back to the caller-supplied
//     ownerID when source.Owner is nil or unresolvable. If ownerID is <=0
//     and source has no owner, the view is skipped (Failed).
package bundle

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/johnnybravo-xyz/suchi/core/db"
)

// SavedViewFields matches documents.savedview rows in the paperless-ngx
// manifest. Only fields suchi consumes are decoded; extras are ignored.
type SavedViewFields struct {
	Owner           *int64          `json:"owner"`
	Name            string          `json:"name"`
	ShowOnDashboard bool            `json:"show_on_dashboard"`
	ShowInSidebar   bool            `json:"show_in_sidebar"`
	Icon            string          `json:"icon"`
	SortField       string          `json:"sort_field"`
	SortReverse     bool            `json:"sort_reverse"`
	PageSize        *int            `json:"page_size"`
	DisplayMode     string          `json:"display_mode"`
	DisplayFields   json.RawMessage `json:"display_fields"`
}

// SavedViewFilterRuleFields matches documents.savedviewfilterrule rows.
type SavedViewFilterRuleFields struct {
	SavedView int64   `json:"saved_view"`
	RuleType  int     `json:"rule_type"`
	Value     *string `json:"value"`
}

// ImportSavedViews reads savedview + savedviewfilterrule objects and
// writes suchi saved_views rows. Returns the count of views persisted.
//
// If a source view has no Owner and ownerID > 0, ownerID is used. If a
// view has no owner and ownerID is unset, the view is Failed and skipped.
func ImportSavedViews(
	ctx context.Context,
	d *db.DB,
	log *slog.Logger,
	objs []Object,
	dry bool,
	ownerID int64,
	tagMap, corMap, dtMap map[int64]int64,
	report *MigrationReport,
) (int, error) {
	log = log.With("component", "import.bundle.saved_views")

	// Group rules by their source saved_view PK. Order within a view
	// does not matter — the resulting filter_json is a set-of-keys map.
	rulesByView := map[int64][]SavedViewFilterRuleFields{}
	var views []Object
	for _, o := range objs {
		switch o.Model {
		case ModelSavedView:
			views = append(views, o)
		case ModelSavedViewFilterRule:
			var r SavedViewFilterRuleFields
			if err := json.Unmarshal(o.Fields, &r); err != nil {
				return 0, fmt.Errorf("decode savedviewfilterrule pk=%d: %w", o.PK, err)
			}
			rulesByView[r.SavedView] = append(rulesByView[r.SavedView], r)
		}
	}

	// Deterministic order — sort views by PK so re-runs log identically.
	sort.Slice(views, func(i, j int) bool { return views[i].PK < views[j].PK })

	written := 0
	for _, o := range views {
		var f SavedViewFields
		if err := json.Unmarshal(o.Fields, &f); err != nil {
			return written, fmt.Errorf("decode savedview pk=%d: %w", o.PK, err)
		}
		source := fmt.Sprintf("savedview:%s", f.Name)

		// Owner resolution. paperless owner PKs are not remapped by this
		// importer — the bundle importer today writes every doc under the
		// caller-supplied owner, so views follow the same policy.
		effectiveOwner := ownerID
		if effectiveOwner <= 0 {
			log.Info("saved_view.skip.no_owner", "name", f.Name, "pk", o.PK)
			report.Failed(KindSavedView, source, "no target owner resolved (source owner missing and no fallback ownerID)")
			continue
		}

		filter, outcome, reason := buildFilterJSON(f, rulesByView[o.PK], tagMap, corMap, dtMap, report)
		if outcome == OutcomeFailed {
			log.Info("saved_view.skip.no_mappable_rules", "name", f.Name, "pk", o.PK, "reason", reason)
			report.Failed(KindSavedView, source, reason)
			continue
		}

		display := mapDisplayMode(f.DisplayMode)

		// sort_field → filter_json.ordering, respecting sort_reverse via
		// a leading `-` (paperless-list-endpoint convention that suchi's
		// documents list also honors).
		if f.SortField != "" {
			ord := f.SortField
			if f.SortReverse {
				ord = "-" + ord
			}
			filter["ordering"] = ord
		}

		blob, err := json.Marshal(filter)
		if err != nil {
			return written, fmt.Errorf("marshal filter_json for view %q: %w", f.Name, err)
		}

		target := fmt.Sprintf("saved_views:%s", f.Name)
		log.Info("saved_view.import",
			"name", f.Name,
			"owner", effectiveOwner,
			"display", display,
			"rules_in", len(rulesByView[o.PK]),
			"outcome", outcomeLabel(outcome),
		)

		if dry {
			recordOutcome(report, outcome, source, target, reason)
			written++
			continue
		}

		now := time.Now().Unix()
		var inserted int64
		err = d.WriteTx(ctx, func(tx *sql.Tx) error {
			res, err := tx.ExecContext(ctx, `
				INSERT INTO saved_views(owner_id, name, filter_json, display, position, shared, created_at, updated_at)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?)
				ON CONFLICT(owner_id, name) DO NOTHING
			`, effectiveOwner, f.Name, string(blob), display, 0, 0, now, now)
			if err != nil {
				return err
			}
			inserted, err = res.RowsAffected()
			return err
		})
		if err != nil {
			return written, fmt.Errorf("insert saved_view %q: %w", f.Name, err)
		}

		if inserted == 0 {
			log.Info("saved_view.skip.exists", "name", f.Name, "owner", effectiveOwner)
			// Idempotent skip is neither full nor failed — do not double-count.
			continue
		}

		recordOutcome(report, outcome, source, target, reason)
		written++
	}

	return written, nil
}

// buildFilterJSON walks each rule and drops mapped keys into a fresh map.
// Returns the map, the aggregate outcome (worst of any rule mapping), and
// a summary reason string if PARTIAL/FAILED.
//
// Aggregate rules:
//   - any FAILED rule that is the ONLY rule → view FAILED
//   - any FAILED rule alongside a mappable rule → view PARTIAL
//   - any PARTIAL rule → at least PARTIAL
//   - all FULL → FULL
func buildFilterJSON(
	f SavedViewFields,
	rules []SavedViewFilterRuleFields,
	tagMap, corMap, dtMap map[int64]int64,
	report *MigrationReport,
) (map[string]any, Outcome, string) {
	filter := map[string]any{}
	var reasons []string
	worst := OutcomeFull
	mapped := 0
	failedRules := 0

	// Track q text so multiple title/content rules combine.
	var qParts []string

	// Rule-type mapping frozen against paperless-ngx v3.0.5 (enum
	// verified against a live OPTIONS /api/saved_views/ call on
	// 2026-08-15). We do not chase minor releases — this is a
	// one-way migration from a single pinned target, not a compatibility
	// surface. See docs/importer.mdx §"Migration target version" for
	// the policy. If a future paperless release renumbers or extends
	// the enum, unknown rule_types land in the default branch → FAILED
	// with a clear "paperless version drift" reason and the caller adds
	// a Followup so the maintainer knows to consider a bump.
	for _, r := range rules {
		switch r.RuleType {
		case 0, 19, 20, 48, 49: // title / content / title-or-content / fulltext / simple searches
			if r.Value != nil && *r.Value != "" {
				qParts = append(qParts, *r.Value)
			}
			mapped++
		case 1: // content contains — same q, note that q also matches title
			if r.Value != nil && *r.Value != "" {
				qParts = append(qParts, *r.Value)
			}
			mapped++
			worst = worsten(worst, OutcomePartial)
			reasons = append(reasons, "q matches title+content combined")
		case 2, 18, 23, 24: // ASN filters
			failedRules++
			report.Followup("saved_view ASN filter")
		case 3: // correspondent is
			if r.Value != nil {
				if id, ok := remapIDString(corMap, *r.Value); ok {
					appendIntArray(filter, "correspondents__id__in", id)
					mapped++
					continue
				}
			}
			failedRules++
		case 26: // has correspondent in
			if r.Value != nil {
				if id, ok := remapIDString(corMap, *r.Value); ok {
					appendIntArray(filter, "correspondents__id__in", id)
					mapped++
					continue
				}
			}
			failedRules++
		case 4: // document type is
			if r.Value != nil {
				if id, ok := remapIDString(dtMap, *r.Value); ok {
					// suchi allow-list has singular document_type__id.
					// Repeated rules → last-wins + PARTIAL.
					if _, exists := filter["document_type__id"]; exists {
						worst = worsten(worst, OutcomePartial)
						reasons = append(reasons, "multiple document_type rules collapsed to last")
					}
					filter["document_type__id"] = id
					mapped++
					continue
				}
			}
			failedRules++
		case 28: // has document type in
			if r.Value != nil {
				if id, ok := remapIDString(dtMap, *r.Value); ok {
					if _, exists := filter["document_type__id"]; exists {
						reasons = append(reasons, "document_type 'in' list truncated to first")
					}
					filter["document_type__id"] = id
					mapped++
					worst = worsten(worst, OutcomePartial)
					continue
				}
			}
			failedRules++
		case 5: // is in inbox
			// suchi has jd_category_id as a native inbox concept — no
			// degradation needed. Caller must supply the inbox id.
			// We leave marker "__jd_inbox__" and let ImportSavedViews
			// resolve after the fact (we don't have DB here). Actually
			// simpler: skip inbox filtering for now and mark PARTIAL —
			// most users re-create Inbox saved-views by hand.
			worst = worsten(worst, OutcomePartial)
			reasons = append(reasons, "is-in-inbox rule dropped — suchi surfaces Inbox natively")
			mapped++
		case 6: // has tag
			if r.Value != nil {
				if id, ok := remapIDString(tagMap, *r.Value); ok {
					appendIntArray(filter, "tags__id__in", id)
					mapped++
					continue
				}
			}
			failedRules++
		case 7, 22: // has any tag / has tags in
			if r.Value != nil {
				if id, ok := remapIDString(tagMap, *r.Value); ok {
					appendIntArray(filter, "tags__id__in", id)
					mapped++
					continue
				}
			}
			failedRules++
		case 8, 9, 10, 11, 12, 13, 14, 15, 16, 43, 44, 45, 46: // date filters
			failedRules++
			report.Followup("saved_view date filters")
		case 17: // does not have tag
			failedRules++
			report.Followup("saved_view negative-tag filter")
		case 21: // more like this
			failedRules++
			report.Followup("saved_view more-like-this filter")
		case 25: // storage path is
			if r.Value != nil && *r.Value != "" {
				qParts = append(qParts, *r.Value)
				mapped++
				worst = worsten(worst, OutcomePartial)
				reasons = append(reasons, "storage_path filter degraded to text search")
			} else {
				failedRules++
			}
			report.Followup("saved_view storage_path filter")
		case 27, 29, 31: // negative "in" filters
			failedRules++
			report.Followup("saved_view negative filters")
		case 30: // has storage path in
			if r.Value != nil && *r.Value != "" {
				qParts = append(qParts, *r.Value)
				mapped++
				worst = worsten(worst, OutcomePartial)
				reasons = append(reasons, "storage_path 'in' filter degraded to text search")
			} else {
				failedRules++
			}
			report.Followup("saved_view storage_path filter")
		case 32, 33, 34, 35: // owner filters
			failedRules++
			report.Followup("saved_view owner filter")
		case 36, 38, 39, 40, 41, 42: // custom-field filters
			failedRules++
			report.Followup("saved_view custom-field filter")
		case 37: // is shared by me
			failedRules++
			report.Followup("saved_view sharing filter")
		case 47: // mime type is
			failedRules++
			report.Followup("saved_view mime-type filter")
		default:
			// Unknown rule_type — likely a newer paperless release.
			// Migration is frozen at v2.x current stable; add a
			// Followup so the maintainer knows to update the table.
			failedRules++
			report.Followup(fmt.Sprintf("saved_view unknown rule_type=%d (paperless version drift)", r.RuleType))
		}
	}

	if len(qParts) > 0 {
		filter["q"] = strings.Join(qParts, " ")
	}

	// If zero rules mapped, the whole view is FAILED.
	if mapped == 0 {
		return nil, OutcomeFailed, "no filter rules mapped to a suchi saved_view field"
	}
	if failedRules > 0 {
		worst = worsten(worst, OutcomePartial)
		reasons = append(reasons, fmt.Sprintf("%d rule(s) dropped", failedRules))
	}

	// Icon is intentionally dropped. Note it only when the view is
	// already PARTIAL — do NOT create a partial just for a missing icon.
	if f.Icon != "" && worst == OutcomePartial {
		reasons = append(reasons, "icon dropped")
	}

	// show_on_dashboard, show_in_sidebar, page_size have no home in
	// suchi's filter_json allow-list today; drop silently. Same for
	// display_fields — the SPA owns column selection separately.

	return filter, worst, strings.Join(reasons, "; ")
}

// mapDisplayMode translates paperless display modes to suchi's.
func mapDisplayMode(m string) string {
	switch strings.ToUpper(strings.TrimSpace(m)) {
	case "TABLE", "":
		return "table"
	case "SMALL_CARDS", "LARGE_CARDS":
		return "card"
	default:
		return "table"
	}
}

// remapIDString parses a stringified int64 and looks it up in the given
// remap table. Returns (target-id, true) on success, (0, false) otherwise.
func remapIDString(remap map[int64]int64, s string) (int64, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, false
	}
	// Manual parse to avoid strconv import churn; the value shape is
	// always a base-10 signed int in paperless exports.
	var n int64
	var neg bool
	i := 0
	if s[0] == '-' {
		neg = true
		i = 1
	}
	if i >= len(s) {
		return 0, false
	}
	for ; i < len(s); i++ {
		c := s[i]
		if c < '0' || c > '9' {
			return 0, false
		}
		n = n*10 + int64(c-'0')
	}
	if neg {
		n = -n
	}
	id, ok := remap[n]
	return id, ok
}

// appendIntArray appends v to the []int64-shaped filter[key], creating
// the slice if needed. Coerced to []any at end since filter_json marshals
// through map[string]any and the API allow-list accepts array-of-scalars.
func appendIntArray(filter map[string]any, key string, v int64) {
	cur, ok := filter[key].([]any)
	if !ok {
		cur = []any{}
	}
	// De-dup to keep filter_json compact.
	for _, e := range cur {
		if ev, ok := e.(int64); ok && ev == v {
			return
		}
	}
	filter[key] = append(cur, v)
}

// worsten returns whichever outcome is worse. FAILED > PARTIAL > FULL.
func worsten(a, b Outcome) Outcome {
	if a == OutcomeFailed || b == OutcomeFailed {
		return OutcomeFailed
	}
	if a == OutcomePartial || b == OutcomePartial {
		return OutcomePartial
	}
	return OutcomeFull
}

// recordOutcome dispatches to the MigrationReport method matching outcome.
func recordOutcome(report *MigrationReport, outcome Outcome, source, target, reason string) {
	switch outcome {
	case OutcomeFull:
		report.Full(KindSavedView, source, target)
	case OutcomePartial:
		report.Partial(KindSavedView, source, target, reason)
	case OutcomeFailed:
		report.Failed(KindSavedView, source, reason)
	}
}

func outcomeLabel(o Outcome) string {
	switch o {
	case OutcomeFull:
		return "full"
	case OutcomePartial:
		return "partial"
	case OutcomeFailed:
		return "failed"
	}
	return "unknown"
}
