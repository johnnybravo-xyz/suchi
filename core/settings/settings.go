// Package settings is a thin typed wrapper over the settings
// key-value table. Every value round-trips as JSON so scalars, lists,
// and structs share one storage shape.
//
// Callers stay away from the raw table — Get + Set here are the only
// public surface, plus the SetupState helpers the wizard uses.
package settings

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/suchi-dms/suchi/core/db"
)

// Well-known keys. Prefer these constants over raw strings so a rename
// is one grep away.
const (
	KeySetupCompletedAt = "setup.completed_at"
	KeySetupStepsDone   = "setup.steps_done"
	KeySetupStepsSkip   = "setup.steps_skipped"

	KeyLLMEndpointURL  = "llm.endpoint_url"
	KeyLLMModel        = "llm.model"
	KeyLLMAPIKeySealed = "llm.api_key_sealed"
	KeyLLMEgressAck    = "llm.egress_ack"

	KeyJDPreset = "jd.preset"

	KeyBackupIntervalHours = "backup.interval_hours"
	KeyOCRLanguages        = "ocr.languages" // JSON array of ISO codes

	KeyFSWatchDir        = "ingest.fs_watch_dir"
	KeyFSWatchOwnerEmail = "ingest.fs_watch_owner"
)

// ErrNotFound signals the key isn't present (distinct from a scan
// error). Callers can use errors.Is.
var ErrNotFound = errors.New("settings: key not found")

// Get reads a single setting into out (must be a pointer). Returns
// ErrNotFound when the key is absent — callers pick defaults.
func Get(ctx context.Context, database *db.DB, key string, out any) error {
	var payload string
	err := database.Read.QueryRowContext(ctx,
		`SELECT value_json FROM settings WHERE key = ?`, key).Scan(&payload)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("read %s: %w", key, err)
	}
	if err := json.Unmarshal([]byte(payload), out); err != nil {
		return fmt.Errorf("unmarshal %s: %w", key, err)
	}
	return nil
}

// Set writes (or overwrites) a key. Serialization runs before the tx
// so a bad shape can't half-commit.
func Set(ctx context.Context, database *db.DB, key string, value any) error {
	payload, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("marshal %s: %w", key, err)
	}
	return database.WriteTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
			INSERT INTO settings (key, value_json, updated_at)
			VALUES (?, ?, ?)
			ON CONFLICT(key) DO UPDATE SET
				value_json = excluded.value_json,
				updated_at = excluded.updated_at
		`, key, string(payload), time.Now().Unix())
		return err
	})
}

// Delete removes a key. No-op if absent.
func Delete(ctx context.Context, database *db.DB, key string) error {
	return database.WriteTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `DELETE FROM settings WHERE key = ?`, key)
		return err
	})
}

// ---------- SetupState — the wizard's specific surface ----------

// StepStatus records per-step outcome. "done" and "skipped" are the
// only observed states; a step not present is implicitly pending.
type StepStatus string

const (
	StepDone    StepStatus = "done"
	StepSkipped StepStatus = "skipped"
)

// SetupState is the wizard's view of onboarding progress.
type SetupState struct {
	CompletedAt *int64                `json:"completed_at,omitempty"` // unix seconds
	Steps       map[string]StepStatus `json:"steps"`
}

// LoadSetupState reads the wizard's state. Missing keys → zero-value.
func LoadSetupState(ctx context.Context, database *db.DB) (*SetupState, error) {
	s := &SetupState{Steps: map[string]StepStatus{}}
	var completedAt int64
	if err := Get(ctx, database, KeySetupCompletedAt, &completedAt); err == nil {
		s.CompletedAt = &completedAt
	} else if !errors.Is(err, ErrNotFound) {
		return nil, err
	}
	var done []string
	if err := Get(ctx, database, KeySetupStepsDone, &done); err != nil && !errors.Is(err, ErrNotFound) {
		return nil, err
	}
	for _, step := range done {
		s.Steps[step] = StepDone
	}
	var skipped []string
	if err := Get(ctx, database, KeySetupStepsSkip, &skipped); err != nil && !errors.Is(err, ErrNotFound) {
		return nil, err
	}
	for _, step := range skipped {
		s.Steps[step] = StepSkipped
	}
	return s, nil
}

// RecordStep marks step as done or skipped. Preserves the OTHER list.
func RecordStep(ctx context.Context, database *db.DB, step string, status StepStatus) error {
	if status != StepDone && status != StepSkipped {
		return fmt.Errorf("bad status %q", status)
	}
	s, err := LoadSetupState(ctx, database)
	if err != nil {
		return err
	}
	// Remove from the opposite list if it existed there. Same-list
	// dupes are handled by the dedup below.
	s.Steps[step] = status
	var done, skipped []string
	for k, v := range s.Steps {
		switch v {
		case StepDone:
			done = append(done, k)
		case StepSkipped:
			skipped = append(skipped, k)
		}
	}
	if err := Set(ctx, database, KeySetupStepsDone, done); err != nil {
		return err
	}
	return Set(ctx, database, KeySetupStepsSkip, skipped)
}

// MarkSetupComplete stamps the wizard-finished timestamp. Idempotent —
// re-runs update the timestamp so admins revisiting see "completed
// most-recent".
func MarkSetupComplete(ctx context.Context, database *db.DB) error {
	return Set(ctx, database, KeySetupCompletedAt, time.Now().Unix())
}

// SetupNeeded reports whether an admin landing on / should be redirected
// to /admin/setup. True when no completion timestamp exists yet.
func SetupNeeded(ctx context.Context, database *db.DB) (bool, error) {
	var completedAt int64
	err := Get(ctx, database, KeySetupCompletedAt, &completedAt)
	if errors.Is(err, ErrNotFound) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	return completedAt == 0, nil
}
