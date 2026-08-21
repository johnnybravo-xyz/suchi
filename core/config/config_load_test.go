package config

import (
	"net/netip"
	"os"
	"strings"
	"testing"
)

func TestPublicURLValidation(t *testing.T) {
	for _, raw := range []string{
		"localhost:8000",
		"ftp://example.com",
		"https:///missing-host",
		"https://:443",
		"https://user@example.com",
		"https://example.com?debug=1",
		"https://example.com?",
		"https://example.com#fragment",
		"https://example.com#",
		"https://example.com/archive",
	} {
		t.Run(raw, func(t *testing.T) {
			isolateConfigEnv(t)
			t.Setenv("PUBLIC_URL", raw)
			_, err := Load()
			if err == nil || !strings.Contains(err.Error(), "PUBLIC_URL") {
				t.Fatalf("Load error = %v, want PUBLIC_URL validation error", err)
			}
		})
	}

	for _, raw := range []string{"http://localhost:8000", "https://suchi.example.com/"} {
		t.Run(raw, func(t *testing.T) {
			isolateConfigEnv(t)
			t.Setenv("PUBLIC_URL", raw)
			if _, err := Load(); err != nil {
				t.Fatalf("Load(%q): %v", raw, err)
			}
		})
	}
}

func TestDemoGlobalRPSDefaultAndDisable(t *testing.T) {
	t.Setenv("PUBLIC_URL", "http://localhost")
	for _, key := range []string{
		"OIDC_ISSUER_URL", "OIDC_CLIENT_ID", "OIDC_CLIENT_SECRET",
		"OIDC_CLIENT_SECRET_FILE", "TLS_CERT_FILE", "TLS_KEY_FILE",
	} {
		t.Setenv(key, "")
	}

	old, present := os.LookupEnv("SUCHI_DEMO_GLOBAL_RPS")
	if err := os.Unsetenv("SUCHI_DEMO_GLOBAL_RPS"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if present {
			_ = os.Setenv("SUCHI_DEMO_GLOBAL_RPS", old)
		} else {
			_ = os.Unsetenv("SUCHI_DEMO_GLOBAL_RPS")
		}
	})

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DemoGlobalRPS != 5 {
		t.Fatalf("default DemoGlobalRPS = %d, want 5", cfg.DemoGlobalRPS)
	}

	t.Setenv("SUCHI_DEMO_GLOBAL_RPS", "0")
	cfg, err = Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DemoGlobalRPS != 0 {
		t.Fatalf("disabled DemoGlobalRPS = %d, want 0", cfg.DemoGlobalRPS)
	}
}

func TestTrustedProxyCIDRs(t *testing.T) {
	t.Setenv("PUBLIC_URL", "http://localhost")
	t.Setenv("TRUSTED_PROXY_CIDRS", "10.0.0.7/8, 2001:db8::1/48")
	for _, key := range []string{
		"OIDC_ISSUER_URL", "OIDC_CLIENT_ID", "OIDC_CLIENT_SECRET",
		"OIDC_CLIENT_SECRET_FILE", "TLS_CERT_FILE", "TLS_KEY_FILE",
	} {
		t.Setenv(key, "")
	}

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	want := []netip.Prefix{
		netip.MustParsePrefix("10.0.0.0/8"),
		netip.MustParsePrefix("2001:db8::/48"),
	}
	if len(cfg.TrustedProxyCIDRs) != len(want) {
		t.Fatalf("trusted proxies = %v", cfg.TrustedProxyCIDRs)
	}
	for i := range want {
		if cfg.TrustedProxyCIDRs[i] != want[i] {
			t.Fatalf("trusted proxy %d = %s, want %s", i, cfg.TrustedProxyCIDRs[i], want[i])
		}
	}
}

func TestTrustedProxyCIDRsRejectInvalidValue(t *testing.T) {
	t.Setenv("PUBLIC_URL", "http://localhost")
	t.Setenv("TRUSTED_PROXY_CIDRS", "10.0.0.0/8,not-a-network")
	if _, err := Load(); err == nil {
		t.Fatal("invalid trusted proxy network was accepted")
	}
}

func TestLLMConfidenceThreshold(t *testing.T) {
	t.Setenv("PUBLIC_URL", "http://localhost")
	for _, key := range []string{
		"OIDC_ISSUER_URL", "OIDC_CLIENT_ID", "OIDC_CLIENT_SECRET",
		"OIDC_CLIENT_SECRET_FILE", "TLS_CERT_FILE", "TLS_KEY_FILE",
	} {
		t.Setenv(key, "")
	}

	t.Setenv("LLM_CONFIDENCE_THRESHOLD", "")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.LLMConfidenceThreshold != 0.7 {
		t.Fatalf("default LLMConfidenceThreshold = %v, want 0.7", cfg.LLMConfidenceThreshold)
	}

	t.Setenv("LLM_CONFIDENCE_THRESHOLD", "0.85")
	cfg, err = Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.LLMConfidenceThreshold != 0.85 {
		t.Fatalf("configured LLMConfidenceThreshold = %v, want 0.85", cfg.LLMConfidenceThreshold)
	}

	t.Setenv("LLM_CONFIDENCE_THRESHOLD", "0.49")
	if _, err := Load(); err == nil {
		t.Fatal("out-of-range LLM confidence threshold was accepted")
	}
}

func TestInvalidRuntimeSettings(t *testing.T) {
	for key, value := range map[string]string{
		"BODY_LIMIT":            "-1",
		"PDF_MAX_CONTENT_BYTES": "9223372036854775807G",
		"BACKUP_INTERVAL":       "-1h",
		"LISTEN_ADDR":           "",
	} {
		t.Run(key, func(t *testing.T) {
			isolateConfigEnv(t)
			t.Setenv("PUBLIC_URL", "http://localhost")
			t.Setenv(key, value)
			if _, err := Load(); err == nil {
				t.Fatalf("%s=%q was accepted", key, value)
			}
		})
	}
}
