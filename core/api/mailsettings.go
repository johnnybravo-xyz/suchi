// Mail-intake settings handlers for the SPA Admin panel.
//
//	GET  /api/admin/settings/mail      → returns the stored config
//	                                     (password absent — write-only)
//	PUT  /api/admin/settings/mail      → persists; blank password keeps
//	                                     the stored one; validates host/
//	                                     username; save is boot-load
//	                                     (no live reload today).
//	POST /api/admin/settings/mail/test → dials IMAP with the submitted
//	                                     creds (or stored ones if body
//	                                     is empty) and returns
//	                                     {ok, message}.
//
// Wire shape is the SPA's flat form (imap_host, imap_port, username,
// password, folder, poll_interval_min, owner_email). At boot, main.go
// stitches Host/Port/Username/Folder into the imaps://user@host/FOLDER
// URL that emailwatch actually reads.

package api

import (
	"crypto/tls"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"

	imapclient "github.com/emersion/go-imap/client"

	"github.com/johnnybravo-xyz/suchi/core/audit"
	"github.com/johnnybravo-xyz/suchi/core/auth"
	"github.com/johnnybravo-xyz/suchi/core/settings"
)

// MailSettings is the wire shape. Fields align with lib/MailForm.svelte.
type MailSettings struct {
	IMAPHost        string `json:"imap_host"`
	IMAPPort        int    `json:"imap_port"`
	Username        string `json:"username"`
	Password        string `json:"password,omitempty"`
	Folder          string `json:"folder"`
	PollIntervalMin int    `json:"poll_interval_min"`
	OwnerEmail      string `json:"owner_email"`
}

// GetMailSettings — GET /api/admin/settings/mail. Admin-only.
// Returns 200 with the stored config; password is omitted. A
// zero-value response is fine — the SPA treats empty fields as
// "not configured yet".
func (s *Server) GetMailSettings(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	cfg := settings.ResolveEmailWatchConfig(r.Context(), s.DB, settings.EmailWatchConfig{})
	if cfg.Folder == "" {
		cfg.Folder = "INBOX"
	}
	if cfg.Port == 0 {
		cfg.Port = 993
	}
	if cfg.PollIntervalMin == 0 {
		cfg.PollIntervalMin = 10
	}
	s.writeJSON(w, http.StatusOK, MailSettings{
		IMAPHost:        cfg.Host,
		IMAPPort:        cfg.Port,
		Username:        cfg.Username,
		Folder:          cfg.Folder,
		PollIntervalMin: cfg.PollIntervalMin,
		OwnerEmail:      cfg.OwnerEmail,
	})
}

// PutMailSettings — PUT /api/admin/settings/mail. Admin-only.
// Persists every submitted field. A blank Password field means
// "keep the stored one" — the SPA relies on this for edit-without-
// re-typing. Live reload isn't wired for emailwatch; the response
// carries restart_required=true so the SPA can surface it.
func (s *Server) PutMailSettings(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	var body MailSettings
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		s.writeError(w, http.StatusBadRequest, "bad_json", err.Error())
		return
	}
	body.IMAPHost = strings.TrimSpace(body.IMAPHost)
	body.Username = strings.TrimSpace(body.Username)
	body.Folder = strings.TrimSpace(body.Folder)
	body.OwnerEmail = strings.TrimSpace(body.OwnerEmail)
	if body.IMAPHost == "" || body.Username == "" {
		s.writeError(w, http.StatusBadRequest, "missing_fields",
			"imap_host and username are required")
		return
	}
	if body.IMAPPort <= 0 || body.IMAPPort > 65535 {
		body.IMAPPort = 993
	}
	if body.Folder == "" {
		body.Folder = "INBOX"
	}
	if body.PollIntervalMin <= 0 {
		body.PollIntervalMin = 10
	}

	writes := []struct {
		key string
		val any
	}{
		{settings.KeyIMAPHost, body.IMAPHost},
		{settings.KeyIMAPPort, body.IMAPPort},
		{settings.KeyIMAPUsername, body.Username},
		{settings.KeyIMAPFolder, body.Folder},
		{settings.KeyIMAPPollIntervalMin, body.PollIntervalMin},
		{settings.KeyIMAPOwnerEmail, body.OwnerEmail},
	}
	for _, wr := range writes {
		if err := settings.Set(r.Context(), s.DB, wr.key, wr.val); err != nil {
			s.serverErr(w, "settings.mail."+wr.key, err)
			return
		}
	}
	// Password is sensitive. Persist only when explicitly supplied —
	// blank leaves the previous value intact. Stored plaintext today,
	// matching the LLM API-key precedent in setup.go; AEAD seal is a
	// follow-up. WARN so the operator sees it.
	if body.Password != "" {
		s.Log.Warn("settings.mail.password.plaintext",
			"note", "storing plaintext until AEAD seal wired; back up DATA_DIR wholesale")
		if err := settings.Set(r.Context(), s.DB, settings.KeyIMAPPasswordSealed, body.Password); err != nil {
			s.serverErr(w, "settings.mail.password", err)
			return
		}
	}
	audit.Log(r.Context(), s.DB, s.Log, audit.Event{
		Actor:      auth.FromContext(r.Context()),
		Action:     "settings.mail.update",
		ObjectKind: "settings",
		After: map[string]any{
			"imap_host":         body.IMAPHost,
			"username":          body.Username,
			"folder":            body.Folder,
			"poll_interval_min": body.PollIntervalMin,
			"owner_email":       body.OwnerEmail,
		},
	})
	s.writeJSON(w, http.StatusOK, map[string]any{
		"saved":            true,
		"restart_required": true,
		"restart_hint":     "settings saved; restart suchi to pick up the new mail intake config",
	})
}

// TestMailSettings — POST /api/admin/settings/mail/test. Admin-only.
// Body is optional: when present, tests the submitted creds; when
// empty, tests the stored ones. Returns {ok:true, message} on
// success, or {ok:false, message} on failure. Never 500s on a
// connect-refused: that's user data being wrong, not a server bug.
func (s *Server) TestMailSettings(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	var body MailSettings
	// An empty request body is fine — decoder returns io.EOF then; we
	// fall through to stored creds.
	if r.ContentLength > 0 {
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			s.writeError(w, http.StatusBadRequest, "bad_json", err.Error())
			return
		}
	}
	body.IMAPHost = strings.TrimSpace(body.IMAPHost)
	body.Username = strings.TrimSpace(body.Username)

	// Fill blanks from stored settings so "click Test" works after a
	// GET-then-Save round-trip, and so "just retype the password" tests
	// against the previously-configured host.
	cfg := settings.ResolveEmailWatchConfig(r.Context(), s.DB, settings.EmailWatchConfig{})
	if body.IMAPHost == "" {
		body.IMAPHost = cfg.Host
	}
	if body.IMAPPort <= 0 {
		body.IMAPPort = cfg.Port
	}
	if body.Username == "" {
		body.Username = cfg.Username
	}
	if body.Password == "" {
		body.Password = cfg.Password
	}
	if body.IMAPPort <= 0 || body.IMAPPort > 65535 {
		body.IMAPPort = 993
	}
	if body.IMAPHost == "" || body.Username == "" || body.Password == "" {
		s.writeJSON(w, http.StatusOK, map[string]any{
			"ok": false,
			"message": "host, username, and password must all be provided " +
				"(either in this request or already saved)",
		})
		return
	}

	addr := fmt.Sprintf("%s:%d", body.IMAPHost, body.IMAPPort)
	dialer := &tls.Dialer{
		NetDialer: &net.Dialer{Timeout: 5 * time.Second},
		Config:    &tls.Config{ServerName: body.IMAPHost, MinVersion: tls.VersionTLS12},
	}
	conn, err := dialer.DialContext(r.Context(), "tcp", addr)
	if err != nil {
		s.writeJSON(w, http.StatusOK, map[string]any{
			"ok":      false,
			"message": "connect: " + err.Error(),
		})
		return
	}
	c, err := imapclient.New(conn)
	if err != nil {
		_ = conn.Close()
		s.writeJSON(w, http.StatusOK, map[string]any{
			"ok":      false,
			"message": "imap handshake: " + err.Error(),
		})
		return
	}
	defer c.Logout()

	if err := c.Login(body.Username, body.Password); err != nil {
		s.writeJSON(w, http.StatusOK, map[string]any{
			"ok":      false,
			"message": "login: " + err.Error(),
		})
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{
		"ok":      true,
		"message": "Connected — mailbox reachable.",
	})
}
