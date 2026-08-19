package config

import (
	"net/netip"
	"os"
	"testing"
)

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
