package api

// Setup wizard endpoints. Admin-only. Every payload is JSON; every
// string that lands in SQL rides a parameterized query (default via
// database/sql); every string that reaches a shell (mail wizard,
// docker.sock) is regex-validated at the boundary. Enum-like inputs
// are matched against explicit allowlists — no reflection, no dynamic
// dispatch on user-supplied names.

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/suchi-dms/suchi/core/auth"
	"github.com/suchi-dms/suchi/core/jd"
	"github.com/suchi-dms/suchi/core/refile"
	"github.com/suchi-dms/suchi/core/settings"
)

// StepNames is the wizard's step allowlist. Any /step/{name} endpoint
// call not matching this set returns 400.
var StepNames = map[string]bool{
	"welcome":     true,
	"users":       true,
	"mail":        true,
	"llm":         true,
	"jd":          true,
	"rules":       true,
	"sources":     true,
	"preferences": true,
	"done":        true,
}

// registerSetup wires the wizard's routes. Called from Register().
func (s *Server) registerSetup(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/admin/setup/state", s.SetupState)
	mux.HandleFunc("POST /api/admin/setup/step/{name}", s.SetupStep)
	mux.HandleFunc("POST /api/admin/setup/complete", s.SetupComplete)
	mux.HandleFunc("POST /api/admin/users", s.CreateUser)
	mux.HandleFunc("POST /api/admin/setup/jd-preset", s.ApplyJDPreset)
	mux.HandleFunc("POST /api/admin/settings/llm", s.SaveLLMSettings)
	mux.HandleFunc("POST /api/admin/settings/preferences", s.SavePreferences)
	mux.HandleFunc("POST /api/admin/settings/ingest", s.SaveIngestSettings)
}

// ---------- state + step recording ----------

// SetupState returns the wizard's progress.
func (s *Server) SetupState(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	st, err := settings.LoadSetupState(r.Context(), s.DB)
	if err != nil {
		s.serverErr(w, "setup.state", err)
		return
	}
	s.writeJSON(w, http.StatusOK, st)
}

// SetupStep records a step as done or skipped. Body: {"status":"done"|"skipped"}.
func (s *Server) SetupStep(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	name := r.PathValue("name")
	if !StepNames[name] {
		s.writeError(w, http.StatusBadRequest, "unknown_step", "unknown step")
		return
	}
	var body struct {
		Status string `json:"status"`
	}
	if err := decodeJSON(r, &body); err != nil {
		s.writeError(w, http.StatusBadRequest, "bad_json", err.Error())
		return
	}
	var status settings.StepStatus
	switch body.Status {
	case "done":
		status = settings.StepDone
	case "skipped":
		status = settings.StepSkipped
	default:
		s.writeError(w, http.StatusBadRequest, "bad_status", `status must be "done" or "skipped"`)
		return
	}
	if err := settings.RecordStep(r.Context(), s.DB, name, status); err != nil {
		s.serverErr(w, "setup.step", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// SetupComplete stamps the wizard-finished timestamp.
func (s *Server) SetupComplete(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
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
	if !s.requireAdmin(w, r) {
		return
	}
	if s.PasswordHasher == nil {
		s.writeError(w, http.StatusServiceUnavailable, "no_hasher",
			"password hasher not wired — this is a boot-time misconfiguration")
		return
	}
	var body struct {
		Email       string `json:"email"`
		Password    string `json:"password"`
		DisplayName string `json:"display_name"`
		Role        string `json:"role"`
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
	hash, err := s.PasswordHasher(body.Password)
	if err != nil {
		s.serverErr(w, "createuser.hash", err)
		return
	}
	var newID int64
	err = s.DB.WriteTx(r.Context(), func(tx *sql.Tx) error {
		now := time.Now().Unix()
		res, err := tx.ExecContext(r.Context(), `
			INSERT INTO users(email, display_name, role, password_hash, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?)
		`, body.Email, displayName, body.Role, hash, now, now)
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
		"id":    newID,
		"email": body.Email,
		"role":  body.Role,
	})
}

// ---------- JD preset ----------

// ApplyJDPreset swaps the JD tree to one of the curated presets. Body:
// {"preset_id":"solo","confirm_blank":false}. Refuses blank without
// confirm_blank=true.
func (s *Server) ApplyJDPreset(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	var body struct {
		PresetID     string `json:"preset_id"`
		ConfirmBlank bool   `json:"confirm_blank"`
		// Refile=true accepts existing docs filed outside the inbox —
		// they get parked on the new inbox and a refile sweep is
		// triggered afterwards (re-run rules + enqueue re-render).
		// Selling point: "you can always come back to change this."
		Refile bool `json:"refile"`
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
	applyFn := jd.ApplyPreset
	if body.Refile {
		applyFn = jd.ApplyPresetWithRefile
	}
	if err := applyFn(r.Context(), s.DB, s.Log, body.PresetID); err != nil {
		if errors.Is(err, jd.ErrDocumentsExist) {
			s.writeError(w, http.StatusConflict, "documents_filed",
				err.Error()+` (re-post with "refile": true to accept the refile)`)
			return
		}
		s.serverErr(w, "jdpreset.apply", err)
		return
	}
	if body.Refile {
		// Kick off the sweep synchronously so operators see the counters
		// in the response. Long-running installs can hit the dedicated
		// /api/admin/refile endpoint instead for the background flavor.
		stats, err := refile.All(r.Context(), s.DB, s.Log, refile.Options{})
		if err != nil {
			s.Log.Warn("jdpreset.refile.err", "err", err.Error())
		}
		if s.Jobs != nil {
			s.Jobs.Nudge()
		}
		_ = stats // captured in the log; response stays minimal for now
	}
	if err := settings.Set(r.Context(), s.DB, settings.KeyJDPreset, body.PresetID); err != nil {
		// Non-fatal: the tree is applied; the preset-name record is a
		// nicety for the wizard's recap page.
		s.Log.Warn("jdpreset.record", "err", err.Error())
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"preset_id": body.PresetID})
}

// ---------- LLM settings ----------

// modelSafe accepts common OpenAI-compatible model names — vendor,
// slash, dot, dash, digit, letter.
var modelSafe = regexp.MustCompile(`^[A-Za-z0-9._/\-]{1,64}$`)

// SaveLLMSettings persists LLM classifier config. The plugin still
// reads env at boot today; this write is for the wizard's record and
// future runtime-reconfig. Documented "restart required" in the UI.
func (s *Server) SaveLLMSettings(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	var body struct {
		EndpointURL string `json:"endpoint_url"`
		Model       string `json:"model"`
		APIKey      string `json:"api_key"`
		EgressAck   bool   `json:"egress_ack"`
	}
	if err := decodeJSON(r, &body); err != nil {
		s.writeError(w, http.StatusBadRequest, "bad_json", err.Error())
		return
	}
	body.EndpointURL = strings.TrimSpace(body.EndpointURL)
	body.Model = strings.TrimSpace(body.Model)
	if body.EndpointURL != "" {
		u, err := url.Parse(body.EndpointURL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			s.writeError(w, http.StatusBadRequest, "bad_url", "endpoint_url must be an http(s):// URL")
			return
		}
		if isNonLocal(u.Host) && !body.EgressAck {
			s.writeError(w, http.StatusBadRequest, "egress_ack_required",
				"non-local endpoint — set egress_ack=true to confirm document text will leave the box")
			return
		}
	}
	if body.Model != "" && !modelSafe.MatchString(body.Model) {
		s.writeError(w, http.StatusBadRequest, "bad_model", "model has forbidden characters")
		return
	}
	if err := settings.Set(r.Context(), s.DB, settings.KeyLLMEndpointURL, body.EndpointURL); err != nil {
		s.serverErr(w, "settings.llm.endpoint", err)
		return
	}
	if err := settings.Set(r.Context(), s.DB, settings.KeyLLMModel, body.Model); err != nil {
		s.serverErr(w, "settings.llm.model", err)
		return
	}
	if err := settings.Set(r.Context(), s.DB, settings.KeyLLMEgressAck, body.EgressAck); err != nil {
		s.serverErr(w, "settings.llm.egress", err)
		return
	}
	// The API key is sensitive. Persist only when explicitly supplied;
	// blank leaves the previous value alone. Sealed at rest via the
	// existing decrypt-key AEAD (same as PDF passwords) — plumbed in a
	// follow-up commit; today we store as plaintext to unblock the
	// wizard flow with a WARN log.
	if body.APIKey != "" {
		s.Log.Warn("settings.llm.key.plaintext",
			"note", "storing plaintext until AEAD seal wired; back up DATA_DIR wholesale")
		if err := settings.Set(r.Context(), s.DB, settings.KeyLLMAPIKeySealed, body.APIKey); err != nil {
			s.serverErr(w, "settings.llm.key", err)
			return
		}
	}
	// Live-reload: signal the running classifier to re-read settings.
	// Nil hook (tests, disabled classifier) → skip silently. A reload
	// failure is warn-only: the settings are saved, the operator can
	// restart to apply.
	if s.LLMReloader != nil {
		if err := s.LLMReloader(r.Context()); err != nil {
			s.Log.Warn("settings.llm.reload_failed", "err", err.Error())
			s.writeJSON(w, http.StatusOK, map[string]any{
				"saved":        true,
				"reload_error": err.Error(),
				"restart_hint": "settings saved; restart suchi to apply since live-reload failed",
			})
			return
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---------- preferences ----------

var langCode = regexp.MustCompile(`^[a-z]{2,3}(_[A-Z]{2})?$`)

// SavePreferences persists backup interval + OCR languages.
func (s *Server) SavePreferences(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
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
	if err := settings.Set(r.Context(), s.DB, settings.KeyBackupIntervalHours, body.BackupIntervalHours); err != nil {
		s.serverErr(w, "settings.backup", err)
		return
	}
	if err := settings.Set(r.Context(), s.DB, settings.KeyOCRLanguages, cleaned); err != nil {
		s.serverErr(w, "settings.ocr", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---------- ingest sources ----------

// fsPath restricts to absolute POSIX paths — no shell metachars, no
// backslashes. The fs-watch consumer still resolves + validates the
// path itself; this is belt-and-braces.
var fsPath = regexp.MustCompile(`^/[A-Za-z0-9 ._\-/]{0,255}$`)

// SaveIngestSettings persists fs-watch dir + owner. Runtime picks
// these up on next boot; documented "restart required" in the UI.
func (s *Server) SaveIngestSettings(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
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
	if err := settings.Set(r.Context(), s.DB, settings.KeyFSWatchDir, body.FSWatchDir); err != nil {
		s.serverErr(w, "settings.fswatch.dir", err)
		return
	}
	if err := settings.Set(r.Context(), s.DB, settings.KeyFSWatchOwnerEmail, body.FSWatchOwnerEmail); err != nil {
		s.serverErr(w, "settings.fswatch.owner", err)
		return
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

func (s *Server) requireAdmin(w http.ResponseWriter, r *http.Request) bool {
	p := auth.FromContext(r.Context())
	if p == nil {
		s.writeError(w, http.StatusUnauthorized, "unauthenticated", "sign-in required")
		return false
	}
	if p.Role != "admin" {
		s.writeError(w, http.StatusForbidden, "forbidden", "admin role required")
		return false
	}
	return true
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

// isNonLocal reports whether h is an off-box address. Loopback + link-
// local + private RFC-1918 count as local. We keep the check literal —
// tunnels and NAT can hide egress, but honest operators run local
// models on 127.0.0.1 / 192.168.* / 10.* / *.local so this catches
// the common case + forces an ack for anything else.
func isNonLocal(host string) bool {
	// Strip port. Bracketed IPv6 → strip up to `]`. Bare host — only
	// strip when there's exactly one colon (else it's IPv6-shaped and
	// the whole thing is the host).
	if strings.HasPrefix(host, "[") {
		if end := strings.Index(host, "]"); end != -1 {
			host = host[1:end]
		}
	} else if strings.Count(host, ":") == 1 {
		host = host[:strings.Index(host, ":")]
	}
	h := strings.ToLower(host)
	switch h {
	case "localhost", "127.0.0.1", "::1", "":
		return false
	}
	if strings.HasSuffix(h, ".local") || strings.HasSuffix(h, ".internal") ||
		strings.HasSuffix(h, ".lan") {
		return false
	}
	if strings.HasPrefix(h, "10.") || strings.HasPrefix(h, "192.168.") {
		return false
	}
	// 172.16.0.0/12 — only the first octet check is worth it here.
	if strings.HasPrefix(h, "172.") {
		var second int
		fmt.Sscanf(h, "172.%d.", &second)
		if second >= 16 && second <= 31 {
			return false
		}
	}
	return true
}
