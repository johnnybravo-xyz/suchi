// Config-file loader. Optional overlay above the env-var reader.
//
// Layered precedence (last wins):
//   1. config file (TOML or HUML)
//   2. process env vars (existing config.Load())
//   3. future: CLI flags
//
// **TOML is the recommended default format.** HUML is the documented
// alternative. JSON and YAML remain compatibility parsers for existing
// operator files.
//
// Format is detected by file extension:
//
//	.toml            → TOML (recommended)
//	.huml            → HUML — https://huml.io — human-readable, TOML-adjacent
//	.json            → JSON compatibility
//	.yaml / .yml     → YAML compatibility
//
// File shape mirrors env var names in lower_snake: an operator who
// knows PUBLIC_URL knows public_url. Nested tables/objects are
// flattened with `_` — [oidc]/issuer becomes OIDC_ISSUER. Arrays are
// CSV-joined to match how existing multi-value env vars are shaped
// (OCR_LANGUAGES=eng,deu).
//
// LoadFile() reads the first file it finds from the search path and
// calls os.Setenv() for every key that isn't ALREADY set in the
// process env. Then normal config.Load() runs and picks values
// transparently from env — no duplicate parsing logic per field.

package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"
	huml "github.com/huml-lang/go-huml"
	"gopkg.in/yaml.v3"
)

// FileConfigEnv is the operator-provided explicit path. Set
// SUCHI_CONFIG=/etc/suchi/prod.toml on the command line and this
// bypasses the search order entirely.
const FileConfigEnv = "SUCHI_CONFIG"

// LoadFile reads the first config file it finds and exports each key
// as an env var (unless that var is already set in the process env).
// Returns the path it loaded from, or "" when no file exists. Errors
// only for actual parse/read failures — a missing file is not an
// error.
//
// Call this BEFORE config.Load() in main.go. Calling it multiple
// times is safe: env vars from an earlier call are already set, so
// subsequent calls no-op naturally.
func LoadFile() (string, error) {
	path := findConfigFile()
	if path == "" {
		return "", nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read %s: %w", path, err)
	}
	doc, err := parseByExt(path, raw)
	if err != nil {
		return path, err
	}
	applyFileConfig(doc, "")
	return path, nil
}

// parseByExt picks the parser from the file extension. Anything not
// recognized is a hard error — an operator writing `config.tml`
// probably means TOML but we don't want to guess.
func parseByExt(path string, raw []byte) (map[string]any, error) {
	ext := strings.ToLower(filepath.Ext(path))
	switch ext {
	case ".toml":
		var doc map[string]any
		if _, err := toml.Decode(string(raw), &doc); err != nil {
			return nil, fmt.Errorf("parse TOML %s: %w", path, err)
		}
		return doc, nil
	case ".huml":
		var doc map[string]any
		if err := huml.Unmarshal(raw, &doc); err != nil {
			return nil, fmt.Errorf("parse HUML %s: %w", path, err)
		}
		return doc, nil
	case ".yaml", ".yml":
		var doc map[string]any
		if err := yaml.Unmarshal(raw, &doc); err != nil {
			return nil, fmt.Errorf("parse YAML %s: %w", path, err)
		}
		return doc, nil
	case ".json":
		var doc map[string]any
		if err := json.Unmarshal(raw, &doc); err != nil {
			return nil, fmt.Errorf("parse JSON %s: %w", path, err)
		}
		return doc, nil
	}
	return nil, fmt.Errorf(
		"config file %s: unknown extension %q — use .toml or .huml",
		path, ext)
}

// applyFileConfig walks the parsed TOML tree and calls os.Setenv for
// every leaf. Nested tables get their key path uppercased and joined
// with `_`, so:
//
//	[oidc]
//	issuer = "…"
//
// becomes OIDC_ISSUER. Sequences (arrays) are joined by comma so a
// TOML `["a","b","c"]` matches the CSV convention suchi's existing
// env parsers already use.
func applyFileConfig(m map[string]any, prefix string) {
	for k, v := range m {
		envKey := configKeyToEnv(k)
		if envKey == "" {
			continue
		}
		full := envKey
		if prefix != "" {
			full = prefix + "_" + envKey
		}
		switch x := v.(type) {
		case map[string]any:
			applyFileConfig(x, full)
			continue
		}
		if _, present := os.LookupEnv(full); present {
			continue // env wins over file
		}
		os.Setenv(full, valueToEnv(v))
	}
}

// findConfigFile walks the search paths and returns the first
// readable file. Order (first-found wins):
//
//  1. $SUCHI_CONFIG (explicit override)
//  2. $XDG_CONFIG_HOME/suchi/config.toml
//  3. $HOME/.config/suchi/config.toml
//  4. /etc/suchi/config.toml
//  5. ./suchi.toml
func findConfigFile() string {
	if p := strings.TrimSpace(os.Getenv(FileConfigEnv)); p != "" {
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
			return p
		}
		return ""
	}
	// Keep compatibility formats last so an old file cannot shadow a
	// documented TOML or HUML config in the same directory.
	exts := []string{".toml", ".huml", ".json", ".yaml", ".yml"}
	dirs := []string{}
	if xdg := strings.TrimSpace(os.Getenv("XDG_CONFIG_HOME")); xdg != "" {
		dirs = append(dirs, filepath.Join(xdg, "suchi"))
	}
	if home := strings.TrimSpace(os.Getenv("HOME")); home != "" {
		dirs = append(dirs, filepath.Join(home, ".config", "suchi"))
	}
	dirs = append(dirs, "/etc/suchi", ".")

	for _, dir := range dirs {
		base := "config"
		if dir == "." {
			base = "suchi" // ./suchi.toml, not ./config.toml
		}
		for _, ext := range exts {
			p := filepath.Join(dir, base+ext)
			if fileExists(p) {
				return p
			}
		}
	}
	return ""
}

func fileExists(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && !fi.IsDir()
}

// configKeyToEnv normalises a config key into its env-var counterpart.
// Convention:  public_url  → PUBLIC_URL
//
//	cert_file   → CERT_FILE
//
// Keys with characters outside [a-zA-Z0-9_] are rejected so a stray
// dot or hyphen cannot create an env var no shell can address.
func configKeyToEnv(k string) string {
	k = strings.TrimSpace(k)
	if k == "" {
		return ""
	}
	for i := 0; i < len(k); i++ {
		c := k[i]
		if !((c >= 'a' && c <= 'z') ||
			(c >= 'A' && c <= 'Z') ||
			(c >= '0' && c <= '9') ||
			c == '_') {
			return ""
		}
	}
	return strings.ToUpper(k)
}

// valueToEnv converts a decoded TOML scalar (or slice) into the
// string form the env parser expects. Non-scalar types fall through
// to fmt.Sprintf as a safety net.
func valueToEnv(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case bool:
		if x {
			return "true"
		}
		return "false"
	case int:
		return fmt.Sprintf("%d", x)
	case int64:
		return fmt.Sprintf("%d", x)
	case float64:
		return fmt.Sprintf("%v", x)
	case []any:
		// TOML array → CSV. Matches how OCR_LANGUAGES and similar
		// existing env vars are already shaped.
		parts := make([]string, 0, len(x))
		for _, item := range x {
			parts = append(parts, valueToEnv(item))
		}
		return strings.Join(parts, ",")
	default:
		return fmt.Sprintf("%v", v)
	}
}
