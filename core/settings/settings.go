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
	"sort"
	"time"

	"github.com/johnnybravo-xyz/suchi/core/db"
)

// Well-known keys. Prefer these constants over raw strings so a rename
// is one grep away.
const (
	KeySetupCompletedAt = "setup.completed_at"
	KeySetupIntent      = "setup.intent"

	KeyLLMEndpointURL  = "llm.endpoint_url"
	KeyLLMModel        = "llm.model"
	KeyLLMAPIKeySealed = "llm.api_key_sealed"
	KeyLLMEgressAck    = "llm.egress_ack"
	KeyLLMDisabled     = "llm.disabled"
	KeyLLMConfidence   = "llm.confidence_threshold"
	KeyArchiveEnabled  = "classification.archive_enabled"
	KeyArchiveAuto     = "classification.archive_auto_threshold"
	KeyArchiveReview   = "classification.archive_review_threshold"

	KeyPreset = "preset"

	KeyBackupIntervalHours = "backup.interval_hours"
	KeyOCRLanguages        = "ocr.languages" // JSON array of ISO codes

	KeyFSWatchDir        = "ingest.fs_watch_dir"
	KeyFSWatchOwnerEmail = "ingest.fs_watch_owner"
	KeyMicrosoftOAuthID  = "mail.microsoft_oauth_client_id"

	// Mail intake — legacy seed keys for the emailwatch IMAP poller.
	// Consumed once by emailaccounts.MigrateFromLegacySettings to
	// materialize a pre-multiaccount install as a single email_accounts
	// row; the keys are then deleted. Live mutation goes through
	// /api/admin/email-accounts.
	KeyIMAPHost            = "ingest.imap_host"
	KeyIMAPPort            = "ingest.imap_port"
	KeyIMAPUsername        = "ingest.imap_username"
	KeyIMAPPasswordSealed  = "ingest.imap_password_sealed"
	KeyIMAPFolder          = "ingest.imap_folder"
	KeyIMAPPollIntervalMin = "ingest.imap_poll_interval_min"
	KeyIMAPOwnerEmail      = "ingest.imap_owner_email"
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
	Intent            string `json:"intent,omitempty"`
	RecommendedPreset string `json:"recommended_preset,omitempty"`
	CurrentPreset     string `json:"current_preset,omitempty"`
}

// LoadSetupState reads the wizard's state. Missing keys → zero-value.
func LoadSetupState(ctx context.Context, database *db.DB) (*SetupState, error) {
	s := &SetupState{}
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
	return s, nil
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

// ---------- resolvers — settings-first, env-fallback ----------

// LLMConfig is the shape callers merge into their plugin config. Uses
// plain scalars so this package doesn't import plugins/*.
type LLMConfig struct {
	EndpointURL         string
	Model               string
	APIKey              string
	EgressAck           bool
	ConfidenceThreshold float64
	// Disabled is persisted separately from EndpointURL so an operator can
	// turn off an environment-backed classifier without erasing its setup.
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

func SetLLMAPIKey(ctx context.Context, database *db.DB, box SecretBox, apiKey string) error {
	secret, err := sealLLMAPIKey(box, apiKey)
	if err != nil {
		return err
	}
	return Set(ctx, database, KeyLLMAPIKeySealed, secret)
}

// SaveLLMConfig persists public fields and optionally replaces the API key.
// apiKey=nil preserves the existing setting; a pointer to "" stores an
// encrypted empty override so an environment fallback does not reappear.
func SaveLLMConfig(ctx context.Context, database *db.DB, cfg LLMConfig, box SecretBox, apiKey *string) error {
	values := map[string]any{
		KeyLLMEndpointURL: cfg.EndpointURL,
		KeyLLMModel:       cfg.Model,
		KeyLLMEgressAck:   cfg.EgressAck,
		KeyLLMDisabled:    cfg.Disabled,
		KeyLLMConfidence:  cfg.ConfidenceThreshold,
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

// ResolveLLMConfig merges settings over environment fallbacks and opens the
// stored API key. Plaintext keys written by development builds are migrated on
// read when a secret box is available.
func ResolveLLMConfig(ctx context.Context, database *db.DB, fb LLMConfig, box SecretBox) (LLMConfig, error) {
	out := fb
	var s string
	if err := Get(ctx, database, KeyLLMEndpointURL, &s); err == nil && s != "" {
		out.EndpointURL = s
	}
	s = ""
	if err := Get(ctx, database, KeyLLMModel, &s); err == nil && s != "" {
		out.Model = s
	}
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
		// Before v0.1, the value was stored as a JSON string despite the key
		// name. Accept it once and replace it with a sealed envelope.
		var legacy string
		if legacyErr := Get(ctx, database, KeyLLMAPIKeySealed, &legacy); legacyErr != nil {
			return LLMConfig{}, err
		}
		out.APIKey = legacy
		if legacy != "" && box != nil {
			if err := SetLLMAPIKey(ctx, database, box, legacy); err != nil {
				return LLMConfig{}, err
			}
		}
	}
	var b bool
	if err := Get(ctx, database, KeyLLMEgressAck, &b); err == nil {
		out.EgressAck = b
	}
	b = false
	if err := Get(ctx, database, KeyLLMDisabled, &b); err == nil {
		out.Disabled = b
	}
	var confidence float64
	if err := Get(ctx, database, KeyLLMConfidence, &confidence); err == nil && confidence > 0 {
		out.ConfidenceThreshold = confidence
	}
	if out.ConfidenceThreshold == 0 {
		out.ConfidenceThreshold = 0.7
	}
	return out, nil
}

// EmailWatchConfig mirrors the emailwatch runtime knobs the SPA
// Admin panel can override. Plain scalars so this package doesn't
// import emailwatch. The URL is assembled from Host/Port/Username/
// Folder by the caller in main.go.
type EmailWatchConfig struct {
	Host            string
	Port            int
	Username        string
	Password        string
	Folder          string
	PollIntervalMin int
	OwnerEmail      string
}

// ResolveEmailWatchConfig merges settings over env fallback for the
// mail-intake poller. Settings win when set; blank leaves the fb
// value alone. Kept for the seed-migration path in emailaccounts —
// mutation of live mail intake now flows through the email_accounts
// table via /api/admin/email-accounts.
func ResolveEmailWatchConfig(ctx context.Context, database *db.DB, fb EmailWatchConfig) EmailWatchConfig {
	out := fb
	var s string
	if err := Get(ctx, database, KeyIMAPHost, &s); err == nil && s != "" {
		out.Host = s
	}
	var n int
	if err := Get(ctx, database, KeyIMAPPort, &n); err == nil && n > 0 {
		out.Port = n
	}
	s = ""
	if err := Get(ctx, database, KeyIMAPUsername, &s); err == nil && s != "" {
		out.Username = s
	}
	s = ""
	if err := Get(ctx, database, KeyIMAPPasswordSealed, &s); err == nil && s != "" {
		out.Password = s
	}
	s = ""
	if err := Get(ctx, database, KeyIMAPFolder, &s); err == nil && s != "" {
		out.Folder = s
	}
	n = 0
	if err := Get(ctx, database, KeyIMAPPollIntervalMin, &n); err == nil && n > 0 {
		out.PollIntervalMin = n
	}
	s = ""
	if err := Get(ctx, database, KeyIMAPOwnerEmail, &s); err == nil && s != "" {
		out.OwnerEmail = s
	}
	return out
}

// FSWatchConfig mirrors the fs-watch runtime knobs the setup wizard
// can override.
type FSWatchConfig struct {
	Dir        string
	OwnerEmail string
}

// ResolveFSWatchConfig merges settings over the environment fallback. The
// filesystem-watch supervisor calls it at boot and after setup saves.
func ResolveFSWatchConfig(ctx context.Context, database *db.DB, fb FSWatchConfig) FSWatchConfig {
	out := fb
	var s string
	if err := Get(ctx, database, KeyFSWatchDir, &s); err == nil && s != "" {
		out.Dir = s
	}
	s = ""
	if err := Get(ctx, database, KeyFSWatchOwnerEmail, &s); err == nil && s != "" {
		out.OwnerEmail = s
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

// ResolveRuntimePreferences merges setup values over config-file/environment
// fallbacks. Zero backup hours is an explicit disable; an empty OCR list keeps
// the fallback because the OCR engines require at least one language.
func ResolveRuntimePreferences(ctx context.Context, database *db.DB, fb RuntimePreferences) RuntimePreferences {
	out := RuntimePreferences{
		BackupInterval: fb.BackupInterval,
		OCRLanguages:   append([]string(nil), fb.OCRLanguages...),
	}
	var hours int
	if err := Get(ctx, database, KeyBackupIntervalHours, &hours); err == nil && hours >= 0 && hours <= 720 {
		out.BackupInterval = time.Duration(hours) * time.Hour
	}
	var languages []string
	if err := Get(ctx, database, KeyOCRLanguages, &languages); err == nil && len(languages) > 0 {
		out.OCRLanguages = append([]string(nil), languages...)
	}
	if len(out.OCRLanguages) == 0 {
		out.OCRLanguages = []string{"eng"}
	}
	return out
}
