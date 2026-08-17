// apply_llm_title — the second built-in action.
//
// The LLM classifier plugin writes a `document_proposals` row with
// field='title' when a doc already has a non-empty title AND the LLM
// suggests a different one. This action, wired behind the
// `apply_llm_title` system automation, reads that proposal and either
// auto-applies it or leaves it in the Tasks inbox for the operator
// to accept manually:
//
//   - res.Confidence >= threshold → apply.
//   - Otherwise → leave the proposal pending.
//
// The empty-current-title case is handled by the LLM plugin directly
// (fast path, no proposal written) — this action never sees it.
//
// Toggle the parent automation off to disable auto-apply — pending
// proposals still surface in the Tasks inbox for manual accept.
//
// Params:
//
//	{ "threshold": 0.7 }
//
// Threshold is the ONE user-tunable knob on this built-in — everything
// else (name, trigger, action kind) is locked. Mirrors the
// apply_from_similar convention where the action's params carry the
// operator's tunable state. See docs/pipeline-versions.mdx.
//
// Runs inside the automations WriteTx so title write, proposal
// resolution, and audit event commit together.

package automations

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/johnnybravo-xyz/suchi/core/audit"
)

// SystemSlugApplyLLMTitle + DefaultLLMTitleThreshold live in seed.go
// alongside the other built-ins' constants — one place to see every
// slug + tuning default at a glance.

// runApplyLLMTitle is the action dispatched by runAction when Kind =
// "apply_llm_title". Reads `threshold` from action params; falls back
// to DefaultLLMTitleThreshold when unset or non-numeric.
func runApplyLLMTitle(ctx context.Context, tx *sql.Tx, log *slog.Logger, docID int64, a Action) error {
	threshold := DefaultLLMTitleThreshold
	if t, ok := a.Params["threshold"].(float64); ok && t > 0 {
		threshold = t
	}

	// Latest pending title proposal wins if multiple exist (the plugin
	// only writes one per classify, but a rescan could produce a
	// second before the first was resolved). No-op if the doc has
	// none — that's the empty-current-title fast path in the plugin.
	var (
		proposalID int64
		valueJSON  string
		confidence float64
	)
	err := tx.QueryRowContext(ctx, `
		SELECT id, value_json, confidence
		  FROM document_proposals
		 WHERE document_id = ? AND field = 'title' AND resolved_at IS NULL
		 ORDER BY id DESC
		 LIMIT 1
	`, docID).Scan(&proposalID, &valueJSON, &confidence)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("apply_llm_title: load proposal: %w", err)
	}

	var cache struct {
		Label string `json:"label"`
	}
	if err := json.Unmarshal([]byte(valueJSON), &cache); err != nil {
		return fmt.Errorf("apply_llm_title: parse value_json: %w", err)
	}
	if cache.Label == "" {
		return nil // no-op on malformed proposal — leave for humans
	}

	if confidence < threshold {
		log.Info("apply_llm_title.left_pending",
			"doc_id", docID,
			"proposal_id", proposalID,
			"confidence", confidence,
			"threshold", threshold)
		return nil
	}

	now := time.Now().Unix()
	if _, err := tx.ExecContext(ctx,
		`UPDATE documents SET title = ?, updated_at = ? WHERE id = ?`,
		cache.Label, now, docID); err != nil {
		return fmt.Errorf("apply_llm_title: write title: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE document_proposals
		   SET resolved_at = ?, resolution = 'applied'
		 WHERE id = ? AND resolved_at IS NULL
	`, now, proposalID); err != nil {
		return fmt.Errorf("apply_llm_title: resolve proposal: %w", err)
	}
	audit.LogInTx(ctx, tx, log, audit.Event{
		Actor:      nil, // system actor
		Action:     "apply_llm_title.applied",
		ObjectKind: "document",
		ObjectID:   docID,
		After: map[string]any{
			"field":       "title",
			"label":       cache.Label,
			"confidence":  confidence,
			"threshold":   threshold,
			"proposal_id": proposalID,
		},
	})
	log.Info("apply_llm_title.applied",
		"doc_id", docID,
		"proposal_id", proposalID,
		"confidence", confidence)
	return nil
}
