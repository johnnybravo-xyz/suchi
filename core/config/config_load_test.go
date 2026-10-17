// SPDX-License-Identifier: AGPL-3.0-or-later

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
func TestShareListenerConfigValidation(t *testing.T) {
	for _, tc := range []struct {
		name      string
		listen    string
		publicURL string
		dev       bool
		wantErr   string
	}{
		{name: "both absent"},
		{name: "listen only", listen: "127.0.0.1:9000", wantErr: "both be set"},
		{name: "URL only", publicURL: "https://share.example.com", wantErr: "both be set"},
		{name: "HTTP production URL", listen: "127.0.0.1:9000", publicURL: "http://share.example.com", wantErr: "must use https"},
		{name: "URL path", listen: "127.0.0.1:9000", publicURL: "https://share.example.com/suchi", wantErr: "SHARE_PUBLIC_URL"},
		{name: "HTTPS production", listen: ":9000", publicURL: "https://share.example.com"},
		{name: "loopback development", listen: "127.0.0.1:9000", publicURL: "http://127.0.0.1:9000", dev: true},
		{name: "IPv6 loopback development", listen: "[::1]:9000", publicURL: "http://[::1]:9000", dev: true},
		{name: "localhost development", listen: "localhost:9000", publicURL: "http://localhost:9000", dev: true},
		{name: "wildcard development", listen: ":9000", publicURL: "http://localhost:9000", dev: true, wantErr: "loopback"},
		{name: "private development", listen: "192.168.1.5:9000", publicURL: "http://192.168.1.5:9000", dev: true, wantErr: "loopback"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isolateConfigEnv(t)
			t.Setenv("PUBLIC_URL", "http://localhost:8000")
			t.Setenv("SHARE_LISTEN_ADDR", tc.listen)
			t.Setenv("SHARE_PUBLIC_URL", tc.publicURL)
			if tc.dev {
				t.Setenv("SUCHI_DEV", "1")
			}
			cfg, err := Load()
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("Load error=%v, want text %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if cfg.ShareListenAddr != tc.listen || cfg.SharePublicURL != tc.publicURL {
				t.Fatalf("share config=%q %q", cfg.ShareListenAddr, cfg.SharePublicURL)
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

func TestDeviceOCRMinConfidence(t *testing.T) {
	isolateConfigEnv(t)
	t.Setenv("PUBLIC_URL", "http://localhost")

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DeviceOCRMinConfidence != 0.65 {
		t.Fatalf("default DeviceOCRMinConfidence = %v, want 0.65", cfg.DeviceOCRMinConfidence)
	}

	t.Setenv("DEVICE_OCR_MIN_CONFIDENCE", "0.8")
	cfg, err = Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DeviceOCRMinConfidence != 0.8 {
		t.Fatalf("configured DeviceOCRMinConfidence = %v, want 0.8", cfg.DeviceOCRMinConfidence)
	}

	for _, invalid := range []string{"-0.1", "1.1", "NaN", "+Inf", "-Inf", "not-a-number"} {
		t.Run(invalid, func(t *testing.T) {
			t.Setenv("DEVICE_OCR_MIN_CONFIDENCE", invalid)
			if _, err := Load(); err == nil {
				t.Fatalf("DEVICE_OCR_MIN_CONFIDENCE=%q was accepted", invalid)
			}
		})
	}
}

func TestIngestFSRecursiveDefaultsOnAndCanBeDisabled(t *testing.T) {
	isolateConfigEnv(t)
	t.Setenv("PUBLIC_URL", "http://localhost")

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.IngestFSRecursive {
		t.Fatal("recursive watched-folder discovery should default on")
	}

	t.Setenv("INGEST_FS_RECURSIVE", "OFF")
	cfg, err = Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.IngestFSRecursive {
		t.Fatal("INGEST_FS_RECURSIVE=off did not disable recursion")
	}

	t.Setenv("INGEST_FS_RECURSIVE", "sometimes")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "INGEST_FS_RECURSIVE") {
		t.Fatalf("invalid recursive setting error = %v", err)
	}
}

func TestDevAllowLANRequiresExplicitOne(t *testing.T) {
	isolateConfigEnv(t)
	t.Setenv("PUBLIC_URL", "http://localhost")
	for _, value := range []string{"", "0", "true"} {
		t.Setenv("SUCHI_DEV_ALLOW_LAN", value)
		cfg, err := Load()
		if err != nil {
			t.Fatal(err)
		}
		if cfg.DevAllowLAN {
			t.Fatalf("SUCHI_DEV_ALLOW_LAN=%q enabled LAN access", value)
		}
	}
	t.Setenv("SUCHI_DEV_ALLOW_LAN", "1")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.DevAllowLAN {
		t.Fatal("SUCHI_DEV_ALLOW_LAN=1 did not enable LAN access")
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

func TestScanASNConfig(t *testing.T) {
	isolateConfigEnv(t)
	t.Setenv("PUBLIC_URL", "http://localhost")

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ScanASNMode != "off" || cfg.ScanASNPrefix != "ASN" {
		t.Fatalf("ASN defaults = mode %q prefix %q", cfg.ScanASNMode, cfg.ScanASNPrefix)
	}

	t.Setenv("SCAN_ASN_MODE", "split")
	t.Setenv("SCAN_ASN_PREFIX", "House-2:")
	cfg, err = Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ScanASNMode != "split" || cfg.ScanASNPrefix != "House-2:" {
		t.Fatalf("ASN config = mode %q prefix %q", cfg.ScanASNMode, cfg.ScanASNPrefix)
	}

	for _, tc := range []struct {
		name   string
		mode   string
		prefix string
	}{
		{name: "unknown mode", mode: "required", prefix: "ASN"},
		{name: "quoted prefix", mode: "assign", prefix: `"ASN"`},
		{name: "whitespace prefix", mode: "split", prefix: "ASN "},
		{name: "digit-leading prefix", mode: "split", prefix: "1ASN"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("SCAN_ASN_MODE", tc.mode)
			t.Setenv("SCAN_ASN_PREFIX", tc.prefix)
			if _, err := Load(); err == nil {
				t.Fatalf("mode %q prefix %q was accepted", tc.mode, tc.prefix)
			}
		})
	}
}

func TestScanASNRejectsSeparatorPrefixCollision(t *testing.T) {
	isolateConfigEnv(t)
	t.Setenv("PUBLIC_URL", "http://localhost")
	t.Setenv("SCAN_ASN_MODE", "split")
	t.Setenv("SCAN_ASN_PREFIX", "ASN")
	t.Setenv("SCAN_SPLIT_ENABLED", "on")
	t.Setenv("SCAN_SPLIT_TOKEN", "ASN-SEPARATOR")

	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "SCAN_SPLIT_TOKEN") {
		t.Fatalf("collision error = %v", err)
	}
}
