// Package i18n loads the server-rendered UI string catalogs.
package i18n

import (
	"embed"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"strings"

	"gopkg.in/yaml.v3"
)

//go:embed strings/*.yaml
var catalogFS embed.FS

const DefaultLocale = "en"

// Catalog is one language's string table.
type Catalog struct {
	Locale   string
	Strings  map[string]string
	fallback map[string]string
	log      *slog.Logger
}

// NormalizeLocale converts browser-style locale tags to catalog names.
func NormalizeLocale(locale string) string {
	locale = strings.TrimSpace(strings.Split(locale, ",")[0])
	locale = strings.ReplaceAll(locale, "_", "-")
	locale = strings.ToLower(strings.TrimSpace(strings.Split(locale, ";")[0]))
	if locale == "" || locale == "*" {
		return DefaultLocale
	}
	return locale
}

// Load resolves an exact locale, then its base language, then English.
func Load(locale string, log *slog.Logger) (*Catalog, error) {
	requested := NormalizeLocale(locale)
	resolved, stringsByKey, err := loadBestCatalog(requested)
	if err != nil {
		return nil, err
	}
	fallback := stringsByKey
	if resolved != DefaultLocale {
		_, fallback, err = loadCatalog(DefaultLocale)
		if err != nil {
			return nil, err
		}
	}
	if log == nil {
		log = slog.Default()
	}
	return &Catalog{
		Locale: resolved, Strings: stringsByKey, fallback: fallback,
		log: log.With("component", "i18n", "locale", resolved),
	}, nil
}

func loadBestCatalog(locale string) (string, map[string]string, error) {
	candidates := []string{locale}
	if base, _, ok := strings.Cut(locale, "-"); ok {
		candidates = append(candidates, base)
	}
	if candidates[len(candidates)-1] != DefaultLocale {
		candidates = append(candidates, DefaultLocale)
	}
	for _, candidate := range candidates {
		resolved, catalog, err := loadCatalog(candidate)
		if err == nil {
			return resolved, catalog, nil
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return "", nil, err
		}
	}
	return "", nil, errors.New("no translation catalogs available")
}

func loadCatalog(locale string) (string, map[string]string, error) {
	path := fmt.Sprintf("strings/%s.yaml", locale)
	b, err := catalogFS.ReadFile(path)
	if err != nil {
		return "", nil, fmt.Errorf("read catalog %s: %w", path, err)
	}
	var m map[string]string
	if err := yaml.Unmarshal(b, &m); err != nil {
		return "", nil, fmt.Errorf("parse catalog %s: %w", path, err)
	}
	if len(m) == 0 {
		return "", nil, errors.New("empty catalog")
	}
	return locale, m, nil
}

// Lookup returns a translated string, falling back to English.
func (c *Catalog) Lookup(key string) (string, bool) {
	if value, ok := c.Strings[key]; ok {
		return value, true
	}
	value, ok := c.fallback[key]
	return value, ok
}

// Msg returns a visible marker for missing keys so catalog gaps are obvious.
func (c *Catalog) Msg(key string) string {
	if v, ok := c.Lookup(key); ok {
		return v
	}
	c.log.Warn("i18n.miss", "key", key)
	return "!" + key + "!"
}

// FuncMap exposes msg and formatted msg lookups to html/template.
func (c *Catalog) FuncMap() template.FuncMap {
	return template.FuncMap{
		"msg": c.Msg,
		"msgf": func(key string, args ...any) string {
			return fmt.Sprintf(c.Msg(key), args...)
		},
	}
}
