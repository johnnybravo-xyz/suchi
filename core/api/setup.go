package api

// Admin-only setup wizard endpoints.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/johnnybravo-xyz/suchi/core/authz"
	"github.com/johnnybravo-xyz/suchi/core/ingest/emailwatch/oauth"
	"github.com/johnnybravo-xyz/suchi/core/jd"
	"github.com/johnnybravo-xyz/suchi/core/netutil"
	"github.com/johnnybravo-xyz/suchi/core/refile"
	"github.com/johnnybravo-xyz/suchi/core/settings"
)

var setupIntentPresets = map[string]string{
	"personal":       "solo",
	"household":      "household",
	"freelance":      "freelance",
	"small_business": "smb_billing",
	"custom":         "",
}

// registerSetup wires the wizard's routes. Called from Register().
func (s *Server) registerSetup(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/admin/setup/state", s.SetupState)
	mux.HandleFunc("POST /api/admin/setup/intent", s.SaveSetupIntent)
	mux.HandleFunc("POST /api/admin/setup/complete", s.SetupComplete)
	mux.HandleFunc("GET /api/admin/users", s.ListUsers)
	mux.HandleFunc("POST /api/admin/users", s.CreateUser)
	mux.HandleFunc("PATCH /api/admin/users/{id}", s.PatchUser)
	mux.HandleFunc("POST /api/admin/setup/preset", s.ApplyPreset)
	mux.HandleFunc("GET /api/admin/settings/llm", s.GetLLMSettings)
	mux.HandleFunc("POST /api/admin/settings/llm", s.SaveLLMSettings)
	mux.HandleFunc("POST /api/admin/settings/llm/test", s.TestLLMSettings)
	mux.HandleFunc("GET /api/admin/settings/microsoft-oauth", s.GetMicrosoftOAuthSettings)
	mux.HandleFunc("POST /api/admin/settings/microsoft-oauth", s.SaveMicrosoftOAuthSettings)
	mux.HandleFunc("GET /api/admin/settings/preferences", s.GetPreferences)
	mux.HandleFunc("POST /api/admin/settings/preferences", s.SavePreferences)
	mux.HandleFunc("GET /api/admin/settings/ingest", s.GetIngestSettings)
	mux.HandleFunc("POST /api/admin/settings/ingest", s.SaveIngestSettings)
}

// ---------- state ----------

// SetupState returns the wizard's progress.
func (s *Server) SetupState(w http.ResponseWriter, r *http.Request) {
	if s.requireAdmin(w, r) == nil {
		return
	}
	st, err := settings.LoadSetupState(r.Context(), s.DB)
	if err != nil {
		s.serverErr(w, "setup.state", err)
		return
	}
	st.RecommendedPreset = setupIntentPresets[st.Intent]
	s.writeJSON(w, http.StatusOK, st)
}

// SaveSetupIntent records the operator's onboarding goal. It only guides the
// filing-tree recommendation; it never hides features or sends telemetry.
func (s *Server) SaveSetupIntent(w http.ResponseWriter, r *http.Request) {
	if s.requireAdmin(w, r) == nil {
		return
	}
	var body struct {
		Intent string `json:"intent"`
	}
	if err := decodeJSON(r, &body); err != nil {
		s.writeError(w, http.StatusBadRequest, "bad_json", err.Error())
		return
	}
	body.Intent = strings.TrimSpace(body.Intent)
	recommended, ok := setupIntentPresets[body.Intent]
	if !ok {
		s.writeError(w, http.StatusBadRequest, "bad_intent",
			"intent must be one of: personal, household, freelance, small_business, custom")
		return
	}
	if err := settings.Set(r.Context(), s.DB, settings.KeySetupIntent, body.Intent); err != nil {
		s.serverErr(w, "setup.intent", err)
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{
		"intent": body.Intent, "recommended_preset": recommended,
	})
}

// SetupComplete stamps the wizard-finished timestamp.
func (s *Server) SetupComplete(w http.ResponseWriter, r *http.Request) {
	if s.requireAdmin(w, r) == nil {
		return
	}
	if err := settings.MarkSetupComplete(r.Context(), s.DB); err != nil {
		s.serverErr(w, "setup.complete", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---------- user creation ----------

// emailPattern is intentionally permissive: contains an @, no
// whitespace, no shell metachars. Full RFC 5322 is overkill for a
// self-hosted DMS and rejects legitimate addresses.
var emailPattern = regexp.MustCompile(`^[^\s@<>"'\\;]+@[^\s@<>"'\\;]+\.[^\s@<>"'\\;]+$`)

// CreateUser adds a new user. Admin-only. Password hashed via
// local-auth's argon2id helper (imported lazily via a hook set from
// main.go — see Server.PasswordHasher).
func (s *Server) CreateUser(w http.ResponseWriter, r *http.Request) {
	if s.requireAdmin(w, r) == nil {
		return
	}
	if s.PasswordHasher == nil {
		s.writeError(w, http.StatusServiceUnavailable, "no_hasher",
			"password hasher not wired — this is a boot-time misconfiguration")
		return
	}
	var body struct {
		Email        string   `json:"email"`
		Password     string   `json:"password"`
		DisplayName  string   `json:"display_name"`
		Role         string   `json:"role"`
		Capabilities []string `json:"capabilities"`
	}
	if err := decodeJSON(r, &body); err != nil {
		s.writeError(w, http.StatusBadRequest, "bad_json", err.Error())
		return
	}
	body.Email = strings.TrimSpace(strings.ToLower(body.Email))
	if !emailPattern.MatchString(body.Email) {
		s.writeError(w, http.StatusBadRequest, "bad_email", "email format invalid")
		return
	}
	if len(body.Password) < 8 {
		s.writeError(w, http.StatusBadRequest, "weak_password", "password must be at least 8 characters")
		return
	}
	if body.Role != "admin" && body.Role != "member" {
		body.Role = "member"
	}
	displayName := strings.TrimSpace(body.DisplayName)
	if displayName == "" {
		displayName = body.Email
	}
	// Capabilities column is only meaningful for members; admins are
	// implicitly capable via the role short-circuit. Validate the wire
	// slugs regardless so a typo never sneaks in.
	caps, err := authz.ParseWire(body.Capabilities)
	if err != nil {
		s.writeError(w, http.StatusBadRequest, "bad_capability", err.Error())
		return
	}
	capsJSON, err := json.Marshal(caps.SliceStrings())
	if err != nil {
		s.serverErr(w, "createuser.marshal_caps", err)
		return
	}
	hash, err := s.PasswordHasher(body.Password)
	if err != nil {
		s.serverErr(w, "createuser.hash", err)
		return
	}
	var newID int64
	err = s.DB.WriteTx(r.Context(), func(tx *sql.Tx) error {
		now := time.Now().Unix()
		res, err := tx.ExecContext(r.Context(), `
			INSERT INTO users(email, display_name, role, password_hash, capabilities, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?)
		`, body.Email, displayName, body.Role, hash, string(capsJSON), now, now)
		if err != nil {
			return err
		}
		newID, err = res.LastInsertId()
		return err
	})
	if err != nil {
		if isUniqueViolation(err) {
			s.writeError(w, http.StatusConflict, "email_taken", "an account with that email already exists")
			return
		}
		s.serverErr(w, "createuser.insert", err)
		return
	}
	s.writeJSON(w, http.StatusCreated, map[string]any{
		"id":           newID,
		"email":        body.Email,
		"role":         body.Role,
		"capabilities": caps.SliceStrings(),
	})
}

// ---------- Suchi Preset ----------

// ApplyPreset swaps the JD tree to one of the curated presets. Body:
// {"preset_id":"solo","confirm_blank":false}. Refuses blank without
// confirm_blank=true.
//
// A Suchi Preset is a preset following Suchi's Johnny.Decimal taxonomy —
// the starter tree plus its seeded automations.
func (s *Server) ApplyPreset(w http.ResponseWriter, r *http.Request) {
	if s.requireAdmin(w, r) == nil {
		return
	}
	// IncludeSeeds is a pointer so we can distinguish "field omitted"
	// (default true — the wizard's on-by-default toggle state) from
	// "explicitly false" (operator opted out of starter automations).
	var body struct {
		PresetID     string `json:"preset_id"`
		ConfirmBlank bool   `json:"confirm_blank"`
		// Refile=true accepts existing docs filed outside the inbox —
		// they get parked on the new inbox and a refile sweep is
		// triggered afterwards (re-run automations and enqueue re-render).
		// Selling point: "you can always come back to change this."
		Refile       bool  `json:"refile"`
		IncludeSeeds *bool `json:"include_seeds,omitempty"`
	}
	if err := decodeJSON(r, &body); err != nil {
		s.writeError(w, http.StatusBadRequest, "bad_json", err.Error())
		return
	}
	p, ok := jd.PresetByID(body.PresetID)
	if !ok {
		s.writeError(w, http.StatusBadRequest, "unknown_preset",
			`preset_id must be one of: solo, household, smb_billing, freelance, blank`)
		return
	}
	if p.Blank && !body.ConfirmBlank {
		s.writeError(w, http.StatusBadRequest, "blank_needs_confirm",
			"blank preset requires confirm_blank=true; it's harder to migrate away from")
		return
	}
	opts := jd.ApplyPresetOpts{
		AllowRefile: body.Refile,
		SkipSeeds:   body.IncludeSeeds != nil && !*body.IncludeSeeds,
	}
	if err := jd.ApplyPreset(r.Context(), s.DB, s.Log, body.PresetID, opts); err != nil {
		if errors.Is(err, jd.ErrDocumentsExist) {
			s.writeError(w, http.StatusConflict, "documents_filed",
				err.Error()+` (re-post with "refile": true to accept the refile)`)
			return
		}
		s.serverErr(w, "preset.apply", err)
		return
	}
	if body.Refile {
		// Kick off the sweep synchronously so operators see the counters
		// in the response. Long-running installs can hit the dedicated
		// /api/admin/refile endpoint instead for the background flavor.
		stats, err := refile.All(r.Context(), s.DB, s.Log, refile.Options{})
		if err != nil {
			s.Log.Warn("preset.refile.err", "err", err.Error())
		}
		if s.Jobs != nil {
			s.Jobs.Nudge()
		}
		_ = stats // captured in the log; response stays minimal for now
	}
	if err := settings.Set(r.Context(), s.DB, settings.KeyPreset, body.PresetID); err != nil {
		// Non-fatal: the tree is applied; the preset-name record is a
		// nicety for the wizard's recap page.
		s.Log.Warn("preset.record", "err", err.Error())
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"preset_id": body.PresetID})
}

// ---------- LLM settings ----------

var modelSafe = regexp.MustCompile(`^[A-Za-z0-9._/:\-]{1,128}$`)

type llmSettingsInput struct {
	Enabled             *bool    `json:"enabled,omitempty"`
	EndpointURL         string   `json:"endpoint_url"`
	Model               string   `json:"model"`
	APIKey              string   `json:"api_key"`
	ClearAPIKey         bool     `json:"clear_api_key"`
	EgressAck           bool     `json:"egress_ack"`
	ConfidenceThreshold *float64 `json:"confidence_threshold,omitempty"`
}

func (in *llmSettingsInput) normalize() {
	in.EndpointURL = strings.TrimSpace(in.EndpointURL)
	in.Model = strings.TrimSpace(in.Model)
}

func (in llmSettingsInput) wantsEnabled() bool {
	if in.Enabled != nil {
		return *in.Enabled
	}
	return in.EndpointURL != ""
}

func (s *Server) validateLLMSettings(w http.ResponseWriter, in llmSettingsInput, required bool) bool {
	if required && in.EndpointURL == "" {
		s.writeError(w, http.StatusBadRequest, "endpoint_required", "endpoint_url is required")
		return false
	}
	if in.EndpointURL != "" {
		if len(in.EndpointURL) > 2048 {
			s.writeError(w, http.StatusBadRequest, "bad_url", "endpoint_url is too long")
			return false
		}
		u, err := url.Parse(in.EndpointURL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil {
			s.writeError(w, http.StatusBadRequest, "bad_url", "endpoint_url must be an http(s):// URL without credentials")
			return false
		}
		if u.RawQuery != "" || u.Fragment != "" {
			s.writeError(w, http.StatusBadRequest, "bad_url", "endpoint_url must be a base URL without a query string or fragment")
			return false
		}
		if !netutil.IsLocalHost(u.Host) && !in.EgressAck {
			s.writeError(w, http.StatusBadRequest, "egress_ack_required",
				"non-local endpoint - set egress_ack=true to confirm document text will leave the box")
			return false
		}
	}
	if required && in.Model == "" {
		s.writeError(w, http.StatusBadRequest, "model_required", "model is required")
		return false
	}
	if in.Model != "" && !modelSafe.MatchString(in.Model) {
		s.writeError(w, http.StatusBadRequest, "bad_model", "model has forbidden characters")
		return false
	}
	if len(in.APIKey) > 8192 {
		s.writeError(w, http.StatusBadRequest, "bad_api_key", "api_key is too long")
		return false
	}
	if in.APIKey != "" && in.ClearAPIKey {
		s.writeError(w, http.StatusBadRequest, "api_key_conflict", "api_key and clear_api_key cannot be set together")
		return false
	}
	if in.ConfidenceThreshold != nil && (*in.ConfidenceThreshold < 0.5 || *in.ConfidenceThreshold > 0.95) {
		s.writeError(w, http.StatusBadRequest, "bad_confidence_threshold",
			"confidence_threshold must be between 0.50 and 0.95")
		return false
	}
	return true
}

func (s *Server) loadLLMSettingsStatus(ctx context.Context) (LLMSettingsStatus, error) {
	if s.LLMStatusReader != nil {
		return s.LLMStatusReader(ctx)
	}
	cfg, err := settings.ResolveLLMConfig(ctx, s.DB, settings.LLMConfig{}, s.LLMAEAD)
	if err != nil {
		return LLMSettingsStatus{}, err
	}
	enabled := !cfg.Disabled && cfg.EndpointURL != ""
	return LLMSettingsStatus{
		Enabled:             enabled,
		Active:              false,
		EndpointURL:         cfg.EndpointURL,
		Model:               cfg.Model,
		EgressAck:           cfg.EgressAck,
		HasAPIKey:           cfg.APIKey != "",
		ConfidenceThreshold: cfg.ConfidenceThreshold,
	}, nil
}

func (s *Server) GetLLMSettings(w http.ResponseWriter, r *http.Request) {
	if s.requireAdmin(w, r) == nil {
		return
	}
	status, err := s.loadLLMSettingsStatus(r.Context())
	if err != nil {
		s.serverErr(w, "settings.llm.status", err)
		return
	}
	s.writeJSON(w, http.StatusOK, status)
}

func (s *Server) SaveLLMSettings(w http.ResponseWriter, r *http.Request) {
	if s.requireAdmin(w, r) == nil {
		return
	}
	var body llmSettingsInput
	if err := decodeJSON(r, &body); err != nil {
		s.writeError(w, http.StatusBadRequest, "bad_json", err.Error())
		return
	}
	body.normalize()
	enabled := body.wantsEnabled()
	if !s.validateLLMSettings(w, body, enabled) {
		return
	}
	var apiKeyUpdate *string
	if body.APIKey != "" || body.ClearAPIKey {
		apiKeyUpdate = &body.APIKey
	}
	if apiKeyUpdate != nil && s.LLMAEAD == nil {
		s.writeError(w, http.StatusServiceUnavailable, "secret_storage_unavailable",
			"LLM API keys cannot be stored until secret storage is initialized")
		return
	}
	current, err := settings.ResolveLLMConfig(r.Context(), s.DB, settings.LLMConfig{}, s.LLMAEAD)
	if err != nil {
		s.serverErr(w, "settings.llm.resolve", err)
		return
	}
	confidence := current.ConfidenceThreshold
	if body.ConfidenceThreshold != nil {
		confidence = *body.ConfidenceThreshold
	}
	if err := settings.SaveLLMConfig(r.Context(), s.DB, settings.LLMConfig{
		EndpointURL:         body.EndpointURL,
		Model:               body.Model,
		EgressAck:           body.EgressAck,
		ConfidenceThreshold: confidence,
		Disabled:            !enabled,
	}, s.LLMAEAD, apiKeyUpdate); err != nil {
		s.serverErr(w, "settings.llm.save", err)
		return
	}
	if s.LLMReloader != nil {
		if err := s.LLMReloader(r.Context()); err != nil {
			s.Log.Warn("settings.llm.reload_failed", "err", err.Error())
			s.writeError(w, http.StatusInternalServerError, "apply_failed",
				"settings were saved but could not be applied; try again")
			return
		}
	}
	status, err := s.loadLLMSettingsStatus(r.Context())
	if err != nil {
		s.serverErr(w, "settings.llm.status_after_save", err)
		return
	}
	response := map[string]any{
		"saved":  true,
		"active": status.Active,
	}
	s.writeJSON(w, http.StatusOK, response)
}

func (s *Server) TestLLMSettings(w http.ResponseWriter, r *http.Request) {
	if s.requireAdmin(w, r) == nil {
		return
	}
	var body llmSettingsInput
	if err := decodeJSON(r, &body); err != nil {
		s.writeError(w, http.StatusBadRequest, "bad_json", err.Error())
		return
	}
	body.normalize()
	if !s.validateLLMSettings(w, body, true) {
		return
	}
	if s.LLMTester == nil {
		s.writeError(w, http.StatusNotImplemented, "llm_test_unavailable", "classifier connection testing is unavailable")
		return
	}
	confidence := 0.7
	if body.ConfidenceThreshold != nil {
		confidence = *body.ConfidenceThreshold
	}
	result, err := s.LLMTester(r.Context(), LLMTestConfig{
		EndpointURL:         body.EndpointURL,
		Model:               body.Model,
		APIKey:              body.APIKey,
		ClearAPIKey:         body.ClearAPIKey,
		EgressAck:           body.EgressAck,
		ConfidenceThreshold: confidence,
	})
	if err != nil {
		s.writeError(w, http.StatusBadGateway, "llm_test_failed", err.Error())
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "message": "Classifier responded with a valid result.", "result": result,
	})
}

// ---------- Microsoft OAuth registration ----------

var microsoftClientIDPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

type microsoftOAuthSettingsStatus struct {
	Ready             bool   `json:"ready"`
	EffectiveClientID string `json:"effective_client_id,omitempty"`
	OverrideClientID  string `json:"override_client_id,omitempty"`
	Source            string `json:"source"`
}

func (s *Server) microsoftOAuthStatus(ctx context.Context) (microsoftOAuthSettingsStatus, error) {
	var override string
	if err := settings.Get(ctx, s.DB, settings.KeyMicrosoftOAuthID, &override); err != nil && !errors.Is(err, settings.ErrNotFound) {
		return microsoftOAuthSettingsStatus{}, err
	}
	effective := strings.TrimSpace(override)
	source := "settings"
	if effective == "" {
		effective = strings.TrimSpace(s.MicrosoftOAuthFallbackID)
		source = s.MicrosoftOAuthFallbackSource
		if source == "" {
			source = "built_in"
		}
	}
	return microsoftOAuthSettingsStatus{
		Ready:             oauth.UsableClientID(effective) && s.EmailwatchMSAL != nil && s.EmailwatchMSAL.Ready(),
		EffectiveClientID: effective, OverrideClientID: override, Source: source,
	}, nil
}

func (s *Server) GetMicrosoftOAuthSettings(w http.ResponseWriter, r *http.Request) {
	if s.requireAdmin(w, r) == nil {
		return
	}
	s.microsoftOAuthSettingsMu.Lock()
	defer s.microsoftOAuthSettingsMu.Unlock()
	status, err := s.microsoftOAuthStatus(r.Context())
	if err != nil {
		s.serverErr(w, "settings.microsoft_oauth.status", err)
		return
	}
	s.writeJSON(w, http.StatusOK, status)
}

func (s *Server) SaveMicrosoftOAuthSettings(w http.ResponseWriter, r *http.Request) {
	if s.requireAdmin(w, r) == nil {
		return
	}
	s.microsoftOAuthSettingsMu.Lock()
	defer s.microsoftOAuthSettingsMu.Unlock()
	if s.EmailwatchMSAL == nil {
		s.writeError(w, http.StatusServiceUnavailable, "oauth_manager_unavailable",
			"Microsoft OAuth runtime is unavailable")
		return
	}
	var body struct {
		OverrideClientID string `json:"override_client_id"`
	}
	if err := decodeJSON(r, &body); err != nil {
		s.writeError(w, http.StatusBadRequest, "bad_json", err.Error())
		return
	}
	body.OverrideClientID = strings.TrimSpace(body.OverrideClientID)
	if body.OverrideClientID != "" {
		if !microsoftClientIDPattern.MatchString(body.OverrideClientID) || !oauth.UsableClientID(body.OverrideClientID) {
			s.writeError(w, http.StatusBadRequest, "bad_client_id",
				"override_client_id must be a non-zero Microsoft application id GUID")
			return
		}
		if err := s.EmailwatchMSAL.Validate(body.OverrideClientID); err != nil {
			s.writeError(w, http.StatusBadRequest, "bad_client_id", err.Error())
			return
		}
	}
	effective := body.OverrideClientID
	if effective == "" {
		effective = s.MicrosoftOAuthFallbackID
	}
	if body.OverrideClientID == "" {
		if err := settings.Delete(r.Context(), s.DB, settings.KeyMicrosoftOAuthID); err != nil {
			s.serverErr(w, "settings.microsoft_oauth.clear", err)
			return
		}
	} else if err := settings.Set(r.Context(), s.DB, settings.KeyMicrosoftOAuthID, body.OverrideClientID); err != nil {
		s.serverErr(w, "settings.microsoft_oauth.save", err)
		return
	}
	if err := s.EmailwatchMSAL.SetActive(effective); err != nil {
		s.serverErr(w, "settings.microsoft_oauth.apply", err)
		return
	}
	status, err := s.microsoftOAuthStatus(r.Context())
	if err != nil {
		s.serverErr(w, "settings.microsoft_oauth.status_after_save", err)
		return
	}
	s.writeJSON(w, http.StatusOK, status)
}

// ---------- preferences ----------

var langCode = regexp.MustCompile(`^[a-z]{2,3}(_[A-Z]{2})?$`)

// SavePreferences persists backup interval + OCR languages.
func (s *Server) GetPreferences(w http.ResponseWriter, r *http.Request) {
	if s.requireAdmin(w, r) == nil {
		return
	}
	if s.RuntimePreferencesReader != nil {
		status, err := s.RuntimePreferencesReader(r.Context())
		if err != nil {
			s.serverErr(w, "settings.preferences.status", err)
			return
		}
		s.writeJSON(w, http.StatusOK, status)
		return
	}
	prefs := settings.ResolveRuntimePreferences(r.Context(), s.DB, settings.RuntimePreferences{
		BackupInterval: 24 * time.Hour, OCRLanguages: []string{"eng"},
	})
	s.writeJSON(w, http.StatusOK, RuntimePreferencesStatus{
		BackupIntervalHours: int(prefs.BackupInterval / time.Hour),
		OCRLanguages:        prefs.OCRLanguages,
	})
}

func (s *Server) SavePreferences(w http.ResponseWriter, r *http.Request) {
	if s.requireAdmin(w, r) == nil {
		return
	}
	var body struct {
		BackupIntervalHours int      `json:"backup_interval_hours"`
		OCRLanguages        []string `json:"ocr_languages"`
	}
	if err := decodeJSON(r, &body); err != nil {
		s.writeError(w, http.StatusBadRequest, "bad_json", err.Error())
		return
	}
	if body.BackupIntervalHours < 0 || body.BackupIntervalHours > 720 {
		s.writeError(w, http.StatusBadRequest, "bad_interval",
			"backup_interval_hours must be 0-720 (0 disables)")
		return
	}
	cleaned := make([]string, 0, len(body.OCRLanguages))
	for _, l := range body.OCRLanguages {
		l = strings.TrimSpace(l)
		if l == "" {
			continue
		}
		if !langCode.MatchString(l) {
			s.writeError(w, http.StatusBadRequest, "bad_lang",
				"ocr_languages entries must be ISO codes like eng, hin, en_US")
			return
		}
		cleaned = append(cleaned, l)
	}
	if err := settings.SetMany(r.Context(), s.DB, map[string]any{
		settings.KeyBackupIntervalHours: body.BackupIntervalHours,
		settings.KeyOCRLanguages:        cleaned,
	}); err != nil {
		s.serverErr(w, "settings.preferences", err)
		return
	}
	if s.RuntimePreferencesReloader != nil {
		if err := s.RuntimePreferencesReloader(r.Context()); err != nil {
			s.writeError(w, http.StatusInternalServerError, "apply_failed",
				"preferences were saved but could not be applied; try again")
			return
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---------- ingest sources ----------

// fsPath restricts to absolute POSIX paths — no shell metachars, no
// backslashes. The fs-watch consumer still resolves + validates the
// path itself; this is belt-and-braces.
var fsPath = regexp.MustCompile(`^/[A-Za-z0-9 ._\-/]{0,255}$`)

func (s *Server) GetIngestSettings(w http.ResponseWriter, r *http.Request) {
	if s.requireAdmin(w, r) == nil {
		return
	}
	if s.FSWatchSettingsReader != nil {
		status, err := s.FSWatchSettingsReader(r.Context())
		if err != nil {
			s.serverErr(w, "settings.fswatch.status", err)
			return
		}
		s.writeJSON(w, http.StatusOK, status)
		return
	}
	cfg := settings.ResolveFSWatchConfig(r.Context(), s.DB, settings.FSWatchConfig{})
	s.writeJSON(w, http.StatusOK, FSWatchSettingsStatus{Dir: cfg.Dir, OwnerEmail: cfg.OwnerEmail})
}

// SaveIngestSettings persists fs-watch dir + owner and replaces the running
// watcher after the new configuration validates.
func (s *Server) SaveIngestSettings(w http.ResponseWriter, r *http.Request) {
	if s.requireAdmin(w, r) == nil {
		return
	}
	var body struct {
		FSWatchDir        string `json:"fs_watch_dir"`
		FSWatchOwnerEmail string `json:"fs_watch_owner_email"`
	}
	if err := decodeJSON(r, &body); err != nil {
		s.writeError(w, http.StatusBadRequest, "bad_json", err.Error())
		return
	}
	body.FSWatchDir = strings.TrimSpace(body.FSWatchDir)
	body.FSWatchOwnerEmail = strings.TrimSpace(strings.ToLower(body.FSWatchOwnerEmail))
	if body.FSWatchDir != "" && !fsPath.MatchString(body.FSWatchDir) {
		s.writeError(w, http.StatusBadRequest, "bad_dir",
			"fs_watch_dir must be an absolute path with safe characters")
		return
	}
	if body.FSWatchOwnerEmail != "" && !emailPattern.MatchString(body.FSWatchOwnerEmail) {
		s.writeError(w, http.StatusBadRequest, "bad_email", "fs_watch_owner_email invalid")
		return
	}
	if (body.FSWatchDir == "") != (body.FSWatchOwnerEmail == "") {
		s.writeError(w, http.StatusBadRequest, "incomplete_source",
			"fs_watch_dir and fs_watch_owner_email are both required")
		return
	}
	if body.FSWatchOwnerEmail != "" {
		var ownerID int64
		err := s.DB.Read.QueryRowContext(r.Context(),
			`SELECT id FROM users WHERE email = ? AND disabled = 0`, body.FSWatchOwnerEmail).Scan(&ownerID)
		if errors.Is(err, sql.ErrNoRows) {
			s.writeError(w, http.StatusBadRequest, "owner_not_found", "fs-watch owner is not an active user")
			return
		}
		if err != nil {
			s.serverErr(w, "settings.fswatch.owner", err)
			return
		}
	}
	if err := settings.SetMany(r.Context(), s.DB, map[string]any{
		settings.KeyFSWatchDir:        body.FSWatchDir,
		settings.KeyFSWatchOwnerEmail: body.FSWatchOwnerEmail,
	}); err != nil {
		s.serverErr(w, "settings.fswatch", err)
		return
	}
	if s.FSWatchReloader != nil {
		if err := s.FSWatchReloader(r.Context()); err != nil {
			s.writeError(w, http.StatusInternalServerError, "apply_failed",
				"ingest source was saved but could not be applied; try again")
			return
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---------- helpers ----------

// decodeJSON parses the request body, capping at 1 MiB so a huge body
// can't OOM the server. Callers already validated they'll get a body.
func decodeJSON(r *http.Request, into any) error {
	r.Body = http.MaxBytesReader(nil, r.Body, 1<<20)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	return dec.Decode(into)
}

func (s *Server) serverErr(w http.ResponseWriter, tag string, err error) {
	s.Log.Error("api."+tag, "err", err.Error())
	s.writeError(w, http.StatusInternalServerError, "internal", "server error")
}

// isUniqueViolation checks the modernc.org/sqlite error surface for
// SQLite's SQLITE_CONSTRAINT_UNIQUE (2067). String-match rather than
// import-and-typecast to keep the dependency graph unchanged.
func isUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "UNIQUE constraint failed") ||
		strings.Contains(msg, "(2067)")
}
