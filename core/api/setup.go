// SPDX-License-Identifier: AGPL-3.0-or-later

package api

// Admin-only archive configuration endpoints.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/johnnybravo-xyz/suchi/core/auth"
	"github.com/johnnybravo-xyz/suchi/core/authz"
	"github.com/johnnybravo-xyz/suchi/core/jd"
	"github.com/johnnybravo-xyz/suchi/core/jd/importer"
	"github.com/johnnybravo-xyz/suchi/core/jd/systems"
	"github.com/johnnybravo-xyz/suchi/core/netutil"
	"github.com/johnnybravo-xyz/suchi/core/refile"
	"github.com/johnnybravo-xyz/suchi/core/settings"
)

// registerArchiveConfiguration wires the archive administration routes.
func (s *Server) registerArchiveConfiguration(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/admin/setup/state", s.SetupState)
	mux.HandleFunc("GET /api/admin/users", s.ListUsers)
	mux.HandleFunc("POST /api/admin/users", s.CreateUser)
	mux.HandleFunc("PATCH /api/admin/users/{id}", s.PatchUser)
	mux.HandleFunc("POST /api/admin/setup/preset/preview", s.PreviewPresetChange)
	mux.HandleFunc("POST /api/admin/setup/preset/apply", s.ApplyPresetChange)
	mux.HandleFunc("GET /api/admin/settings/llm", s.GetLLMSettings)
	mux.HandleFunc("PATCH /api/admin/settings/llm", s.PatchLLMSettings)
	mux.HandleFunc("POST /api/admin/settings/llm", s.SaveLLMSettings)
	mux.HandleFunc("POST /api/admin/settings/llm/test", s.TestLLMSettings)
	mux.HandleFunc("POST /api/admin/settings/llm/subscriptions/{provider}/{action}", s.SubscriptionLoginAction)
	mux.HandleFunc("GET /api/admin/settings/llm/subscriptions/{provider}/models", s.SubscriptionModels)
	mux.HandleFunc("GET /api/admin/settings/preferences", s.GetPreferences)
	mux.HandleFunc("POST /api/admin/settings/preferences", s.SavePreferences)
	mux.HandleFunc("GET /api/admin/settings/ingest", s.GetIngestSettings)
	mux.HandleFunc("POST /api/admin/settings/ingest", s.SaveIngestSettings)
}

// ---------- state ----------

// SetupState returns filing-tree onboarding state for the selected system.
func (s *Server) SetupState(w http.ResponseWriter, r *http.Request) {
	actor := s.requireAdmin(w, r)
	if actor == nil {
		return
	}
	systemID, ok := s.requireSystem(w, r, actor)
	if !ok {
		return
	}
	st, err := settings.LoadSetupState(r.Context(), s.DB, systemID)
	if err != nil {
		s.serverErr(w, "setup.state", err)
		return
	}
	s.writeJSON(w, http.StatusOK, st)
}

// ---------- user creation ----------

// CreateUser adds a new user. Admin-only. Password hashed via
// local-auth's argon2id helper (imported lazily via a hook set from
// main.go — see Server.PasswordHasher).
func (s *Server) CreateUser(w http.ResponseWriter, r *http.Request) {
	p := s.requireAdmin(w, r)
	if p == nil {
		return
	}
	if s.emailChangeMode(p) == EmailChangeModeOIDC {
		s.writeError(w, http.StatusConflict, "oidc_managed",
			"accounts are provisioned by the configured identity provider")
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
	normalizedEmail, err := auth.NormalizeEmail(body.Email)
	if err != nil {
		s.writeError(w, http.StatusBadRequest, "bad_email", "email format invalid or reserved")
		return
	}
	body.Email = normalizedEmail
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
	if body.Role == "admin" {
		caps = nil
	}
	capsJSON, err := json.Marshal(caps.SliceStrings())
	if err != nil {
		s.serverErr(w, "createuser.marshal_caps", err)
		return
	}
	hash, err := s.PasswordHasher(body.Password)
	if err != nil {
		s.passwordHashUnavailable(w, "createuser.hash", err)
		return
	}
	var newID int64
	err = s.DB.WriteTx(r.Context(), func(tx *sql.Tx) error {
		if err := requireActiveAdminInTx(r.Context(), tx, p.UserID); err != nil {
			return err
		}
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

// ---------- Filing tree changes ----------

type presetChangeBody struct {
	PresetID          string       `json:"preset_id"`
	SetIDs            []string     `json:"set_ids"`
	Replacements      map[int]bool `json:"replacements"`
	Remaps            map[int]int  `json:"remaps"`
	ConfirmBlank      bool         `json:"confirm_blank"`
	Refile            bool         `json:"refile"`
	IncludeSeeds      *bool        `json:"include_seeds,omitempty"`
	ExpectedStateHash string       `json:"expected_state_hash"`
}

type refileResult struct {
	DocumentsScanned int64  `json:"documents_scanned"`
	RulesApplied     int64  `json:"rules_applied"`
	RendersQueued    int64  `json:"renders_queued"`
	Errors           int64  `json:"errors"`
	Elapsed          string `json:"elapsed"`
}

type presetChangeResponse struct {
	*jd.PresetChange
	Refile      *refileResult `json:"refile,omitempty"`
	RefileError string        `json:"refile_error,omitempty"`
}

func (s *Server) PreviewPresetChange(w http.ResponseWriter, r *http.Request) {
	principal := s.requireAdmin(w, r)
	if principal == nil {
		return
	}
	systemID, ok := s.requireSystem(w, r, principal)
	if !ok {
		return
	}
	var body presetChangeBody
	if err := decodeJSON(r, &body); err != nil {
		s.writeError(w, http.StatusBadRequest, "bad_json", err.Error())
		return
	}
	change, err := jd.PreviewPresetChange(r.Context(), s.DB, body.request(systemID, principal.UserID))
	if err != nil {
		s.writePresetChangeError(w, "preset.preview", err)
		return
	}
	s.writeJSON(w, http.StatusOK, presetChangeResponse{PresetChange: change})
}

func (s *Server) ApplyPresetChange(w http.ResponseWriter, r *http.Request) {
	principal := s.requireAdmin(w, r)
	if principal == nil {
		return
	}
	systemID, ok := s.requireSystem(w, r, principal)
	if !ok {
		return
	}
	var body presetChangeBody
	if err := decodeJSON(r, &body); err != nil {
		s.writeError(w, http.StatusBadRequest, "bad_json", err.Error())
		return
	}
	if body.ExpectedStateHash == "" {
		s.writeError(w, http.StatusBadRequest, "preview_required", "preview the filing-tree change before applying it")
		return
	}
	change, err := jd.ApplyPresetChange(r.Context(), s.DB, s.Log, body.request(systemID, principal.UserID))
	if err != nil {
		s.writePresetChangeError(w, "preset.apply", err)
		return
	}
	response := presetChangeResponse{PresetChange: change}
	if body.Refile {
		stats, refileErr := refile.All(r.Context(), s.DB, s.Actions, s.Log, refile.Options{SystemID: systemID, ActorID: principal.UserID})
		response.Refile = &refileResult{
			DocumentsScanned: stats.DocsScanned, RulesApplied: stats.AutomationsApplied,
			RendersQueued: stats.RenderEnqueued, Errors: stats.Errors, Elapsed: stats.Elapsed.String(),
		}
		if refileErr != nil {
			response.RefileError = refileErr.Error()
			s.Log.Warn("preset.refile.err", "err", refileErr.Error())
		}
	}
	if s.Jobs != nil {
		s.Jobs.Nudge()
	}
	s.writeJSON(w, http.StatusOK, response)
}

func (body presetChangeBody) request(systemID, actorID int64) jd.PresetChangeRequest {
	return jd.PresetChangeRequest{
		PresetID: body.PresetID, SetIDs: body.SetIDs, Replacements: body.Replacements, Remaps: body.Remaps,
		SkipSeeds: body.IncludeSeeds != nil && !*body.IncludeSeeds, ConfirmBlank: body.ConfirmBlank,
		ExpectedStateHash: body.ExpectedStateHash, SystemID: systemID, ActorID: actorID,
	}
}

func (s *Server) writePresetChangeError(w http.ResponseWriter, operation string, err error) {
	var collisions *importer.UnresolvedCollisionsError
	switch {
	case errors.As(err, &collisions):
		s.writeJSON(w, http.StatusConflict, map[string]any{
			"code": "collisions", "error": err.Error(), "collisions": collisions.Items,
		})
	case errors.Is(err, importer.ErrStalePreview):
		s.writeError(w, http.StatusConflict, "stale_preview", err.Error())
	case errors.Is(err, jd.ErrBlankPresetConfirmation):
		s.writeError(w, http.StatusBadRequest, "blank_needs_confirm", err.Error())
	default:
		s.writeError(w, http.StatusBadRequest, "invalid_filing_tree", err.Error())
	}
}

// ---------- LLM settings ----------

var modelSafe = regexp.MustCompile(`^[A-Za-z0-9._/:\-]{1,128}$`)

type llmSettingsInput struct {
	SubscriptionProvider string   `json:"subscription_provider"`
	Enabled              *bool    `json:"enabled,omitempty"`
	EndpointURL          string   `json:"endpoint_url"`
	Model                string   `json:"model"`
	APIKey               string   `json:"api_key"`
	ClearAPIKey          bool     `json:"clear_api_key"`
	EgressAck            bool     `json:"egress_ack"`
	ConfidenceThreshold  *float64 `json:"confidence_threshold,omitempty"`
	ArchiveEnabled       *bool    `json:"archive_enabled,omitempty"`
	ArchiveAuto          *float64 `json:"archive_auto_threshold,omitempty"`
	ArchiveReview        *float64 `json:"archive_review_threshold,omitempty"`
}

func (in *llmSettingsInput) normalize() {
	in.EndpointURL = strings.TrimSpace(in.EndpointURL)
	in.Model = strings.TrimSpace(in.Model)
}

func (in llmSettingsInput) wantsEnabled() bool {
	if in.Enabled != nil {
		return *in.Enabled
	}
	return in.EndpointURL != "" || in.SubscriptionProvider != ""
}

func (s *Server) validateLLMSettings(ctx context.Context, w http.ResponseWriter, in llmSettingsInput, required bool) bool {
	if in.SubscriptionProvider != "" {
		if s.SubscriptionProvider == nil {
			s.writeError(w, 503, "subscription_unavailable", "Account subscriptions are unavailable")
			return false
		}
		provider, ok := s.SubscriptionProvider(in.SubscriptionProvider)
		if !ok {
			s.writeError(w, 400, "bad_subscription_provider", "Unknown subscription provider")
			return false
		}
		if in.EndpointURL != "" || in.APIKey != "" || in.ClearAPIKey {
			s.writeError(w, 400, "subscription_conflict", "Subscriptions do not accept endpoint or API key settings")
			return false
		}
		if required && !in.EgressAck {
			s.writeError(w, 400, "egress_ack_required", "Account subscription requires document egress acknowledgement")
			return false
		}
		if required && !provider.Connected(ctx) {
			s.writeError(w, 400, "subscription_not_connected", "Connect the subscription before testing or enabling it")
			return false
		}
	}
	if required && in.EndpointURL == "" && in.SubscriptionProvider == "" {
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
	archive := settings.ResolveArchiveClassifierConfig(ctx, s.DB)
	if in.ArchiveAuto != nil {
		archive.AutoThreshold = *in.ArchiveAuto
	}
	if in.ArchiveReview != nil {
		archive.ReviewThreshold = *in.ArchiveReview
	}
	if archive.ReviewThreshold < 0.5 || archive.ReviewThreshold > 0.9 ||
		archive.AutoThreshold < 0.55 || archive.AutoThreshold > 0.95 ||
		archive.ReviewThreshold >= archive.AutoThreshold {
		s.writeError(w, http.StatusBadRequest, "bad_archive_thresholds",
			"archive review threshold must be 0.50-0.90 and below the 0.55-0.95 auto-apply threshold")
		return false
	}
	return true
}

func (s *Server) loadLLMSettingsStatus(ctx context.Context) (LLMSettingsStatus, error) {
	archive := settings.ResolveArchiveClassifierConfig(ctx, s.DB)
	researchContextMode := settings.ResolveResearchContextMode(ctx, s.DB)
	autoApply, err := settings.ResolveAutoApply(ctx, s.DB.Read)
	if err != nil {
		return LLMSettingsStatus{}, err
	}
	if s.LLMStatusReader != nil {
		status, err := s.LLMStatusReader(ctx)
		if status.Provider != "" {
			if err := settings.Get(ctx, s.DB, settings.SubscriptionModelKey(status.Provider), &status.SubscriptionStatus.Model); err != nil && !errors.Is(err, settings.ErrNotFound) {
				return LLMSettingsStatus{}, err
			}
		}
		status.AutoApply = autoApply
		status.ArchiveEnabled = archive.Enabled
		status.ArchiveAuto = archive.AutoThreshold
		status.ArchiveReview = archive.ReviewThreshold
		status.ResearchContextMode = string(researchContextMode)
		return status, err
	}
	cfg, err := settings.ResolveLLMConfig(ctx, s.DB, settings.LLMConfig{}, s.LLMAEAD)
	if err != nil {
		return LLMSettingsStatus{}, err
	}
	var subscriptionModel string
	if cfg.SubscriptionProvider != "" {
		if err := settings.Get(ctx, s.DB, settings.SubscriptionModelKey(cfg.SubscriptionProvider), &subscriptionModel); err != nil && !errors.Is(err, settings.ErrNotFound) {
			return LLMSettingsStatus{}, err
		}
	}
	enabled := !cfg.Disabled && (cfg.EndpointURL != "" || cfg.SubscriptionProvider != "")
	return LLMSettingsStatus{
		Enabled:             enabled,
		Active:              false,
		SubscriptionStatus:  settings.SubscriptionStatus{Provider: cfg.SubscriptionProvider, Model: subscriptionModel},
		Mode:                llmMode(cfg),
		EndpointURL:         cfg.EndpointURL,
		Model:               cfg.Model,
		EgressAck:           cfg.EgressAck,
		HasAPIKey:           cfg.APIKey != "",
		ConfidenceThreshold: cfg.ConfidenceThreshold,
		AutoApply:           autoApply,
		ArchiveEnabled:      archive.Enabled,
		ArchiveAuto:         archive.AutoThreshold,
		ArchiveReview:       archive.ReviewThreshold,
		ResearchContextMode: string(researchContextMode),
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

// PatchLLMSettings independently saves local matching, a policy, research preset,
// or subscription model preference without touching activation, credentials, or existing reviews.
func (s *Server) PatchLLMSettings(w http.ResponseWriter, r *http.Request) {
	if s.requireAdmin(w, r) == nil {
		return
	}
	var body struct {
		AutoApply            json.RawMessage `json:"auto_apply"`
		ResearchContextMode  json.RawMessage `json:"research_context_mode"`
		SubscriptionModel    json.RawMessage `json:"subscription_model"`
		SubscriptionProvider string          `json:"subscription_provider"`
		ArchiveEnabled       *bool           `json:"archive_enabled"`
		ArchiveAuto          *float64        `json:"archive_auto_threshold"`
		ArchiveReview        *float64        `json:"archive_review_threshold"`
	}
	if err := decodeJSON(r, &body); err != nil {
		s.writeError(w, http.StatusBadRequest, "bad_json", err.Error())
		return
	}
	fields := 0
	for _, value := range []json.RawMessage{body.AutoApply, body.ResearchContextMode, body.SubscriptionModel} {
		if value != nil {
			fields++
		}
	}
	archivePatch := body.ArchiveEnabled != nil || body.ArchiveAuto != nil || body.ArchiveReview != nil
	if archivePatch {
		fields++
	}
	if fields != 1 {
		s.writeError(w, http.StatusBadRequest, "bad_settings_patch",
			"provide one settings group: local matching, auto_apply, research_context_mode or subscription_model")
		return
	}
	if body.SubscriptionModel == nil && body.SubscriptionProvider != "" {
		s.writeError(w, 400, "bad_settings_patch", "subscription_provider requires subscription_model")
		return
	}
	if archivePatch {
		if !s.validateLLMSettings(r.Context(), w, llmSettingsInput{ArchiveAuto: body.ArchiveAuto, ArchiveReview: body.ArchiveReview}, false) {
			return
		}
		cfg := settings.ResolveArchiveClassifierConfig(r.Context(), s.DB)
		if body.ArchiveEnabled != nil {
			cfg.Enabled = *body.ArchiveEnabled
		}
		if body.ArchiveAuto != nil {
			cfg.AutoThreshold = *body.ArchiveAuto
		}
		if body.ArchiveReview != nil {
			cfg.ReviewThreshold = *body.ArchiveReview
		}
		if err := settings.SaveArchiveClassifierConfig(r.Context(), s.DB, cfg); err != nil {
			s.serverErr(w, "settings.archive_classifier.save", err)
			return
		}
		s.writeJSON(w, http.StatusOK, struct {
			Enabled bool    `json:"archive_enabled"`
			Auto    float64 `json:"archive_auto_threshold"`
			Review  float64 `json:"archive_review_threshold"`
		}{cfg.Enabled, cfg.AutoThreshold, cfg.ReviewThreshold})
		return
	}
	if body.SubscriptionModel != nil {
		if s.SubscriptionProvider == nil {
			s.writeError(w, 503, "subscription_unavailable", "Account subscriptions are unavailable")
			return
		}
		if _, ok := s.SubscriptionProvider(body.SubscriptionProvider); !ok {
			s.writeError(w, 400, "bad_subscription_provider", "Unknown subscription provider")
			return
		}
		var model string
		if err := json.Unmarshal(body.SubscriptionModel, &model); err != nil || !modelSafe.MatchString(model) {
			s.writeError(w, http.StatusBadRequest, "bad_subscription_model", "subscription_model must be a valid model identifier")
			return
		}
		if err := settings.Set(r.Context(), s.DB, settings.SubscriptionModelKey(body.SubscriptionProvider), model); err != nil {
			s.serverErr(w, "settings.llm.subscription_model", err)
			return
		}
		s.writeJSON(w, http.StatusOK, map[string]string{"subscription_provider": body.SubscriptionProvider, "subscription_model": model})
		return
	}
	if body.AutoApply != nil {
		value := strings.TrimSpace(string(body.AutoApply))
		if value != "true" && value != "false" {
			s.writeError(w, http.StatusBadRequest, "bad_auto_apply", "auto_apply must be a boolean")
			return
		}
		enabled := value == "true"
		if err := settings.Set(r.Context(), s.DB, settings.KeyClassificationAutoApply, enabled); err != nil {
			s.serverErr(w, "settings.classification.auto_apply", err)
			return
		}
		s.writeJSON(w, http.StatusOK, map[string]bool{"auto_apply": enabled})
		return
	}
	var mode settings.ResearchContextMode
	if err := json.Unmarshal(body.ResearchContextMode, &mode); err != nil || !mode.Valid() {
		s.writeError(w, http.StatusBadRequest, "bad_research_context_mode",
			"research_context_mode must be one of: focused, balanced, detailed")
		return
	}
	if err := settings.SaveResearchContextMode(r.Context(), s.DB, mode); err != nil {
		s.serverErr(w, "settings.llm.research_context", err)
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]string{"research_context_mode": string(mode)})
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
	if !s.validateLLMSettings(r.Context(), w, body, enabled) {
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
		SubscriptionProvider: body.SubscriptionProvider,
		EndpointURL:          body.EndpointURL,
		Model:                body.Model,
		EgressAck:            body.EgressAck,
		ConfidenceThreshold:  confidence,
		Disabled:             !enabled,
	}, s.LLMAEAD, apiKeyUpdate); err != nil {
		s.serverErr(w, "settings.llm.save", err)
		return
	}
	archive := settings.ResolveArchiveClassifierConfig(r.Context(), s.DB)
	if body.ArchiveEnabled != nil {
		archive.Enabled = *body.ArchiveEnabled
	}
	if body.ArchiveAuto != nil {
		archive.AutoThreshold = *body.ArchiveAuto
	}
	if body.ArchiveReview != nil {
		archive.ReviewThreshold = *body.ArchiveReview
	}
	if err := settings.SaveArchiveClassifierConfig(r.Context(), s.DB, archive); err != nil {
		s.serverErr(w, "settings.archive_classifier.save", err)
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
	if !s.validateLLMSettings(r.Context(), w, body, true) {
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
		SubscriptionProvider: body.SubscriptionProvider,
		EndpointURL:          body.EndpointURL,
		Model:                body.Model,
		APIKey:               body.APIKey,
		ClearAPIKey:          body.ClearAPIKey,
		EgressAck:            body.EgressAck,
		ConfidenceThreshold:  confidence,
	})
	if err != nil {
		if s.Log != nil {
			s.Log.Error("api.response_error", "status", http.StatusBadGateway,
				"code", "llm_test_failed", "err", err.Error())
		}
		s.writeJSON(w, http.StatusBadGateway, errBody{
			Code: "llm_test_failed", Error: llmTestErrorMessage(err),
		})
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "message": "Model connection and response format checked; accuracy was not tested.", "result": result,
	})
}

func llmTestErrorMessage(err error) string {
	var upstream interface{ UpstreamStatusCode() int }
	if errors.As(err, &upstream) {
		switch upstream.UpstreamStatusCode() {
		case http.StatusBadRequest:
			return "Provider rejected the request. Check the endpoint and model."
		case http.StatusUnauthorized:
			return "Provider rejected the API key."
		case http.StatusForbidden:
			return "Provider denied access. Check the API key and model permissions."
		case http.StatusNotFound:
			return "Provider could not find this model. Check the model name and endpoint."
		case http.StatusTooManyRequests:
			return "Provider rate limit reached. Try again shortly."
		}
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "Provider did not respond before the timeout."
	}
	return "Provider request failed. Check the endpoint, model, and provider availability."
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

var (
	errFSWatchSystemUnavailable = errors.New("filesystem watcher system is unavailable")
	errFSWatchOwnerUnavailable  = errors.New("filesystem watcher owner is unavailable")
	errFSWatchOwnerCannotEnter  = errors.New("filesystem watcher owner cannot enter system")
)

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
	ownerEmail := cfg.OwnerEmail
	if cfg.OwnerID > 0 {
		if err := s.DB.Read.QueryRowContext(r.Context(),
			`SELECT email FROM users WHERE id = ? AND disabled = 0`, cfg.OwnerID,
		).Scan(&ownerEmail); err != nil {
			s.serverErr(w, "settings.fswatch.owner", err)
			return
		}
	}
	sys, err := systems.Get(r.Context(), s.DB.Read, systems.DefaultID)
	if cfg.System != "" {
		sys, err = systems.ByCode(r.Context(), s.DB.Read, cfg.System)
	}
	if err != nil {
		s.serverErr(w, "settings.fswatch.system", err)
		return
	}
	s.writeJSON(w, http.StatusOK, FSWatchSettingsStatus{Dir: cfg.Dir, OwnerEmail: ownerEmail, System: sys.Code})
}

// SaveIngestSettings persists fs-watch dir + owner and replaces the running
// watcher after the new configuration validates.
func (s *Server) SaveIngestSettings(w http.ResponseWriter, r *http.Request) {
	actor := s.requireAdmin(w, r)
	if actor == nil {
		return
	}
	var body struct {
		FSWatchDir        string `json:"fs_watch_dir"`
		FSWatchOwnerEmail string `json:"fs_watch_owner_email"`
		FSWatchSystem     string `json:"fs_watch_system"`
	}
	if err := decodeJSON(r, &body); err != nil {
		s.writeError(w, http.StatusBadRequest, "bad_json", err.Error())
		return
	}
	body.FSWatchDir = strings.TrimSpace(body.FSWatchDir)
	body.FSWatchSystem = strings.TrimSpace(body.FSWatchSystem)
	body.FSWatchOwnerEmail = strings.TrimSpace(body.FSWatchOwnerEmail)
	if body.FSWatchOwnerEmail != "" {
		normalized, err := auth.NormalizeEmail(body.FSWatchOwnerEmail)
		if err != nil {
			s.writeError(w, http.StatusBadRequest, "bad_email", "fs_watch_owner_email invalid or reserved")
			return
		}
		body.FSWatchOwnerEmail = normalized
	}
	if body.FSWatchDir != "" && !fsPath.MatchString(body.FSWatchDir) {
		s.writeError(w, http.StatusBadRequest, "bad_dir",
			"fs_watch_dir must be an absolute path with safe characters")
		return
	}
	if (body.FSWatchDir == "") != (body.FSWatchOwnerEmail == "") {
		s.writeError(w, http.StatusBadRequest, "incomplete_source",
			"fs_watch_dir and fs_watch_owner_email are both required")
		return
	}

	keys := []string{
		settings.KeyFSWatchDir,
		settings.KeyFSWatchOwnerID,
		settings.KeyFSWatchSystem,
	}
	var (
		previous map[string]settings.RawValue
		written  map[string]settings.RawValue
	)
	err := s.DB.WriteTx(r.Context(), func(tx *sql.Tx) error {
		if err := requireActiveAdminInTx(r.Context(), tx, actor.UserID); err != nil {
			return err
		}
		sys, err := systems.Get(r.Context(), tx, systems.DefaultID)
		if body.FSWatchSystem != "" {
			sys, err = systems.ByCode(r.Context(), tx, body.FSWatchSystem)
		}
		if errors.Is(err, sql.ErrNoRows) {
			return errFSWatchSystemUnavailable
		}
		if err != nil {
			return err
		}

		var ownerID int64
		if body.FSWatchOwnerEmail != "" {
			err := tx.QueryRowContext(r.Context(),
				`SELECT id FROM users WHERE email = ? AND disabled = 0`,
				body.FSWatchOwnerEmail,
			).Scan(&ownerID)
			if errors.Is(err, sql.ErrNoRows) {
				return errFSWatchOwnerUnavailable
			}
			if err != nil {
				return err
			}
			allowed, err := systems.CanEnter(r.Context(), tx, ownerID, sys.ID)
			if err != nil {
				return err
			}
			if !allowed {
				return errFSWatchOwnerCannotEnter
			}
		}

		previous, err = settings.SnapshotInTx(r.Context(), tx, keys)
		if err != nil {
			return err
		}
		written, err = settings.SetManyInTx(r.Context(), tx, map[string]any{
			settings.KeyFSWatchDir:     body.FSWatchDir,
			settings.KeyFSWatchOwnerID: ownerID,
			settings.KeyFSWatchSystem:  sys.Code,
		}, time.Now().Unix())
		return err
	})
	if err != nil {
		switch {
		case errors.Is(err, errFSWatchSystemUnavailable):
			s.writeError(w, http.StatusBadRequest, "bad_system", "filing system is unavailable")
		case errors.Is(err, errFSWatchOwnerUnavailable):
			s.writeError(w, http.StatusBadRequest, "owner_not_found", "fs-watch owner is not an active user")
		case errors.Is(err, errFSWatchOwnerCannotEnter):
			s.writeError(w, http.StatusBadRequest, "owner_unavailable", "fs-watch owner cannot enter the filing system")
		case errors.Is(err, errSystemUnavailable):
			s.writeError(w, http.StatusConflict, "admin_changed", "administrator is no longer active")
		default:
			s.serverErr(w, "settings.fswatch", err)
		}
		return
	}

	if s.FSWatchReloader != nil {
		if err := s.FSWatchReloader(r.Context()); err != nil {
			restored, rollbackErr := settings.RestoreManyIfCurrent(
				r.Context(), s.DB, written, previous,
			)
			if rollbackErr != nil {
				s.Log.Error("api.settings.fswatch.rollback", "err", rollbackErr.Error())
			} else if !restored {
				s.Log.Info("api.settings.fswatch.rollback_skipped",
					"reason", "settings changed after failed reload")
			}
			message := "ingest source could not be applied"
			if restored {
				message += "; previous settings restored"
			} else {
				message += "; newer settings were preserved"
			}
			s.writeError(w, http.StatusUnprocessableEntity, "apply_failed", message)
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
	dec.UseNumber()
	dec.DisallowUnknownFields()
	if err := dec.Decode(into); err != nil {
		return err
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("request body must contain one JSON object")
		}
		return err
	}
	return nil
}

func (s *Server) serverErr(w http.ResponseWriter, tag string, err error) {
	if errors.Is(err, errSystemUnavailable) {
		s.writeError(w, http.StatusNotFound, "not_found", "system unavailable")
		return
	}
	s.Log.Error("api."+tag, "err", err.Error())
	s.writeJSON(w, http.StatusInternalServerError,
		errBody{Code: "internal", Error: "server error"})
}

func (s *Server) passwordHashUnavailable(w http.ResponseWriter, tag string, err error) {
	if s.Log != nil {
		s.Log.Error("api."+tag, "err", err.Error())
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Retry-After", "1")
	s.writeError(w, http.StatusServiceUnavailable, "password_hash_unavailable",
		"password hashing temporarily unavailable")
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

// Subscription login and catalog operations are administrator session-only.
func (s *Server) SubscriptionModels(w http.ResponseWriter, r *http.Request) {
	s.subscriptionAction(w, r, "models")
}
func (s *Server) SubscriptionLoginAction(w http.ResponseWriter, r *http.Request) {
	s.subscriptionAction(w, r, r.PathValue("action"))
}
func (s *Server) subscriptionAction(w http.ResponseWriter, r *http.Request, action string) {
	actor := s.requireAdmin(w, r)
	if actor == nil {
		return
	}
	if s.SubscriptionProvider == nil {
		s.writeError(w, 503, "subscription_unavailable", "Account subscriptions are unavailable")
		return
	}
	provider, ok := s.SubscriptionProvider(r.PathValue("provider"))
	if !ok {
		s.writeError(w, 404, "bad_subscription_provider", "Unknown subscription provider")
		return
	}
	var result any
	var err error
	switch action {
	case "start":
		result, err = provider.Start(r.Context(), actor.UserID)
	case "poll":
		result, err = provider.Poll(r.Context(), actor.UserID)
	case "cancel":
		err = provider.Cancel(r.Context(), actor.UserID)
		result = struct {
			Canceled bool `json:"canceled"`
		}{true}
	case "disconnect":
		err = provider.Disconnect(r.Context())
		result = settings.SubscriptionPoll{}
	case "models":
		if r.Method != http.MethodGet {
			s.writeError(w, 400, "bad_action", "Models requires GET")
			return
		}
		var models []settings.SubscriptionModel
		models, err = provider.Models(r.Context())
		result = struct {
			Models []settings.SubscriptionModel `json:"models"`
		}{models}
	default:
		s.writeError(w, 400, "bad_action", "Unknown login action")
		return
	}
	if err != nil {
		s.writeError(w, 502, "subscription_failed", err.Error())
		return
	}
	s.writeJSON(w, 200, result)
}

func llmMode(cfg settings.LLMConfig) string {
	if cfg.SubscriptionProvider != "" {
		return "subscription"
	}
	u, err := url.Parse(cfg.EndpointURL)
	if err == nil && u.Host != "" && !netutil.IsLocalHost(u.Host) {
		return "hosted"
	}
	return "local"
}
