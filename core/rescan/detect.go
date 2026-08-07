// Boot-time detector — the discovery half of the rescan-via-
// approvals feature. Called from main.go after the approvals engine
// is registered + the rescan-proposal def is seeded.
//
// For each pipeline kind (ocr / llm / content), the detector:
//   1. Counts docs whose pipeline_version_<kind> lags the current
//      binary's constant.
//   2. If count > 0 and no pending run exists for that kind+version,
//      Start()s a rescan-proposal run. The engine's approve state
//      creates a task in the tasks feed automatically.
//   3. If a pending run exists for an OLDER version (constant bumped
//      since last boot), Cancel()s it — the fresh run supersedes.
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

	"github.com/johnnybravo-xyz/suchi/core/approvals"
	"github.com/johnnybravo-xyz/suchi/core/db"
	pluginapi "github.com/johnnybravo-xyz/suchi/plugin-api"
)

// kindsToCheck is the fixed set of pipeline versions the detector
// walks each boot. Order is stable so the log lines are readable.
var kindsToCheck = []string{"ocr", "llm", "content"}

// EnsureProposals is the boot-time entrypoint. Idempotent.
//
// `engine` is the approvals engine (already Set-Default'd by main.go).
// `versions` is the pipeline snapshot from main.go (postingest.PipelineVersion*
// + llmclassifier.PipelineVersionLLM).
func EnsureProposals(ctx context.Context, d *db.DB, engine *approvals.Engine, versions Versions) error {
	for _, kind := range kindsToCheck {
		current := versionFor(kind, versions)
		if current <= 0 {
			continue // kind's version is disabled (zero); nothing to detect against
		}
		stale, err := CountStale(ctx, d, kind, current)
		if err != nil {
			return fmt.Errorf("rescan.detect: count stale %s: %w", kind, err)
		}
		if err := reconcile(ctx, d, engine, kind, current, stale); err != nil {
			return fmt.Errorf("rescan.detect: reconcile %s: %w", kind, err)
		}
	}
	return nil
}

// versionFor picks the current constant from Versions by kind.
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
func reconcile(ctx context.Context, d *db.DB, engine *approvals.Engine, kind string, current, stale int) error {
	pending, err := findPendingRun(ctx, d, kind)
	if err != nil {
		return err
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
		pendingVer, _ := pending.Vars["current_version"].(float64)
		if int(pendingVer) < current {
			if err := engine.Cancel(ctx, pending.ID, "detect: superseded by newer version", systemActor()); err != nil {
				return err
			}
			pending = nil
		}
	}
	// Case 3: pending run at the current version — nothing to do.
	// (Even if stale_count drifted, the operator sees the card and
	// can act; we don't chase every count change.)
	if pending != nil {
		return nil
	}
	// Case 4: no pending run, but stale > 0. Start one.
	vars := map[string]any{
		"kind":            kind,
		"current_version": current,
		"stale_count":     stale,
	}
	runID, err := engine.Start(ctx, ProposalSlug, 0, vars, systemActor())
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
func findPendingRun(ctx context.Context, d *db.DB, kind string) (*approvals.Run, error) {
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
		 WHERE def.slug = ?
		   AND r.state = 'running'
		   AND json_extract(r.vars_json, '$.kind') = ?
		 ORDER BY r.id DESC
		 LIMIT 1
	`, ProposalSlug, kind).Scan(&id, &defID, &currentState, &varsJSON)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	run := &approvals.Run{
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
