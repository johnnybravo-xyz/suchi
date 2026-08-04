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
	OCRLanguages   []string

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
	IngestIMAPURL      string
	IngestIMAPPassword string

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
}

// Load reads env vars and returns a validated Config. It is intended to be
// called exactly once at process start.
func Load() (*Config, error) {
	c := &Config{
		PublicURL:          env("PUBLIC_URL", ""),
		DataDir:            env("DATA_DIR", "/data"),
		ListenAddr:         env("LISTEN_ADDR", ":8000"),
		LogLevel:           env("LOG_LEVEL", "info"),
		OCRLanguages:       splitCSV(env("OCR_LANGUAGES", "eng")),
		OIDCIssuerURL:      env("OIDC_ISSUER_URL", ""),
		OIDCClientID:       env("OIDC_CLIENT_ID", ""),
		AdminEmail:         env("ADMIN_EMAIL", ""),
		TLSCertFile:        env("TLS_CERT_FILE", ""),
		TLSKeyFile:         env("TLS_KEY_FILE", ""),
		IngestIMAPURL:      env("INGEST_IMAP_URL", ""),
		IngestFSDir:        env("INGEST_FS_DIR", ""),
		IngestFSOwnerEmail: env("INGEST_FS_OWNER_EMAIL", ""),
		LLMEndpointURL:     env("LLM_ENDPOINT_URL", ""),
		LLMModel:           env("LLM_MODEL", ""),
		LLMEgressAck:       env("LLM_EGRESS_ACK", "") == "true",
	}
	if c.IngestFSDir == "" && c.IngestFSOwnerEmail != "" {
		c.IngestFSDir = filepath.Join(c.DataDir, "staging")
	}

	var err error
	if c.BodyLimit, err = parseBytes(env("BODY_LIMIT", "100M")); err != nil {
		return nil, fmt.Errorf("BODY_LIMIT: %w", err)
	}
	if c.BackupInterval, err = time.ParseDuration(env("BACKUP_INTERVAL", "24h")); err != nil {
		return nil, fmt.Errorf("BACKUP_INTERVAL: %w", err)
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
