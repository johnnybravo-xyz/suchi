// Package config is the env-var-first config loader.
//
// Every knob has an env var; config.yaml is for advanced users and does not
// exist in Phase 0. The rule is: reading Load() is the ONLY place raw
// os.Getenv appears in the codebase.
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Config is the fully-resolved runtime configuration.
type Config struct {
	PublicURL      string
	DataDir        string
	ListenAddr     string
	LogLevel       string
	BodyLimit      int64
	SessionKeyPath string
	BackupInterval time.Duration
	// BackupKeep — retention window for VACUUM INTO snapshots. After
	// each successful snapshot, all-but-the-latest N are deleted.
	// 0 = keep everything (documented; not the default).
	BackupKeep int
	// AuditRetentionDays — sliding window over audit_events. Pruning
	// runs after each backup snapshot (same ticker, single-writer
	// serialization). Default 20; env caps at 100 so the notifications
	// feed can't push storage growth without an operator conscious
	// decision. 0 disables pruning.
	AuditRetentionDays int
	OCRLanguages       []string

	// DevMode gates a small pile of DX conveniences intended for local
	// iteration only: admin auto-provisioning, setup-token skip, and a
	// login helper printed on boot. Never intended for production —
	// gated by an explicit env var so it cannot be flipped by accident.
	DevMode bool
	// DevAdmin is the "email:password" pair auto-provisioned when DevMode
	// is on. Empty falls back to dev@suchi.local / devdevdev. Password is
	// argon2-hashed at boot; the plaintext never touches disk.
	DevAdmin string

	// OIDC (all-or-nothing group; empty issuer disables OIDC entirely)
	OIDCIssuerURL    string
	OIDCClientID     string
	OIDCClientSecret string
	AdminEmail       string

	// TLS (optional; empty => plain HTTP behind a proxy)
	TLSCertFile string
	TLSKeyFile  string

	// IMAP email ingest (Phase 2; parsed here so Phase 0 doctor can already
	// report the egress surface honestly)
	IngestIMAPURL        string
	IngestIMAPPassword   string
	IngestIMAPOwnerEmail string

	// Filesystem-watch ingest (Phase 2). Idle unless the owner email is
	// set — matches the design principle "opt-in, never surprise".
	IngestFSDir        string
	IngestFSOwnerEmail string

	// LLM classifier (Phase 3, opt-in). Empty endpoint = disabled.
	// Non-local endpoint requires LLMEgressAck=true; the classifier
	// plugin refuses to enable otherwise. Ollama-on-box is the
	// zero-egress recommended default.
	LLMEndpointURL string
	LLMModel       string
	LLMAPIKey      string
	LLMEgressAck   bool

	// Per-format max content byte caps for documents.content extraction.
	// Ebook formats default higher because a novel/textbook can legitimately
	// exceed the PDF-oriented default. Truncation is silent + logged at
	// Warn — the doc is still ingested, FTS still works over what fit.
	PdfMaxContentBytes  int64 // pdftotext output cap (default 8 MiB)
	EpubMaxContentBytes int64 // concatenated XHTML text cap (default 32 MiB)
	DjvuMaxContentBytes int64 // djvutxt stdout cap (default 32 MiB)

	// OCR engine selector for scanned-PDF ingest. Values:
	//   "auto"      — prefer tesseract-only (tessocr) when available,
	//                 fall back to ocrmypdf, else skip OCR.
	//   "tesseract" — force tessocr; error/skip if binaries missing.
	//   "ocrmypdf"  — force ocrmypdf; error/skip if the binary is missing.
	// tessocr is ~4× smaller in the image but produces no
	// searchable-PDF archive (documents.archive_blob stays NULL).
	OCREngine string

	// Scan-intake preprocessing knobs. Blank-page detection runs
	// after qpdf normalization and before pdf-inspector: if any pages
	// register as >= whiteness threshold, they're stripped from the
	// working copy so OCR + FTS don't waste cycles on blanks. The
	// original blob in the CAS is never touched — only the working
	// copy used for content extraction + archive_blob.
	ScanBlankRemoval            bool    // default true when pdftoppm is on PATH
	ScanBlankWhitenessThreshold float64 // 0.0-1.0; default 0.995 (99.5% white)

	// Multi-doc splitting on QR separator sheets. Opt-in — a user
	// who prints separator sheets carrying ScanSplitToken intends the
	// split; auto-detecting splits from blank pages would silently
	// break legit multipage docs. When enabled AND pdftoppm is on
	// PATH, post-ingest rasterizes each page, checks for the token,
	// and fans out each segment into its own document.
	ScanSplitEnabled bool
	ScanSplitToken   string // default "SUCHI-SPLIT"
	ScanSplitDPI     int    // default 150

	// Password-protected PDF handling.
	//
	//   IngestPasswordsFile — newline-separated candidate passwords,
	//     tried in order on every encrypted PDF ingest. Blank lines
	//     and lines starting with '#' are skipped so operators can
	//     annotate the file.
	//   DecryptKeyPath — AES-256-GCM key file for sealing operator-
	//     supplied passwords in the decryption_passwords table. Auto-
	//     generated 0600 on first boot (like the session key). Losing
	//     this file loses ALL stored passwords — operators back up
	//     DATA_DIR wholesale.
	IngestPasswordsFile string
	DecryptKeyPath      string

	// PreConsumeScript is an optional operator-defined script that runs
	// before any built-in format-specific ingest logic. See
	// docs/preconsume.mdx and core/pipeline/preconsume for the contract.
	PreConsumeScript string

	// Mail-setup wizard admin UI. Leave MailSetupEnvPath empty (default)
	// to disable the wizard entirely — the admin route 404s.
	//
	//   MailSetupEnvPath    — writable path to config/.env that the
	//     wizard rewrites when an admin submits the form. Typically
	//     bind-mounted from the compose recipe's ./config/.env.
	//   MailSetupContainer  — name of the mbsync container to kick via
	//     docker.sock after a successful rewrite. Empty disables the
	//     restart step — the wizard still writes the file and the
	//     operator restarts manually.
	//   MailSetupDockerSock — Docker Engine socket for restart POSTs.
	//     Default "/var/run/docker.sock". Ignored when MailSetupContainer
	//     is empty.
	//
	// Security note: writing to MailSetupEnvPath overwrites credentials
	// on disk. Restart requires the socket to be bind-mounted into the
	// suchi container. Both are opt-in and admin-only.
	MailSetupEnvPath    string
	MailSetupContainer  string
	MailSetupDockerSock string

	// DemoMode toggles public-showcase behaviour. When set:
	//   - the SPA renders a persistent "resets daily" banner + the
	//     /app/#/demo landing panel;
	//   - GET /api/demo/mode returns enabled=true (SPA polls this
	//     to decide whether to render the banner);
	//   - per-visitor scratch users + a reset ticker take over — see
	//     docs/demo-instance.mdx for the full operational shape.
	// Read from SUCHI_DEMO_MODE. Default off. Nothing outside the demo
	// container is expected to set it.
	DemoMode bool
	// DemoGlobalRPS caps the global per-IP request rate when DemoMode is
	// on. 0 disables the cap. Read from SUCHI_DEMO_GLOBAL_RPS.
	DemoGlobalRPS int
	// DemoScratchTTLMinutes bounds how long a per-visitor scratch user
	// (and its uploads) survive before the reset ticker sweeps them.
	// Read from SUCHI_DEMO_SCRATCH_TTL_MINUTES; default 30. The ticker
	// runs every ceil(TTL/2) minutes so an expiry never lingers more
	// than TTL past its deadline.
	DemoScratchTTLMinutes int

	// UIDisabled turns off the built-in server-rendered UI at boot.
	// Set SUCHI_UI_DISABLED=1 for headless deployments where an
	// external SPA (React/Svelte/whatever) fronts /api/. When true,
	// none of /, /docs/{id}, /inbox, /upload, /admin/*, /pending-
	// decryption, /login, /bootstrap register — the mux only serves
	// /api/*, /healthz, /readyz, /metrics, /assets/* (kept so /api/
	// consumers can still reach the manifest + favicon if they want).
	// Existing /api/ auth (Token/Bearer/OIDC) applies unchanged.
	UIDisabled bool
}

// Load reads env vars and returns a validated Config. It is intended to be
// called exactly once at process start.
func Load() (*Config, error) {
	c := &Config{
		PublicURL:            env("PUBLIC_URL", ""),
		DataDir:              env("DATA_DIR", "/data"),
		ListenAddr:           env("LISTEN_ADDR", ":8000"),
		LogLevel:             env("LOG_LEVEL", "info"),
		OCRLanguages:         splitCSV(env("OCR_LANGUAGES", "eng")),
		OIDCIssuerURL:        env("OIDC_ISSUER_URL", ""),
		OIDCClientID:         env("OIDC_CLIENT_ID", ""),
		AdminEmail:           env("ADMIN_EMAIL", ""),
		TLSCertFile:          env("TLS_CERT_FILE", ""),
		TLSKeyFile:           env("TLS_KEY_FILE", ""),
		IngestIMAPURL:        env("INGEST_IMAP_URL", ""),
		IngestIMAPOwnerEmail: env("INGEST_IMAP_OWNER_EMAIL", ""),
		DevMode:              env("SUCHI_DEV", "") == "1",
		DevAdmin:             env("SUCHI_DEV_ADMIN", ""),
		IngestFSDir:          env("INGEST_FS_DIR", ""),
		IngestFSOwnerEmail:   env("INGEST_FS_OWNER_EMAIL", ""),
		LLMEndpointURL:       env("LLM_ENDPOINT_URL", ""),
		LLMModel:             env("LLM_MODEL", ""),
		LLMEgressAck:         env("LLM_EGRESS_ACK", "") == "true",
	}
	if c.IngestFSDir == "" && c.IngestFSOwnerEmail != "" {
		c.IngestFSDir = filepath.Join(c.DataDir, "staging")
	}

	var err error
	// BODY_LIMIT applies to every HTTP request body (uploads,
	// PATCHes, JSON POSTs). 500M by default because scanned PDFs
	// routinely exceed the old 100M ceiling. `UPLOAD_MAX_BYTES` is
	// accepted as an alias for the same value — the review used it,
	// so we honor both. `0` disables the cap (for the operators who
	// insist).
	uploadEnv := env("UPLOAD_MAX_BYTES", "")
	if uploadEnv == "" {
		uploadEnv = env("BODY_LIMIT", "500M")
	}
	if c.BodyLimit, err = parseBytes(uploadEnv); err != nil {
		return nil, fmt.Errorf("UPLOAD_MAX_BYTES / BODY_LIMIT: %w", err)
	}
	if c.BackupKeep, err = parseIntBounded("BACKUP_KEEP", env("BACKUP_KEEP", "7"), 0, 10_000); err != nil {
		return nil, err
	}
	if c.BackupInterval, err = time.ParseDuration(env("BACKUP_INTERVAL", "24h")); err != nil {
		return nil, fmt.Errorf("BACKUP_INTERVAL: %w", err)
	}
	if c.AuditRetentionDays, err = parseIntBounded("AUDIT_RETENTION_DAYS",
		env("AUDIT_RETENTION_DAYS", "20"), 0, 100); err != nil {
		return nil, err
	}
	if c.PdfMaxContentBytes, err = parseBytes(env("PDF_MAX_CONTENT_BYTES", "8M")); err != nil {
		return nil, fmt.Errorf("PDF_MAX_CONTENT_BYTES: %w", err)
	}
	if c.EpubMaxContentBytes, err = parseBytes(env("EPUB_MAX_CONTENT_BYTES", "32M")); err != nil {
		return nil, fmt.Errorf("EPUB_MAX_CONTENT_BYTES: %w", err)
	}
	if c.DjvuMaxContentBytes, err = parseBytes(env("DJVU_MAX_CONTENT_BYTES", "32M")); err != nil {
		return nil, fmt.Errorf("DJVU_MAX_CONTENT_BYTES: %w", err)
	}
	c.OCREngine = strings.ToLower(env("OCR_ENGINE", "auto"))
	switch c.OCREngine {
	case "auto", "tesseract", "ocrmypdf":
	default:
		return nil, fmt.Errorf("OCR_ENGINE: unknown value %q (want auto|tesseract|ocrmypdf)", c.OCREngine)
	}

	c.ScanBlankRemoval = strings.ToLower(env("SCAN_BLANK_REMOVAL", "auto")) != "off"
	c.ScanBlankWhitenessThreshold = 0.995
	if s := env("SCAN_BLANK_WHITENESS_THRESHOLD", ""); s != "" {
		f, err := strconv.ParseFloat(s, 64)
		if err != nil || f <= 0 || f > 1 {
			return nil, fmt.Errorf("SCAN_BLANK_WHITENESS_THRESHOLD: want float in (0,1], got %q", s)
		}
		c.ScanBlankWhitenessThreshold = f
	}

	c.ScanSplitEnabled = strings.ToLower(env("SCAN_SPLIT_ENABLED", "off")) == "on"
	c.ScanSplitToken = env("SCAN_SPLIT_TOKEN", "SUCHI-SPLIT")
	c.ScanSplitDPI = 150
	if s := env("SCAN_SPLIT_DPI", ""); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n < 72 || n > 600 {
			return nil, fmt.Errorf("SCAN_SPLIT_DPI: want integer in [72,600], got %q", s)
		}
		c.ScanSplitDPI = n
	}

	c.IngestPasswordsFile = env("INGEST_PASSWORDS_FILE", "")
	c.DecryptKeyPath = env("DECRYPT_KEY_FILE", filepath.Join(c.DataDir, ".decrypt-key"))
	c.PreConsumeScript = env("PRE_CONSUME_SCRIPT", "")

	c.MailSetupEnvPath = env("MAIL_SETUP_ENV_PATH", "")
	c.MailSetupContainer = env("MAIL_SETUP_CONTAINER", "suchi-mail-mbsync")
	c.MailSetupDockerSock = env("MAIL_SETUP_DOCKER_SOCK", "/var/run/docker.sock")

	c.UIDisabled = env("SUCHI_UI_DISABLED", "") == "true" ||
		env("SUCHI_UI_DISABLED", "") == "1"

	c.DemoMode = env("SUCHI_DEMO_MODE", "") == "true" ||
		env("SUCHI_DEMO_MODE", "") == "1"
	if c.DemoGlobalRPS, err = parseIntBounded("SUCHI_DEMO_GLOBAL_RPS",
		env("SUCHI_DEMO_GLOBAL_RPS", "0"), 0, 10_000); err != nil {
		return nil, err
	}
	if c.DemoScratchTTLMinutes, err = parseIntBounded("SUCHI_DEMO_SCRATCH_TTL_MINUTES",
		env("SUCHI_DEMO_SCRATCH_TTL_MINUTES", "30"), 1, 24*60); err != nil {
		return nil, err
	}

	// Secrets support _FILE convention for docker/k8s secret mounts.
	if c.OIDCClientSecret, err = readSecret("OIDC_CLIENT_SECRET"); err != nil {
		return nil, err
	}
	if c.IngestIMAPPassword, err = readSecret("INGEST_IMAP_PASSWORD"); err != nil {
		return nil, err
	}
	if c.LLMAPIKey, err = readSecret("LLM_API_KEY"); err != nil {
		return nil, err
	}

	c.SessionKeyPath = env("SESSION_KEY_FILE", filepath.Join(c.DataDir, ".session-key"))

	if c.PublicURL == "" {
		return nil, errors.New("PUBLIC_URL is required")
	}
	if (c.TLSCertFile == "") != (c.TLSKeyFile == "") {
		return nil, errors.New("TLS_CERT_FILE and TLS_KEY_FILE must both be set or both unset")
	}
	if c.OIDCIssuerURL != "" {
		if c.OIDCClientID == "" || c.OIDCClientSecret == "" {
			return nil, errors.New("OIDC configured but OIDC_CLIENT_ID or OIDC_CLIENT_SECRET missing")
		}
		if c.AdminEmail == "" {
			return nil, errors.New("ADMIN_EMAIL is required when OIDC is enabled")
		}
	}

	return c, nil
}

func env(k, def string) string {
	if v, ok := os.LookupEnv(k); ok {
		return v
	}
	return def
}

// readSecret returns $KEY or the contents of $KEY_FILE. The _FILE variant
// wins if both are set — matches docker/k8s conventions.
func readSecret(key string) (string, error) {
	if p := os.Getenv(key + "_FILE"); p != "" {
		b, err := os.ReadFile(p)
		if err != nil {
			return "", fmt.Errorf("%s_FILE: %w", key, err)
		}
		return strings.TrimRight(string(b), "\r\n"), nil
	}
	return os.Getenv(key), nil
}

func splitCSV(s string) []string {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := parts[:0]
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// parseIntBounded parses `s` as a base-10 int and enforces `min <=
// value <= max`. The name is folded into the error so operators see
// which env var was bad.
func parseIntBounded(name, s string, min, max int) (int, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("%s: empty", name)
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", name, err)
	}
	if n < min || n > max {
		return 0, fmt.Errorf("%s: %d out of range [%d, %d]", name, n, min, max)
	}
	return n, nil
}

// parseBytes accepts "100M", "1G", "512K", or a raw byte count.
func parseBytes(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, errors.New("empty")
	}
	mult := int64(1)
	switch last := s[len(s)-1]; last {
	case 'K', 'k':
		mult = 1 << 10
	case 'M', 'm':
		mult = 1 << 20
	case 'G', 'g':
		mult = 1 << 30
	default:
		if last < '0' || last > '9' {
			return 0, fmt.Errorf("bad suffix %q", last)
		}
	}
	if mult > 1 {
		s = s[:len(s)-1]
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, err
	}
	return n * mult, nil
}
