// SPDX-License-Identifier: AGPL-3.0-or-later

// Package config is the env-var-first config loader.
//
// Application settings are resolved from environment variables and an
// optional config file.
package config

import (
	"errors"
	"fmt"
	"math"
	"net"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/johnnybravo-xyz/suchi/core/asn"
	"github.com/johnnybravo-xyz/suchi/core/netutil"
)

// Config is the fully-resolved runtime configuration.
type Config struct {
	PublicURL       string
	DataDir         string
	ListenAddr      string
	SharePublicURL  string
	ShareListenAddr string
	LogLevel        string
	PprofEnabled    bool
	BodyLimit       int64
	BackupInterval  time.Duration
	// TrustedProxyCIDRs enables forwarded client addresses for rate limiting
	// only when the direct TCP peer belongs to an explicitly trusted network.
	TrustedProxyCIDRs []netip.Prefix
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
	// TODO(ocr-languages): discover installed language data, validate selections,
	// and test mixed-script OCR. See docs/formats.mdx.
	OCRLanguages []string
	// DeviceOCRMinConfidence controls when accepted mobile OCR becomes
	// provisional searchable text. Provenance is retained below the threshold.
	DeviceOCRMinConfidence float64

	// DevMode gates a small pile of DX conveniences intended for local
	// iteration only: admin auto-provisioning, setup-token skip, and a
	// login helper printed on boot. Never intended for production —
	// gated by an explicit env var so it cannot be flipped by accident.
	DevMode     bool
	DevAllowLAN bool

	// OIDC requires signed ID-token email + email_verified=true claims.
	// All-or-nothing group; empty issuer disables OIDC entirely.
	OIDCIssuerURL    string
	OIDCClientID     string
	OIDCClientSecret string
	AdminEmail       string

	// TLS (optional; empty => plain HTTP behind a proxy)
	TLSCertFile string
	TLSKeyFile  string

	// IngestIMAPOAuthClientIDMicrosoft is the operator's Entra public-client
	// application ID for Outlook / M365 device-code auth. Empty uses Suchi's
	// shipped registration; public clients carry no client secret.
	IngestIMAPOAuthClientIDMicrosoft string
	// IngestIMAPOAuthScopesMicrosoft is a comma-separated list of MSAL
	// resource scopes. Empty falls back to oauth.DefaultScopes (Exchange IMAP).
	// MSAL adds its required OIDC scopes, including offline_access.
	IngestIMAPOAuthScopesMicrosoft string

	// Filesystem-watch ingest. Idle unless the owner email is set.
	// Recursive discovery defaults on; INGEST_FS_RECURSIVE=off retains
	// top-level-only discovery for unusually large or constrained trees.
	IngestFSDir        string
	IngestFSOwnerEmail string
	IngestFSSystem     string
	IngestFSRecursive  bool

	// LLM classifier (opt-in). Empty endpoint = disabled.
	// Non-local endpoint requires LLMEgressAck=true; the classifier
	// plugin refuses to enable otherwise. Ollama-on-box is the
	// zero-egress recommended default.
	LLMEndpointURL         string
	LLMModel               string
	LLMAPIKey              string
	LLMEgressAck           bool
	LLMConfidenceThreshold float64

	// Per-format documents.content caps. Truncation is logged and ingest
	// continues with the content that fit.
	PdfMaxContentBytes    int64
	AnyDocMaxContentBytes int64
	DjvuMaxContentBytes   int64

	// Ordered OCR engines for scanned PDFs and raster images. "auto" keeps the
	// local default and must be the only entry. External engines are opt-in.
	OCREngines []string
	// The HTTP engine calls one operator-configured endpoint that implements
	// Suchi's fixed multipart request and Docling-compatible response contract.
	OCRHTTPURL       string
	OCRHTTPAPIKey    string
	OCRHTTPTimeout   time.Duration
	ExtractEgressAck bool

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
	// External archive serial number labels are opt-in and independent from
	// disposable legacy separator sheets.
	ScanASNMode   string // off|assign|split; default "off"
	ScanASNPrefix string // default "ASN"

	// Password-protected PDF handling.
	//
	//   IngestPasswordsFile — newline-separated candidate passwords,
	//     tried in order on every encrypted PDF ingest. Blank lines
	//     and lines starting with '#' are skipped so operators can
	//     annotate the file.
	//   DecryptKeyPath — AES-256-GCM key file for sealing operator-
	//     supplied passwords in the decryption_passwords table. Auto-
	//     generated 0600 on first boot. Losing
	//     this file loses ALL stored passwords — operators back up
	//     DATA_DIR wholesale.
	IngestPasswordsFile string
	DecryptKeyPath      string

	// PreConsumeScript is an optional operator-defined script that runs
	// before any built-in format-specific ingest logic. See
	// docs/preconsume.mdx and core/pipeline/preconsume for the contract.
	PreConsumeScript string

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
	// DemoGlobalRPS caps demo session endpoints per IP. 0 disables it.
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
		PublicURL:                        env("PUBLIC_URL", ""),
		DataDir:                          env("DATA_DIR", "/data"),
		ListenAddr:                       env("LISTEN_ADDR", ":8000"),
		SharePublicURL:                   env("SHARE_PUBLIC_URL", ""),
		ShareListenAddr:                  env("SHARE_LISTEN_ADDR", ""),
		LogLevel:                         env("LOG_LEVEL", "info"),
		PprofEnabled:                     env("SUCHI_PPROF", "") == "1",
		OCRLanguages:                     splitCSV(env("OCR_LANGUAGES", "eng")),
		OIDCIssuerURL:                    env("OIDC_ISSUER_URL", ""),
		OIDCClientID:                     env("OIDC_CLIENT_ID", ""),
		AdminEmail:                       env("ADMIN_EMAIL", ""),
		TLSCertFile:                      env("TLS_CERT_FILE", ""),
		TLSKeyFile:                       env("TLS_KEY_FILE", ""),
		IngestIMAPOAuthClientIDMicrosoft: env("INGEST_IMAP_OAUTH_CLIENT_ID_MICROSOFT", ""),
		IngestIMAPOAuthScopesMicrosoft:   env("INGEST_IMAP_OAUTH_SCOPES_MICROSOFT", ""),
		DevMode:                          env("SUCHI_DEV", "") == "1",
		DevAllowLAN:                      env("SUCHI_DEV_ALLOW_LAN", "") == "1",
		IngestFSDir:                      env("INGEST_FS_DIR", ""),
		IngestFSOwnerEmail:               env("INGEST_FS_OWNER_EMAIL", ""),
		IngestFSSystem:                   env("INGEST_FS_SYSTEM", ""),
		LLMEndpointURL:                   env("LLM_ENDPOINT_URL", ""),
		LLMModel:                         env("LLM_MODEL", ""),
		LLMEgressAck:                     env("LLM_EGRESS_ACK", "") == "true",
		OCRHTTPURL:                       env("OCR_HTTP_URL", ""),
		ExtractEgressAck:                 env("EXTRACT_EGRESS_ACK", "") == "true",
	}
	if c.IngestFSDir == "" && c.IngestFSOwnerEmail != "" {
		c.IngestFSDir = filepath.Join(c.DataDir, "staging")
	}

	switch value := strings.ToLower(env("INGEST_FS_RECURSIVE", "on")); value {
	case "on":
		c.IngestFSRecursive = true
	case "off":
		c.IngestFSRecursive = false
	default:
		return nil, fmt.Errorf("INGEST_FS_RECURSIVE: unknown value %q (want on|off)", value)
	}

	var err error
	// BODY_LIMIT applies to every HTTP request body and watched file. 500M by
	// default because scanned PDFs routinely exceed the old 100M ceiling. Zero
	// disables the cap.
	if c.BodyLimit, err = parseBytes(env("BODY_LIMIT", "500M")); err != nil {
		return nil, fmt.Errorf("BODY_LIMIT: %w", err)
	}
	if c.BackupKeep, err = parseIntBounded("BACKUP_KEEP", env("BACKUP_KEEP", "7"), 0, 10_000); err != nil {
		return nil, err
	}
	if c.BackupInterval, err = time.ParseDuration(env("BACKUP_INTERVAL", "24h")); err != nil {
		return nil, fmt.Errorf("BACKUP_INTERVAL: %w", err)
	}
	if c.BackupInterval < 0 {
		return nil, errors.New("BACKUP_INTERVAL: must be zero or positive")
	}
	if c.AuditRetentionDays, err = parseIntBounded("AUDIT_RETENTION_DAYS",
		env("AUDIT_RETENTION_DAYS", "20"), 0, 100); err != nil {
		return nil, err
	}
	if c.TrustedProxyCIDRs, err = parseCIDRs(env("TRUSTED_PROXY_CIDRS", "")); err != nil {
		return nil, fmt.Errorf("TRUSTED_PROXY_CIDRS: %w", err)
	}
	if c.PdfMaxContentBytes, err = parseBytes(env("PDF_MAX_CONTENT_BYTES", "8M")); err != nil {
		return nil, fmt.Errorf("PDF_MAX_CONTENT_BYTES: %w", err)
	}
	if c.AnyDocMaxContentBytes, err = parseBytes(env("ANYDOC_MAX_CONTENT_BYTES", "32M")); err != nil {
		return nil, fmt.Errorf("ANYDOC_MAX_CONTENT_BYTES: %w", err)
	}
	if c.DjvuMaxContentBytes, err = parseBytes(env("DJVU_MAX_CONTENT_BYTES", "32M")); err != nil {
		return nil, fmt.Errorf("DJVU_MAX_CONTENT_BYTES: %w", err)
	}
	c.DeviceOCRMinConfidence = 0.65
	if s := env("DEVICE_OCR_MIN_CONFIDENCE", ""); s != "" {
		f, err := strconv.ParseFloat(s, 64)
		if err != nil || math.IsNaN(f) || math.IsInf(f, 0) || f < 0 || f > 1 {
			return nil, fmt.Errorf("DEVICE_OCR_MIN_CONFIDENCE: want finite float in [0,1], got %q", s)
		}
		c.DeviceOCRMinConfidence = f
	}

	c.OCREngines, err = parseOCREngines(env("OCR_ENGINE", "auto"))
	if err != nil {
		return nil, err
	}
	c.OCRHTTPTimeout = 10 * time.Minute
	if slices.Contains(c.OCREngines, "http") {
		if c.OCRHTTPTimeout, err = time.ParseDuration(env("SUCHI_OCR_HTTP_TIMEOUT", "10m")); err != nil {
			return nil, fmt.Errorf("SUCHI_OCR_HTTP_TIMEOUT: %w", err)
		}
		if c.OCRHTTPTimeout <= 0 {
			return nil, errors.New("SUCHI_OCR_HTTP_TIMEOUT: must be positive")
		}
	}

	switch value := strings.ToLower(env("SCAN_BLANK_REMOVAL", "auto")); value {
	case "auto":
		c.ScanBlankRemoval = true
	case "off":
		c.ScanBlankRemoval = false
	default:
		return nil, fmt.Errorf("SCAN_BLANK_REMOVAL: unknown value %q (want auto|off)", value)
	}
	c.ScanBlankWhitenessThreshold = 0.995
	if s := env("SCAN_BLANK_WHITENESS_THRESHOLD", ""); s != "" {
		f, err := strconv.ParseFloat(s, 64)
		if err != nil || f <= 0 || f > 1 {
			return nil, fmt.Errorf("SCAN_BLANK_WHITENESS_THRESHOLD: want float in (0,1], got %q", s)
		}
		c.ScanBlankWhitenessThreshold = f
	}

	switch value := strings.ToLower(env("SCAN_SPLIT_ENABLED", "off")); value {
	case "on":
		c.ScanSplitEnabled = true
	case "off":
		c.ScanSplitEnabled = false
	default:
		return nil, fmt.Errorf("SCAN_SPLIT_ENABLED: unknown value %q (want on|off)", value)
	}
	c.ScanSplitToken = env("SCAN_SPLIT_TOKEN", "SUCHI-SPLIT")
	c.ScanSplitDPI = 150
	if s := env("SCAN_SPLIT_DPI", ""); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n < 72 || n > 600 {
			return nil, fmt.Errorf("SCAN_SPLIT_DPI: want integer in [72,600], got %q", s)
		}
		c.ScanSplitDPI = n
	}

	c.ScanASNMode = strings.ToLower(env("SCAN_ASN_MODE", "off"))
	switch c.ScanASNMode {
	case "off", "assign", "split":
	default:
		return nil, fmt.Errorf("SCAN_ASN_MODE: unknown value %q (want off|assign|split)", c.ScanASNMode)
	}
	c.ScanASNPrefix = env("SCAN_ASN_PREFIX", "ASN")
	if c.ScanASNMode != "off" {
		if err := asn.ValidatePrefix(c.ScanASNPrefix); err != nil {
			return nil, fmt.Errorf("SCAN_ASN_PREFIX: want case-sensitive ASCII [A-Za-z][A-Za-z0-9._:-]*, got %q", c.ScanASNPrefix)
		}
		if c.ScanSplitEnabled {
			effectiveToken := strings.TrimSpace(c.ScanSplitToken)
			if effectiveToken == "" {
				effectiveToken = "SUCHI-SPLIT"
			}
			if strings.HasPrefix(effectiveToken, c.ScanASNPrefix) {
				return nil, fmt.Errorf("SCAN_SPLIT_TOKEN: effective token %q starts with SCAN_ASN_PREFIX %q", effectiveToken, c.ScanASNPrefix)
			}
		}
	}

	c.LLMConfidenceThreshold = 0.7
	if s := env("LLM_CONFIDENCE_THRESHOLD", ""); s != "" {
		f, err := strconv.ParseFloat(s, 64)
		if err != nil || f < 0.5 || f > 0.95 {
			return nil, fmt.Errorf("LLM_CONFIDENCE_THRESHOLD: want float in [0.50,0.95], got %q", s)
		}
		c.LLMConfidenceThreshold = f
	}

	c.IngestPasswordsFile = env("INGEST_PASSWORDS_FILE", "")
	c.DecryptKeyPath = env("DECRYPT_KEY_FILE", filepath.Join(c.DataDir, ".decrypt-key"))
	c.PreConsumeScript = env("PRE_CONSUME_SCRIPT", "")

	c.UIDisabled = env("SUCHI_UI_DISABLED", "") == "true" ||
		env("SUCHI_UI_DISABLED", "") == "1"

	c.DemoMode = env("SUCHI_DEMO_MODE", "") == "true" ||
		env("SUCHI_DEMO_MODE", "") == "1"
	if c.DemoGlobalRPS, err = parseIntBounded("SUCHI_DEMO_GLOBAL_RPS",
		env("SUCHI_DEMO_GLOBAL_RPS", "5"), 0, 10_000); err != nil {
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
	if c.LLMAPIKey, err = readSecret("LLM_API_KEY"); err != nil {
		return nil, err
	}
	if slices.Contains(c.OCREngines, "http") {
		if c.OCRHTTPAPIKey, err = readSecret("OCR_HTTP_API_KEY"); err != nil {
			return nil, err
		}
		if err := validateOCRHTTPConfig(c); err != nil {
			return nil, err
		}
	}

	if c.PublicURL == "" {
		return nil, errors.New("PUBLIC_URL is required")
	}
	if err := validateOriginURL("PUBLIC_URL", c.PublicURL); err != nil {
		return nil, err
	}
	if strings.TrimSpace(c.ListenAddr) == "" {
		return nil, errors.New("LISTEN_ADDR must not be empty")
	}
	shareURLSet := strings.TrimSpace(c.SharePublicURL) != ""
	shareListenSet := strings.TrimSpace(c.ShareListenAddr) != ""
	if shareURLSet != shareListenSet {
		return nil, errors.New("SHARE_PUBLIC_URL and SHARE_LISTEN_ADDR must both be set or both unset")
	}
	if shareURLSet {
		if err := validateOriginURL("SHARE_PUBLIC_URL", c.SharePublicURL); err != nil {
			return nil, err
		}
		shareURL, _ := url.Parse(c.SharePublicURL)
		if !c.DevMode && !strings.EqualFold(shareURL.Scheme, "https") {
			return nil, errors.New("SHARE_PUBLIC_URL must use https outside development mode")
		}
		if c.DevMode {
			if err := validateDevShareListenAddr(c.ShareListenAddr); err != nil {
				return nil, err
			}
		}
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

func parseOCREngines(raw string) ([]string, error) {
	parts := strings.Split(raw, ",")
	engines := make([]string, 0, len(parts))
	seen := make(map[string]bool, len(parts))
	for _, part := range parts {
		engine := strings.ToLower(strings.TrimSpace(part))
		if engine == "" {
			return nil, errors.New("OCR_ENGINE: empty engine name")
		}
		switch engine {
		case "auto", "tesseract", "ocrmypdf", "http":
		default:
			return nil, fmt.Errorf("OCR_ENGINE: unknown value %q (want auto|tesseract|ocrmypdf|http)", engine)
		}
		if seen[engine] {
			return nil, fmt.Errorf("OCR_ENGINE: duplicate engine %q", engine)
		}
		seen[engine] = true
		engines = append(engines, engine)
	}
	if seen["auto"] && len(engines) != 1 {
		return nil, errors.New("OCR_ENGINE: auto must be the only engine")
	}
	return engines, nil
}

func validateOCRHTTPConfig(c *Config) error {
	raw := strings.TrimSpace(c.OCRHTTPURL)
	if raw == "" {
		return errors.New("OCR_HTTP_URL is required when OCR_ENGINE includes http")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("OCR_HTTP_URL: %w", err)
	}
	scheme := strings.ToLower(u.Scheme)
	if (scheme != "http" && scheme != "https") || u.Hostname() == "" || u.Opaque != "" {
		return errors.New("OCR_HTTP_URL: must be an absolute http or https URL with a host")
	}
	if u.User != nil {
		return errors.New("OCR_HTTP_URL: userinfo is not allowed")
	}
	if u.RawQuery != "" || u.ForceQuery {
		return errors.New("OCR_HTTP_URL: query is not allowed")
	}
	if strings.Contains(raw, "#") {
		return errors.New("OCR_HTTP_URL: fragment is not allowed")
	}
	local := netutil.IsLocalHost(u.Hostname())
	if !local && scheme != "https" {
		return errors.New("OCR_HTTP_URL: non-local endpoint must use https")
	}
	if !local && !c.ExtractEgressAck {
		return errors.New("OCR_HTTP_URL: non-local endpoint requires EXTRACT_EGRESS_ACK=true")
	}
	c.OCRHTTPURL = u.String()
	return nil
}

func validateOriginURL(name, raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	scheme := strings.ToLower(u.Scheme)
	if (scheme != "http" && scheme != "https") || u.Hostname() == "" || u.Opaque != "" {
		return fmt.Errorf("%s: must be an absolute http or https URL with a host", name)
	}
	if u.User != nil {
		return fmt.Errorf("%s: userinfo is not allowed", name)
	}
	if u.RawQuery != "" || u.ForceQuery {
		return fmt.Errorf("%s: query is not allowed", name)
	}
	if strings.Contains(raw, "#") {
		return fmt.Errorf("%s: fragment is not allowed", name)
	}
	if u.EscapedPath() != "" && u.EscapedPath() != "/" {
		return fmt.Errorf("%s: path is not supported; serve Suchi at the origin root", name)
	}
	return nil
}

func validateDevShareListenAddr(raw string) error {
	host, _, err := net.SplitHostPort(strings.TrimSpace(raw))
	if err != nil {
		return fmt.Errorf("SHARE_LISTEN_ADDR: %w", err)
	}
	if strings.EqualFold(host, "localhost") {
		return nil
	}
	addr, err := netip.ParseAddr(host)
	if err != nil || !addr.IsLoopback() {
		return errors.New("SHARE_LISTEN_ADDR must bind a loopback address in development mode")
	}
	return nil
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

func parseCIDRs(raw string) ([]netip.Prefix, error) {
	values := splitCSV(raw)
	prefixes := make([]netip.Prefix, 0, len(values))
	for _, value := range values {
		prefix, err := netip.ParsePrefix(value)
		if err != nil {
			return nil, fmt.Errorf("invalid CIDR %q", value)
		}
		prefixes = append(prefixes, prefix.Masked())
	}
	return prefixes, nil
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
	if n < 0 {
		return 0, errors.New("must be zero or positive")
	}
	if n > (1<<63-1)/mult {
		return 0, errors.New("value overflows int64 bytes")
	}
	return n * mult, nil
}
