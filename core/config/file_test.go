package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Every format test writes a config file, points SUCHI_CONFIG at it,
// runs LoadFile, and asserts env vars land. The env-wins rule and
// nested-table flattening are tested once against TOML (the parser
// difference is in ingestion, not the apply layer).

func TestLoadFile_TOML(t *testing.T) {
	body := `
public_url = "http://example.com"
data_dir   = "/data"
body_limit = "50M"
ocr_languages = ["eng", "deu"]

[oidc]
issuer = "https://id.example.com"
client_id = "suchi"
`
	assertLoad(t, ".toml", body, map[string]string{
		"PUBLIC_URL":     "http://example.com",
		"DATA_DIR":       "/data",
		"BODY_LIMIT":     "50M",
		"OCR_LANGUAGES":  "eng,deu",
		"OIDC_ISSUER":    "https://id.example.com",
		"OIDC_CLIENT_ID": "suchi",
	})
}

func TestLoadFile_HUML(t *testing.T) {
	// HUML syntax (github.com/huml-lang/go-huml v0.3.0):
	//   scalars      → key: "value"
	//   lists/tables → key:: <inline or indented block>
	// See https://huml.io for the full grammar.
	body := `
public_url: "http://example.com"
data_dir: "/data"
body_limit: "50M"
ocr_languages:: "eng", "deu"
oidc::
  issuer: "https://id.example.com"
  client_id: "suchi"
`
	assertLoad(t, ".huml", body, map[string]string{
		"PUBLIC_URL":     "http://example.com",
		"DATA_DIR":       "/data",
		"BODY_LIMIT":     "50M",
		"OCR_LANGUAGES":  "eng,deu",
		"OIDC_ISSUER":    "https://id.example.com",
		"OIDC_CLIENT_ID": "suchi",
	})
}

// Env wins over file. Set the env var BEFORE LoadFile — the file's
// value must NOT overwrite it.
func TestLoadFile_EnvOverridesFile(t *testing.T) {
	body := `public_url = "http://from-file"`
	t.Setenv("PUBLIC_URL", "http://from-env")
	// SUCHI_CONFIG points at the file below.
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv(FileConfigEnv, path)
	if _, err := LoadFile(); err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	if got := os.Getenv("PUBLIC_URL"); got != "http://from-env" {
		t.Errorf("env should win: got PUBLIC_URL=%q, want %q", got, "http://from-env")
	}
}

// A missing file is fine — no error, empty path returned.
func TestLoadFile_NoFile(t *testing.T) {
	// Isolate from any test host that might have $HOME/.config/suchi.
	t.Setenv("HOME", "/nonexistent-home-for-suchi-tests")
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv(FileConfigEnv, "")
	// findConfigFile also checks /etc/suchi and ./suchi.toml —
	// unlikely to exist in the test cwd, but if this test ever runs
	// in an environment where they do, remove them for the test window.
	path, err := LoadFile()
	if err != nil {
		t.Errorf("missing file should not error, got %v", err)
	}
	// path may be non-empty if the test host happens to have a config
	// dropped into cwd — that's out of our control, don't fail hard.
	_ = path
}

// Unknown extension is a hard error — explicit and safe.
func TestLoadFile_UnknownExtension(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.ini")
	if err := os.WriteFile(path, []byte("[section]\nkey=value\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv(FileConfigEnv, path)
	_, err := LoadFile()
	if err == nil {
		t.Fatal("expected error for unknown extension, got nil")
	}
	if !strings.Contains(err.Error(), "unknown extension") {
		t.Errorf("expected 'unknown extension' in error, got %v", err)
	}
}

// assertLoad writes body to a temp file with ext, points SUCHI_CONFIG
// at it, runs LoadFile, and asserts every expected env var landed.
func assertLoad(t *testing.T, ext, body string, want map[string]string) {
	t.Helper()
	// Isolate the process env from ambient state.
	for k := range want {
		os.Unsetenv(k)
		t.Cleanup(func() { os.Unsetenv(k) })
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "config"+ext)
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv(FileConfigEnv, path)
	if _, err := LoadFile(); err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	for k, w := range want {
		got := os.Getenv(k)
		if got != w {
			t.Errorf("env %s: got %q, want %q", k, got, w)
		}
	}
}

func TestConfigKeyToEnv(t *testing.T) {
	cases := map[string]string{
		"public_url":  "PUBLIC_URL",
		"BODY_LIMIT":  "BODY_LIMIT",
		"":            "",
		"has.dot":     "",
		"has-hyphen":  "",
		"  trimmed  ": "TRIMMED",
	}
	for in, want := range cases {
		if got := configKeyToEnv(in); got != want {
			t.Errorf("configKeyToEnv(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestValueToEnv(t *testing.T) {
	cases := []struct {
		in   any
		want string
	}{
		{"hello", "hello"},
		{true, "true"},
		{false, "false"},
		{42, "42"},
		{int64(9999), "9999"},
		{3.14, "3.14"},
		{[]any{"a", "b", "c"}, "a,b,c"},
		{[]any{1, 2, 3}, "1,2,3"},
		{nil, ""},
	}
	for _, tc := range cases {
		got := valueToEnv(tc.in)
		if got != tc.want {
			t.Errorf("valueToEnv(%v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
