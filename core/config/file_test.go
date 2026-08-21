package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLoadFileAndLoad(t *testing.T) {
	for _, name := range []string{"config.example.toml", "config.example.huml"} {
		t.Run(name, func(t *testing.T) {
			isolateConfigEnv(t)
			path := filepath.Join("..", "..", "docs", name)
			t.Setenv(FileConfigEnv, path)

			loaded, err := LoadFile()
			if err != nil {
				t.Fatalf("LoadFile: %v", err)
			}
			if loaded != path {
				t.Fatalf("loaded %q, want %q", loaded, path)
			}
			cfg, err := Load()
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if cfg.PublicURL != "http://127.0.0.1:8000" || cfg.BodyLimit != 500<<20 {
				t.Fatalf("representative mapping failed: PublicURL=%q BodyLimit=%d", cfg.PublicURL, cfg.BodyLimit)
			}
			if cfg.BackupInterval != 24*time.Hour || !cfg.ScanBlankRemoval || cfg.ScanBlankWhitenessThreshold != 0.995 {
				t.Fatalf("example values were not loaded: %+v", cfg)
			}
			if cfg.ScanSplitEnabled || cfg.ScanSplitDPI != 150 {
				t.Fatalf("scan split defaults were not loaded: enabled=%v dpi=%d", cfg.ScanSplitEnabled, cfg.ScanSplitDPI)
			}
		})
	}

	t.Run("environment aliases override file aliases", func(t *testing.T) {
		isolateConfigEnv(t)
		dir := t.TempDir()
		path := filepath.Join(dir, "config.toml")
		body := `
public_url = "https://suchi.example.com"
admin_email = "admin@example.com"
body_limit = "64M"

[oidc]
issuer_url = "https://id.example.com"
client_id = "suchi"
client_secret_file = "/missing/file-secret"

[llm]
api_key_file = "/missing/llm-secret"
`
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		t.Setenv(FileConfigEnv, path)
		t.Setenv("BODY_LIMIT", "12M")
		t.Setenv("OIDC_CLIENT_SECRET", "environment-oidc-secret")
		t.Setenv("LLM_API_KEY", "environment-llm-key")

		if _, err := LoadFile(); err != nil {
			t.Fatalf("LoadFile: %v", err)
		}
		cfg, err := Load()
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if cfg.BodyLimit != 12<<20 {
			t.Fatalf("BodyLimit = %d, want %d", cfg.BodyLimit, 12<<20)
		}
		if cfg.OIDCClientSecret != "environment-oidc-secret" || cfg.LLMAPIKey != "environment-llm-key" {
			t.Fatalf("environment secrets did not win: oidc=%q llm=%q", cfg.OIDCClientSecret, cfg.LLMAPIKey)
		}
		if cfg.AdminEmail != "admin@example.com" || cfg.OIDCIssuerURL != "https://id.example.com" {
			t.Fatalf("nested mapping failed: AdminEmail=%q OIDCIssuerURL=%q", cfg.AdminEmail, cfg.OIDCIssuerURL)
		}
	})
}

func TestLoadFileExplicitPathErrors(t *testing.T) {
	dir := t.TempDir()
	unreadable := filepath.Join(dir, "unreadable.toml")
	if err := os.WriteFile(unreadable, []byte("public_url = \"http://localhost\""), 0o000); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name string
		path string
		want string
	}{
		{name: "missing", path: filepath.Join(dir, "missing.toml"), want: "no such file"},
		{name: "directory", path: dir, want: "is a directory"},
		{name: "unreadable", path: unreadable, want: "no read permission"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv(FileConfigEnv, tt.path)
			_, err := LoadFile()
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("LoadFile error = %v, want text %q", err, tt.want)
			}
		})
	}

	t.Run("unknown extension", func(t *testing.T) {
		path := filepath.Join(dir, "config.ini")
		if err := os.WriteFile(path, []byte("key=value\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		t.Setenv(FileConfigEnv, path)
		_, err := LoadFile()
		if err == nil || !strings.Contains(err.Error(), "unknown extension") {
			t.Fatalf("LoadFile error = %v, want unknown extension", err)
		}
	})
}

func isolateConfigEnv(t *testing.T) {
	t.Helper()
	for _, key := range []string{
		FileConfigEnv, "PUBLIC_URL", "DATA_DIR", "LISTEN_ADDR", "LOG_LEVEL", "SUCHI_PPROF",
		"BODY_LIMIT", "BACKUP_INTERVAL", "BACKUP_KEEP", "AUDIT_RETENTION_DAYS",
		"TRUSTED_PROXY_CIDRS", "OCR_LANGUAGES", "OCR_ENGINE", "PDF_MAX_CONTENT_BYTES",
		"ANYDOC_MAX_CONTENT_BYTES", "DJVU_MAX_CONTENT_BYTES", "SCAN_BLANK_REMOVAL",
		"SCAN_BLANK_WHITENESS_THRESHOLD", "SCAN_SPLIT_ENABLED", "SCAN_SPLIT_TOKEN", "SCAN_SPLIT_DPI",
		"OIDC_ISSUER_URL", "OIDC_CLIENT_ID", "OIDC_CLIENT_SECRET", "OIDC_CLIENT_SECRET_FILE", "ADMIN_EMAIL",
		"TLS_CERT_FILE", "TLS_KEY_FILE", "INGEST_FS_DIR", "INGEST_FS_OWNER_EMAIL",
		"INGEST_IMAP_OAUTH_CLIENT_ID_MICROSOFT",
		"INGEST_IMAP_OAUTH_SCOPES_MICROSOFT", "INGEST_PASSWORDS_FILE", "DECRYPT_KEY_FILE",
		"LLM_ENDPOINT_URL", "LLM_MODEL", "LLM_API_KEY", "LLM_API_KEY_FILE", "LLM_EGRESS_ACK",
		"LLM_CONFIDENCE_THRESHOLD", "PRE_CONSUME_SCRIPT", "SUCHI_UI_DISABLED", "SUCHI_DEV",
		"SUCHI_DEMO_MODE", "SUCHI_DEMO_GLOBAL_RPS", "SUCHI_DEMO_SCRATCH_TTL_MINUTES",
	} {
		unsetEnv(t, key)
	}
}

func unsetEnv(t *testing.T, key string) {
	t.Helper()
	old, present := os.LookupEnv(key)
	if err := os.Unsetenv(key); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if present {
			_ = os.Setenv(key, old)
		} else {
			_ = os.Unsetenv(key)
		}
	})
}
