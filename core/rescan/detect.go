// SPDX-License-Identifier: AGPL-3.0-or-later

// Boot-time detector — the discovery half of the rescan-via-
// approvals feature. Called from main.go after the approvals engine
// is registered + the rescan-proposal def is seeded.
//
// For each configured pipeline kind (ocr / llm / content), the detector:
//   1. Counts docs whose pipeline_version_<kind> lags the release's
//      recommended proposal revision.
//   2. If count > 0 and no pending run or dismissal exists for that kind+version,
//      Start()s a rescan-proposal run. The engine's approve state
//      creates a task in the tasks feed automatically.
//   3. If a pending run exists for an OLDER recommended revision,
//      Cancel()s it — the fresh run supersedes.
//   4. If count == 0 and a pending run exists, Cancel()s it —
//      whatever the state was, the archive caught up externally and
//      nagging the operator is stale.
//
// Idempotent: safe to call every boot. No background workers.

package rescan

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"math"

	"github.com/johnnybravo-xyz/suchi/core/approvals"
	"github.com/johnnybravo-xyz/suchi/core/db"
	pluginapi "github.com/johnnybravo-xyz/suchi/plugin-api"
)

// kindsToCheck is the fixed set of proposal revisions the detector walks each
// boot. Order is stable so the log lines are readable.
var kindsToCheck = []string{"ocr", "llm", "content"}

const proposalTargetPreview = 10

var errMalformedProposalVars = fmt.Errorf("rescan: malformed proposal vars")

// ProposalStillNeeded reports whether a rescan proposal still has eligible
// work. target_documents is a bounded preview and is deliberately ignored.
func ProposalStillNeeded(ctx context.Context, d *db.DB, systemID int64, vars map[string]any) (bool, error) {
	kind, ok := vars["kind"].(string)
	if !ok || kind == "" {
		return false, errMalformedProposalVars
	}
	currentVersion, err := proposalTargetVersion(vars)
	if err != nil {
		return false, err
	}

	count, err := CountProposalStale(ctx, d, systemID, kind, currentVersion)
	if err != nil {
		return false, err
	}
	return count > 0, nil
}

func proposalTargetVersion(vars map[string]any) (int, error) {
	var version int
	switch value := vars["current_version"].(type) {
	case float64:
		if value <= 0 || math.Trunc(value) != value {
			return 0, errMalformedProposalVars
		}
		version = int(value)
		if version <= 0 || float64(version) != value {
			return 0, errMalformedProposalVars
		}
	case int:
		version = value
	case int64:
		version = int(value)
		if int64(version) != value {
			return 0, errMalformedProposalVars
		}
	default:
		return 0, errMalformedProposalVars
	}
	if version <= 0 {
		return 0, errMalformedProposalVars
	}
	return version, nil
}

// EnsureProposals is the boot-time entrypoint. Idempotent.
//
// `engine` is the approvals engine (already Set-Default'd by main.go).
// `versions` is checked-in release policy. Zero skips that kind without changing
// an existing reminder. A positive value is the minimum processing revision the
// release recommends; it may lag the runtime's current processing revision.
func EnsureProposals(ctx context.Context, d *db.DB, systemID int64, engine *approvals.Engine, versions Versions) error {
	for _, kind := range kindsToCheck {
		recommended := versionFor(kind, versions)
		if recommended <= 0 {
			// No new proposal policy for this kind. Existing reminders remain
			// visible until resolved or reconciled by a later positive policy.
			continue
		}
		stale, err := CountProposalStale(ctx, d, systemID, kind, recommended)
		if err != nil {
			return fmt.Errorf("rescan.detect: count stale %s: %w", kind, err)
		}
		if err := reconcile(ctx, d, systemID, engine, kind, recommended, stale); err != nil {
			return fmt.Errorf("rescan.detect: reconcile %s: %w", kind, err)
		}
	}
	return nil
}

// versionFor picks one configured revision from Versions by kind.
func versionFor(kind string, v Versions) int {
	switch kind {
	case "ocr":
		return v.OCR
	case "llm":
		return v.LLM
	case "content":
		return v.Content
	}
	return 0
}

// reconcile is the per-kind decision: cancel stale pending runs,
// start a new one if warranted, or leave things be.
func reconcile(ctx context.Context, d *db.DB, systemID int64, engine *approvals.Engine, kind string, recommended, stale int) error {
	pending, err := findPendingRun(ctx, d, systemID, kind)
	if err != nil {
		return err
	}
	var pendingVersion int
	if pending != nil {
		pendingVersion, err = proposalTargetVersion(pending.Vars)
		if err != nil {
			return fmt.Errorf("decode pending proposal revision: %w", err)
		}
		if pendingVersion > recommended {
			// A lower recommendation must not hide a newer reminder that was
			// already offered by an earlier tagged release.
			return nil
		}
	}
	// Case 1: no stale docs. Any pending run is now obsolete.
	if stale == 0 {
		if pending != nil {
			return engine.Cancel(ctx, pending.ID, "detect: stale count reached 0", systemActor())
		}
		return nil
	}
	// Case 2: pending run exists at a stale version. Supersede.
	if pending != nil {
		if pendingVersion < recommended {
			if err := engine.Cancel(ctx, pending.ID, "detect: superseded by newer version", systemActor()); err != nil {
				return err
			}
			pending = nil
		}
	}
	// Dismissal is durable as soon as Resolve commits, even if its advance
	// job has not finished. Reuse that history across definition versions.
	// An explicitly approved duplicate must still finish its queued work.
	var pendingID int64
	if pending != nil {
		pendingID = pending.ID
	}
	var dismissed bool
	err = d.Read.QueryRowContext(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM approval_runs r
			JOIN approval_defs def ON def.id = r.def_id
			JOIN approval_tasks t ON t.run_id = r.id
			WHERE r.system_id = ? AND def.slug = ?
			  AND json_extract(r.vars_json, '$.kind') = ?
			  AND json_extract(r.vars_json, '$.current_version') = ?
			  AND t.status = 'resolved' AND t.resolved_choice = 'dismiss'
		) AND NOT EXISTS (
			SELECT 1 FROM approval_tasks
			WHERE run_id = ? AND status = 'resolved'
			  AND resolved_choice IN ('approve_all', 'approve_sample')
		)
	`, systemID, ProposalSlug, kind, recommended, pendingID).Scan(&dismissed)
	if err != nil {
		return fmt.Errorf("check rescan dismissal for %s: %w", kind, err)
	}
	if dismissed {
		if pending != nil {
			return engine.Cancel(ctx, pending.ID, "detect: target revision dismissed", systemActor())
		}
		return nil
	}
	// Case 3: pending run at the current version — nothing to do.
	// (Even if stale_count drifted, the operator sees the card and
	// can act; we don't chase every count change.)
	if pending != nil {
		return nil
	}
	// Case 4: no pending run, but stale > 0. Start one.
	targets, err := ProposalTargets(ctx, d, systemID, kind, recommended, proposalTargetPreview)
	if err != nil {
		return fmt.Errorf("load rescan-proposal targets for %s: %w", kind, err)
	}
	vars := map[string]any{
		"kind": kind,
		// Retain the persisted field name for proposals created by older
		// builds; it now carries the tagged recommendation threshold.
		"current_version":  recommended,
		"stale_count":      stale,
		"target_documents": targets,
	}
	runID, err := engine.Start(ctx, systemID, ProposalSlug, 0, vars, systemActor())
	if err != nil {
		return fmt.Errorf("start rescan-proposal for %s: %w", kind, err)
	}
	if engine != nil {
		_ = runID // logging happens inside engine.Start
	}
	return nil
}

// findPendingRun returns the running rescan-proposal run for the
// given kind, if one exists. Uses a JOIN on approval_defs to filter
// by slug; approval_runs' vars_json is queried by JSON extract for
// the kind match.
//
// Returns (nil, nil) when no matching run — not an error.
func findPendingRun(ctx context.Context, d *db.DB, systemID int64, kind string) (*approvals.Run, error) {
	var (
		id           int64
		defID        int64
		currentState string
		varsJSON     sql.NullString
	)
	err := d.Read.QueryRowContext(ctx, `
		SELECT r.id, r.def_id, r.current_state, r.vars_json
		  FROM approval_runs r
		  JOIN approval_defs def ON def.id = r.def_id
		 WHERE r.system_id = ? AND def.slug = ?
		   AND r.state = 'running'
		   AND json_extract(r.vars_json, '$.kind') = ?
		 ORDER BY r.id DESC
		 LIMIT 1
	`, systemID, ProposalSlug, kind).Scan(&id, &defID, &currentState, &varsJSON)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	run := &approvals.Run{
		SystemID:     systemID,
		ID:           id,
		DefID:        defID,
		CurrentState: currentState,
	}
	if varsJSON.Valid {
		vars := map[string]any{}
		if err := json.Unmarshal([]byte(varsJSON.String), &vars); err != nil {
			return nil, fmt.Errorf("decode vars_json: %w", err)
		}
		run.Vars = vars
	}
	return run, nil
}

// systemActor returns a synthesized "system" principal for detector-
// initiated Start/Cancel calls. UserID=0 flows through to
// approval_runs.started_by = NULL — the operator sees runs without
// a human attribution, which is right.
func systemActor() *pluginapi.Principal {
	return &pluginapi.Principal{Kind: "system"}
}
