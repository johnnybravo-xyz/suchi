// Package mailsetup owns the write side of the mail-mbsync wizard.
//
// The UI page (core/ui) collects provider + credentials + folders +
// port; the API endpoint (core/api) hands the request off to Apply()
// below, which atomically rewrites config/.env and (optionally) POSTs
// /containers/<name>/restart to /var/run/docker.sock so mbsync picks
// up the new creds without a full compose down/up.
//
// Both the rewrite and the restart are opt-in via env vars — see
// config.MailSetupEnvPath / MailSetupContainer. The zero-value config
// leaves the whole surface disabled.
package mailsetup

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// Provider is the wizard's provider choice. Determines default host,
// port, and TLS mode plus whether Bridge is expected as a sibling.
type Provider string

const (
	ProviderProton   Provider = "proton"
	ProviderGmail    Provider = "gmail"
	ProviderFastmail Provider = "fastmail"
	ProviderGeneric  Provider = "generic"
)

// Request is what the wizard's form POSTs (JSON, admin-only).
type Request struct {
	Provider     Provider `json:"provider"`
	MailHost     string   `json:"mail_host"`
	MailPort     int      `json:"mail_port"`
	MailSSL      string   `json:"mail_ssl"`
	MailUser     string   `json:"mail_user"`
	MailPassword string   `json:"mail_password"`
	MailFolders  string   `json:"mail_folders"`
	MaxMessages  int      `json:"max_messages"`
	MaxSize      string   `json:"max_size"`
	SyncInterval int      `json:"sync_interval_seconds"`
}

// Response reports what the wizard did (or refused to do).
type Response struct {
	Wrote            bool   `json:"wrote"`
	Restarted        bool   `json:"restarted"`
	RestartAttempted bool   `json:"restart_attempted"`
	RestartError     string `json:"restart_error,omitempty"`
	EnvPath          string `json:"env_path"`
}

// Options is the runtime config the wizard needs. Pass the empty
// zero-value to disable — Apply then returns ErrDisabled.
type Options struct {
	EnvPath    string
	Container  string
	DockerSock string
	Log        *slog.Logger
}

var (
	ErrDisabled     = errors.New("mail wizard disabled (MAIL_SETUP_ENV_PATH unset)")
	ErrInvalidInput = errors.New("invalid input")
)

// Apply validates the request, atomically rewrites the .env file, and
// (best-effort) restarts the mbsync container. Errors on validation or
// file-write; restart failure is reported in Response.RestartError so
// the operator sees it without failing the whole action.
func Apply(ctx context.Context, opts Options, req Request) (*Response, error) {
	if opts.EnvPath == "" {
		return nil, ErrDisabled
	}
	if opts.Log == nil {
		opts.Log = slog.Default()
	}
	if err := validate(req); err != nil {
		return nil, err
	}
	env := renderEnv(req)
	if err := writeAtomic(opts.EnvPath, env, 0o600); err != nil {
		return nil, fmt.Errorf("write %s: %w", opts.EnvPath, err)
	}
	resp := &Response{Wrote: true, EnvPath: opts.EnvPath}
	if opts.Container != "" && opts.DockerSock != "" {
		resp.RestartAttempted = true
		if err := restartContainer(ctx, opts.DockerSock, opts.Container); err != nil {
			opts.Log.Warn("mailsetup.restart_failed",
				"container", opts.Container, "err", err.Error())
			resp.RestartError = err.Error()
		} else {
			resp.Restarted = true
		}
	}
	return resp, nil
}

// Providers returns the wizard's provider allowlist. Keep in sync
// with the UI dropdown.
func Providers() []Provider {
	return []Provider{ProviderProton, ProviderGmail, ProviderFastmail, ProviderGeneric}
}

// Defaults returns the sane (host, port, ssl) defaults for a provider
// so the wizard's form can pre-fill.
func Defaults(p Provider) (host string, port int, ssl string) {
	switch p {
	case ProviderProton:
		return "protonmail-bridge", 143, "None"
	case ProviderGmail:
		return "imap.gmail.com", 993, "IMAPS"
	case ProviderFastmail:
		return "imap.fastmail.com", 993, "IMAPS"
	default:
		return "", 993, "IMAPS"
	}
}

// ---------- internals ----------

var (
	sslModes = map[string]bool{"IMAPS": true, "STARTTLS": true, "None": true}
	// MailUser is an IMAP username — most providers use email address
	// form, but self-hosted setups accept bare handles. Keep the check
	// loose (non-empty, no whitespace, no shell metachars) rather than
	// insisting on RFC 5322.
	userSafe = regexp.MustCompile(`^[^\s"'$\x60\\;<>]+$`)
	// MailFolders is fed to mbsync's Patterns line — quoted then joined
	// verbatim. Reject shell metachars that could break out.
	foldersSafe = regexp.MustCompile(`^[A-Za-z0-9 ._\-/*\[\]?"]+$`)
	// MailHost tolerates hostnames and IPv4 literals; anything with
	// shell / newline chars is out.
	hostSafe = regexp.MustCompile(`^[A-Za-z0-9._\-]+$`)
	// MaxSize is an mbsync size literal: digits + optional K/M/G suffix.
	sizeLiteral = regexp.MustCompile(`^\d+[KMGkmg]?$`)
)

func validate(r Request) error {
	switch r.Provider {
	case ProviderProton, ProviderGmail, ProviderFastmail, ProviderGeneric:
	default:
		return fmt.Errorf("%w: unknown provider %q", ErrInvalidInput, r.Provider)
	}
	if !hostSafe.MatchString(r.MailHost) {
		return fmt.Errorf("%w: mail_host", ErrInvalidInput)
	}
	if r.MailPort < 1 || r.MailPort > 65535 {
		return fmt.Errorf("%w: mail_port %d out of range", ErrInvalidInput, r.MailPort)
	}
	if !sslModes[r.MailSSL] {
		return fmt.Errorf("%w: mail_ssl %q (want IMAPS|STARTTLS|None)", ErrInvalidInput, r.MailSSL)
	}
	if !userSafe.MatchString(r.MailUser) {
		return fmt.Errorf("%w: mail_user", ErrInvalidInput)
	}
	if r.MailPassword == "" {
		return fmt.Errorf("%w: mail_password required", ErrInvalidInput)
	}
	folders := strings.TrimSpace(r.MailFolders)
	if folders == "" {
		folders = "INBOX"
	}
	if !foldersSafe.MatchString(folders) {
		return fmt.Errorf("%w: mail_folders", ErrInvalidInput)
	}
	if r.MaxMessages < 0 || r.MaxMessages > 100000 {
		return fmt.Errorf("%w: max_messages %d out of range", ErrInvalidInput, r.MaxMessages)
	}
	if r.MaxSize != "" && !sizeLiteral.MatchString(r.MaxSize) {
		return fmt.Errorf("%w: max_size %q (want <int>[K|M|G])", ErrInvalidInput, r.MaxSize)
	}
	if r.SyncInterval < 0 || r.SyncInterval > 86400 {
		return fmt.Errorf("%w: sync_interval_seconds", ErrInvalidInput)
	}
	return nil
}

// renderEnv produces the docker-compose config/.env body. Only the
// container-side vars mbsync + suchi read — the compose-level file
// (project profile, port, UID/GID) is unchanged by this flow.
func renderEnv(r Request) []byte {
	folders := strings.TrimSpace(r.MailFolders)
	if folders == "" {
		folders = "INBOX"
	}
	maxMsg := r.MaxMessages
	if maxMsg == 0 {
		maxMsg = 200
	}
	maxSize := r.MaxSize
	if maxSize == "" {
		maxSize = "25m"
	}
	interval := r.SyncInterval
	if interval == 0 {
		interval = 300
	}
	var b strings.Builder
	fmt.Fprintln(&b, "# Rendered by the mail-setup wizard. Do not commit.")
	fmt.Fprintf(&b, "MAIL_HOST=%s\n", r.MailHost)
	fmt.Fprintf(&b, "MAIL_PORT=%d\n", r.MailPort)
	fmt.Fprintf(&b, "MAIL_SSL=%s\n", r.MailSSL)
	fmt.Fprintf(&b, "MAIL_USER=%s\n", r.MailUser)
	fmt.Fprintf(&b, "MAIL_PASSWORD=%s\n", r.MailPassword)
	fmt.Fprintf(&b, "MAIL_FOLDERS=%q\n", folders)
	fmt.Fprintf(&b, "MAIL_MAX_MESSAGES=%d\n", maxMsg)
	fmt.Fprintf(&b, "MAIL_MAX_SIZE=%s\n", maxSize)
	fmt.Fprintf(&b, "SYNC_INTERVAL=%d\n", interval)
	return []byte(b.String())
}

// writeAtomic writes via <path>.tmp then renames so a partial write
// can't leave a corrupt .env in place.
func writeAtomic(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, mode); err != nil {
		return err
	}
	if err := os.Chmod(tmp, mode); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, path)
}

// restartContainer POSTs /containers/<name>/restart?t=5 to the Docker
// Engine socket. No CLI dep, no SDK dep — just an HTTP client whose
// transport dials the unix socket.
func restartContainer(ctx context.Context, sock, name string) error {
	client := &http.Client{
		Timeout: 30 * time.Second,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", sock)
			},
		},
	}
	// Container name may contain safe chars only — Container is set
	// via env at boot (operator-supplied), so add a belt-and-braces
	// check here too.
	if !hostSafe.MatchString(name) {
		return fmt.Errorf("bad container name %q", name)
	}
	u := "http://docker/containers/" + name + "/restart?t=5"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("docker restart: HTTP %s", resp.Status)
	}
	return nil
}

// Enabled reports whether the wizard is configured for writes. Used by
// the UI to hide the settings link when the operator hasn't opted in.
func (o Options) Enabled() bool {
	return o.EnvPath != ""
}

// String returns a redacted summary useful for the doctor / status
// endpoint; never includes any credential material.
func (o Options) String() string {
	if o.EnvPath == "" {
		return "disabled"
	}
	if o.Container == "" {
		return "write-only " + o.EnvPath
	}
	return fmt.Sprintf("write %s + restart %s via %s", o.EnvPath, o.Container, o.DockerSock)
}
