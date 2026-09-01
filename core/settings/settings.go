// Package settings is a thin typed wrapper over the settings
// key-value table. Every value round-trips as JSON so scalars, lists,
// and structs share one storage shape.
//
// Callers stay away from the raw table and use the typed helpers here.
package settings

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"time"

	"github.com/johnnybravo-xyz/suchi/core/db"
)

// Well-known keys. Prefer these constants over raw strings so a rename
// is one grep away.
const (
	KeySetupCompletedAt = "setup.completed_at"
	KeySetupIntent      = "setup.intent"

	KeyLLMEndpointURL   = "llm.endpoint_url"
	KeyLLMModel         = "llm.model"
	KeyLLMAPIKeySealed  = "llm.api_key_sealed"
	KeyLLMEgressAck     = "llm.egress_ack"
	KeyLLMDisabled      = "llm.disabled"
	KeyLLMConfidence    = "llm.confidence_threshold"
	KeyLLMDateAutoApply = "llm.date_auto_apply"
	KeyResearchContext  = "llm.research_context_mode"
	KeyArchiveEnabled   = "classification.archive_enabled"
	KeyArchiveAuto      = "classification.archive_auto_threshold"
	KeyArchiveReview    = "classification.archive_review_threshold"

	KeyPreset = "preset"

	KeyBackupIntervalHours = "backup.interval_hours"
	KeyOCRLanguages        = "ocr.languages" // JSON array of ISO codes

	KeyFSWatchDir        = "ingest.fs_watch_dir"
	KeyFSWatchOwnerEmail = "ingest.fs_watch_owner"
)

type ResearchContextMode string

const (
	ResearchContextFocused  ResearchContextMode = "focused"
	ResearchContextBalanced ResearchContextMode = "balanced"
	ResearchContextDetailed ResearchContextMode = "detailed"
)

func (mode ResearchContextMode) Valid() bool {
	switch mode {
	case ResearchContextFocused, ResearchContextBalanced, ResearchContextDetailed:
		return true
	default:
		return false
	}
}

// ResolveResearchContextMode reads the operator-owned retrieval preset. A
// missing, malformed, or obsolete value falls back to Balanced so a bad row
// can never enlarge or disable the evidence boundary.
func ResolveResearchContextMode(ctx context.Context, database *db.DB) ResearchContextMode {
	var mode ResearchContextMode
	if err := Get(ctx, database, KeyResearchContext, &mode); err != nil || !mode.Valid() {
		return ResearchContextBalanced
	}
	return mode
}

func SaveResearchContextMode(ctx context.Context, database *db.DB, mode ResearchContextMode) error {
	if !mode.Valid() {
		return fmt.Errorf("settings: invalid research context mode %q", mode)
	}
	return Set(ctx, database, KeyResearchContext, mode)
}

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

func Set(ctx context.Context, database *db.DB, key string, value any) error {
	return SetMany(ctx, database, map[string]any{key: value})
}

// SetMany writes all values in one transaction.
func SetMany(ctx context.Context, database *db.DB, values map[string]any) error {
	payloads := make(map[string]string, len(values))
	keys := make([]string, 0, len(values))
	for key, value := range values {
		payload, err := json.Marshal(value)
		if err != nil {
			return fmt.Errorf("marshal %s: %w", key, err)
		}
		payloads[key] = string(payload)
		keys = append(keys, key)
	}
	sort.Strings(keys)

	return database.WriteTx(ctx, func(tx *sql.Tx) error {
		now := time.Now().Unix()
		for _, key := range keys {
			if _, err := tx.ExecContext(ctx, `
					INSERT INTO settings (key, value_json, updated_at)
					VALUES (?, ?, ?)
					ON CONFLICT(key) DO UPDATE SET
						value_json = excluded.value_json,
						updated_at = excluded.updated_at
				`, key, payloads[key], now); err != nil {
				return err
			}
		}
		return nil
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

// SetupState is the wizard's view of onboarding progress.
type SetupState struct {
	CompletedAt       *int64 `json:"completed_at,omitempty"` // unix seconds
	StartedAt         *int64 `json:"started_at,omitempty"`   // first admin creation
	Intent            string `json:"intent,omitempty"`
	RecommendedPreset string `json:"recommended_preset,omitempty"`
	CurrentPreset     string `json:"current_preset,omitempty"`
	FilingTreeChosen  bool   `json:"filing_tree_chosen"`
}

// LoadSetupState reads the wizard's state. Missing keys → zero-value.
func LoadSetupState(ctx context.Context, database *db.DB) (*SetupState, error) {
	s := &SetupState{}
	var startedAt sql.NullInt64
	if err := database.Read.QueryRowContext(ctx,
		`SELECT MIN(created_at) FROM users WHERE role = 'admin'`).Scan(&startedAt); err != nil {
		return nil, fmt.Errorf("read setup start: %w", err)
	}
	if startedAt.Valid && startedAt.Int64 > 0 {
		value := startedAt.Int64
		s.StartedAt = &value
	}
	var completedAt int64
	if err := Get(ctx, database, KeySetupCompletedAt, &completedAt); err == nil {
		s.CompletedAt = &completedAt
	} else if !errors.Is(err, ErrNotFound) {
		return nil, err
	}
	if err := Get(ctx, database, KeySetupIntent, &s.Intent); err != nil && !errors.Is(err, ErrNotFound) {
		return nil, err
	}
	if err := Get(ctx, database, KeyPreset, &s.CurrentPreset); err != nil && !errors.Is(err, ErrNotFound) {
		return nil, err
	}
	chosen, err := FilingTreeChosen(ctx, database)
	if err != nil {
		return nil, err
	}
	s.FilingTreeChosen = chosen
	return s, nil
}

// FilingTreeChosen reports whether an operator has explicitly selected or
// built a filing tree. The protected Inbox-only bootstrap does not count.
func FilingTreeChosen(ctx context.Context, database *db.DB) (bool, error) {
	var chosen bool
	if err := database.Read.QueryRowContext(ctx, `
		SELECT EXISTS(SELECT 1 FROM jd_categories WHERE system = 0)
		    OR EXISTS(SELECT 1 FROM settings WHERE key IN (?, 'taxonomy_preset_id'))
	`, KeyPreset).Scan(&chosen); err != nil {
		return false, fmt.Errorf("read filing-tree choice: %w", err)
	}
	return chosen, nil
}

// MarkSetupComplete stamps the wizard-finished timestamp. Idempotent —
// re-runs update the timestamp so admins revisiting see "completed
// most-recent".
func MarkSetupComplete(ctx context.Context, database *db.DB) error {
	return Set(ctx, database, KeySetupCompletedAt, time.Now().Unix())
}

// ---------- resolvers — database defaults, explicit boot config wins ----------

// LLMConfig is the shape callers merge into their plugin config. Uses
// plain scalars so this package doesn't import plugins/*.
type LLMConfig struct {
	EndpointURL         string
	Model               string
	APIKey              string
	EgressAck           bool
	ConfidenceThreshold float64
	DateAutoApply       bool
	// Disabled is persisted separately from EndpointURL so a web-managed
	// classifier can be turned off without erasing its setup.
	Disabled bool
}

// ArchiveClassifierConfig controls the local similar-document classifier.
// It is intentionally small: retrieval and supported fields are product
// behavior, while operators only choose whether and how confidently to apply.
type ArchiveClassifierConfig struct {
	Enabled         bool
	AutoThreshold   float64
	ReviewThreshold float64
}

func ResolveArchiveClassifierConfig(ctx context.Context, database *db.DB) ArchiveClassifierConfig {
	out := ArchiveClassifierConfig{Enabled: true, AutoThreshold: 0.9, ReviewThreshold: 0.5}
	var enabled bool
	if err := Get(ctx, database, KeyArchiveEnabled, &enabled); err == nil {
		out.Enabled = enabled
	}
	var threshold float64
	if err := Get(ctx, database, KeyArchiveAuto, &threshold); err == nil && threshold > 0 {
		out.AutoThreshold = threshold
	}
	threshold = 0
	if err := Get(ctx, database, KeyArchiveReview, &threshold); err == nil && threshold > 0 {
		out.ReviewThreshold = threshold
	}
	return out
}

func SaveArchiveClassifierConfig(ctx context.Context, database *db.DB, cfg ArchiveClassifierConfig) error {
	return SetMany(ctx, database, map[string]any{
		KeyArchiveEnabled: cfg.Enabled,
		KeyArchiveAuto:    cfg.AutoThreshold,
		KeyArchiveReview:  cfg.ReviewThreshold,
	})
}

type SecretBox interface {
	Seal([]byte) ([]byte, error)
	Open([]byte) ([]byte, error)
}

type sealedSecret struct {
	Version    int    `json:"version"`
	Ciphertext []byte `json:"ciphertext"`
}

// SaveLLMConfig persists public fields and optionally replaces the API key.
// apiKey=nil preserves the existing setting; a pointer to "" stores an
// encrypted empty value. Explicit file and environment keys still win.
func SaveLLMConfig(ctx context.Context, database *db.DB, cfg LLMConfig, box SecretBox, apiKey *string) error {
	values := map[string]any{
		KeyLLMEndpointURL:   cfg.EndpointURL,
		KeyLLMModel:         cfg.Model,
		KeyLLMEgressAck:     cfg.EgressAck,
		KeyLLMDisabled:      cfg.Disabled,
		KeyLLMConfidence:    cfg.ConfidenceThreshold,
		KeyLLMDateAutoApply: cfg.DateAutoApply,
	}
	if apiKey != nil {
		secret, err := sealLLMAPIKey(box, *apiKey)
		if err != nil {
			return err
		}
		values[KeyLLMAPIKeySealed] = secret
	}
	return SetMany(ctx, database, values)
}

func sealLLMAPIKey(box SecretBox, apiKey string) (sealedSecret, error) {
	if box == nil {
		return sealedSecret{}, errors.New("settings: secret storage unavailable")
	}
	sealed, err := box.Seal([]byte(apiKey))
	if err != nil {
		return sealedSecret{}, fmt.Errorf("seal LLM API key: %w", err)
	}
	return sealedSecret{
		Version:    1,
		Ciphertext: sealed,
	}, nil
}

// ResolveLLMConfig starts with boot configuration and fills unconfigured
// fields from database settings. LoadFile promotes file values into the
// environment before this runs, so explicit file and environment values both
// remain authoritative.
func ResolveLLMConfig(ctx context.Context, database *db.DB, fb LLMConfig, box SecretBox) (LLMConfig, error) {
	out := fb
	out.DateAutoApply = true
	var s string
	if !envSet("LLM_ENDPOINT_URL") {
		if err := Get(ctx, database, KeyLLMEndpointURL, &s); err == nil && s != "" {
			out.EndpointURL = s
		}
	}
	s = ""
	if !envSet("LLM_MODEL") {
		if err := Get(ctx, database, KeyLLMModel, &s); err == nil && s != "" {
			out.Model = s
		}
	}
	if !envSet("LLM_API_KEY", "LLM_API_KEY_FILE") {
		var secret sealedSecret
		if err := Get(ctx, database, KeyLLMAPIKeySealed, &secret); err == nil {
			if secret.Version != 1 || len(secret.Ciphertext) == 0 {
				return LLMConfig{}, errors.New("settings: invalid sealed LLM API key")
			}
			if box == nil {
				return LLMConfig{}, errors.New("settings: cannot open LLM API key without secret storage")
			}
			plaintext, err := box.Open(secret.Ciphertext)
			if err != nil {
				return LLMConfig{}, fmt.Errorf("open LLM API key: %w", err)
			}
			out.APIKey = string(plaintext)
		} else if !errors.Is(err, ErrNotFound) {
			return LLMConfig{}, err
		}
	}
	if !envSet("LLM_EGRESS_ACK") {
		var b bool
		if err := Get(ctx, database, KeyLLMEgressAck, &b); err == nil {
			out.EgressAck = b
		}
	}
	if envSet("LLM_ENDPOINT_URL") {
		out.Disabled = out.EndpointURL == ""
	} else {
		var disabled bool
		if err := Get(ctx, database, KeyLLMDisabled, &disabled); err == nil {
			out.Disabled = disabled
		}
	}
	if !envSet("LLM_CONFIDENCE_THRESHOLD") {
		var confidence float64
		if err := Get(ctx, database, KeyLLMConfidence, &confidence); err == nil && confidence > 0 {
			out.ConfidenceThreshold = confidence
		}
	}
	var dateAutoApply bool
	if err := Get(ctx, database, KeyLLMDateAutoApply, &dateAutoApply); err == nil {
		out.DateAutoApply = dateAutoApply
	}
	if out.ConfidenceThreshold == 0 {
		out.ConfidenceThreshold = 0.7
	}
	return out, nil
}

func envSet(keys ...string) bool {
	for _, key := range keys {
		if _, ok := os.LookupEnv(key); ok {
			return true
		}
	}
	return false
}

// FSWatchConfig mirrors the fs-watch runtime knobs the setup wizard
// can override.
type FSWatchConfig struct {
	Dir        string
	OwnerEmail string
}

// ResolveFSWatchConfig fills fields not pinned by boot configuration from the
// database. The filesystem-watch supervisor calls it at boot and after saves.
func ResolveFSWatchConfig(ctx context.Context, database *db.DB, fb FSWatchConfig) FSWatchConfig {
	out := fb
	var s string
	if !envSet("INGEST_FS_DIR") {
		if err := Get(ctx, database, KeyFSWatchDir, &s); err == nil && s != "" {
			out.Dir = s
		}
	}
	s = ""
	if !envSet("INGEST_FS_OWNER_EMAIL") {
		if err := Get(ctx, database, KeyFSWatchOwnerEmail, &s); err == nil && s != "" {
			out.OwnerEmail = s
		}
	}
	return out
}

// RuntimePreferences are setup-owned values that affect long-running
// components. Durations stay typed here so callers cannot disagree about the
// stored hours-to-duration conversion.
type RuntimePreferences struct {
	BackupInterval time.Duration
	OCRLanguages   []string
}

// ResolveRuntimePreferences fills values not pinned by boot configuration from
// setup. Zero stored backup hours disables backups; OCR always retains at least
// one language.
func ResolveRuntimePreferences(ctx context.Context, database *db.DB, fb RuntimePreferences) RuntimePreferences {
	out := RuntimePreferences{
		BackupInterval: fb.BackupInterval,
		OCRLanguages:   append([]string(nil), fb.OCRLanguages...),
	}
	if !envSet("BACKUP_INTERVAL") {
		var hours int
		if err := Get(ctx, database, KeyBackupIntervalHours, &hours); err == nil && hours >= 0 && hours <= 720 {
			out.BackupInterval = time.Duration(hours) * time.Hour
		}
	}
	if !envSet("OCR_LANGUAGES") {
		var languages []string
		if err := Get(ctx, database, KeyOCRLanguages, &languages); err == nil && len(languages) > 0 {
			out.OCRLanguages = append([]string(nil), languages...)
		}
	}
	if len(out.OCRLanguages) == 0 {
		out.OCRLanguages = []string{"eng"}
	}
	return out
}
