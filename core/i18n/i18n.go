// Package i18n is the UI-string message catalog.
//
// Every user-facing string in a template goes through {{msg "key"}}.
// The catalog is a flat map[string]string loaded from an embedded
// strings/*.yaml file. Missing keys log a warning and return "!key!"
// so gaps show up in the UI instead of silently rendering blank —
// exactly the opposite behavior of most i18n libs, on purpose.
//
// Phase-1 ships English only. Adding a locale is a data problem: drop
// strings/fr.yaml under this package and pick it at Locale-load time.
// No template refactor.
package i18n

import (
	"embed"
	"errors"
	"fmt"
	"html/template"
	"log/slog"

	"gopkg.in/yaml.v3"
)

//go:embed strings/*.yaml
var catalogFS embed.FS

// Catalog is one language's string table.
type Catalog struct {
	Locale string
	Strings map[string]string
	log    *slog.Logger
}

// Load reads strings/<locale>.yaml from the embedded FS.
func Load(locale string, log *slog.Logger) (*Catalog, error) {
	path := fmt.Sprintf("strings/%s.yaml", locale)
	b, err := catalogFS.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read catalog %s: %w", path, err)
	}
	var m map[string]string
	if err := yaml.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("parse catalog %s: %w", path, err)
	}
	if len(m) == 0 {
		return nil, errors.New("empty catalog")
	}
	return &Catalog{Locale: locale, Strings: m, log: log.With("component", "i18n", "locale", locale)}, nil
}

// Msg looks up a key. Missing keys log at Warn once and return "!key!"
// so operators can spot missing strings in the UI directly.
func (c *Catalog) Msg(key string) string {
	if v, ok := c.Strings[key]; ok {
		return v
	}
	c.log.Warn("i18n.miss", "key", key)
	return "!" + key + "!"
}

// FuncMap returns a template.FuncMap that exposes `msg` and `msgf` to
// html/template. `msgf` is msg + fmt.Sprintf so templates never have to
// call fmt themselves.
func (c *Catalog) FuncMap() template.FuncMap {
	return template.FuncMap{
		"msg": c.Msg,
		"msgf": func(key string, args ...any) string {
			return fmt.Sprintf(c.Msg(key), args...)
		},
	}
}
