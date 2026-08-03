package paperless

import (
	_ "embed"
	"errors"
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

//go:embed heuristics.yaml
var heuristicsYAML []byte

// AutoMapping returns the built-in heuristic ruleset used when the
// operator passes --auto-jd and hasn't supplied their own --map-jd.
// Bias: high precision over recall. Missing a category lands the doc
// in inbox; mis-filing hides it.
func AutoMapping() (*Mapping, error) {
	var m Mapping
	if err := yaml.Unmarshal(heuristicsYAML, &m); err != nil {
		return nil, fmt.Errorf("parse embedded heuristics: %w", err)
	}
	if err := validateRules(m.Rules); err != nil {
		return nil, fmt.Errorf("embedded heuristics: %w", err)
	}
	return &m, nil
}

// Mapping is a first-match ruleset from Paperless metadata → JD code.
//
// YAML shape (documented in docs/import.md when that lands):
//
//	rules:
//	  - if: tag:tax
//	    category: 22
//	  - if: storage_path:Insurance
//	    category: 23
//	  - if: document_type:Bill
//	    category: 31
//	  - if: correspondent:Landlord
//	    category: 32
//
// "if" is `<kind>:<name>`. Supported kinds: tag, storage_path,
// document_type, correspondent. Comparison is case-insensitive on the
// value.
//
// category is the JD code (integer). Must resolve at import time to a
// row in jd_categories.code. Unmapped docs fall through to the inbox.
type Mapping struct {
	Rules []Rule `yaml:"rules"`
}

// Rule is one line of the ruleset.
type Rule struct {
	If       string `yaml:"if"`
	Category int    `yaml:"category"`
}

// LoadMapping reads a YAML file into a Mapping and validates its shape.
// Empty path returns a nil *Mapping — the caller treats that as
// "no mapping" and falls through to the inbox for every doc.
func LoadMapping(path string) (*Mapping, error) {
	if path == "" {
		return nil, nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read mapping: %w", err)
	}
	var m Mapping
	if err := yaml.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("parse mapping: %w", err)
	}
	if err := validateRules(m.Rules); err != nil {
		return nil, err
	}
	return &m, nil
}

func validateRules(rules []Rule) error {
	if len(rules) == 0 {
		return errors.New("no rules")
	}
	for i, r := range rules {
		if r.If == "" {
			return fmt.Errorf("rule %d: missing 'if'", i)
		}
		kind, value, ok := strings.Cut(r.If, ":")
		if !ok || value == "" {
			return fmt.Errorf("rule %d: 'if' must be '<kind>:<value>' (got %q)", i, r.If)
		}
		switch kind {
		case "tag", "storage_path", "document_type", "correspondent":
		default:
			return fmt.Errorf("rule %d: unknown kind %q (want tag|storage_path|document_type|correspondent)", i, kind)
		}
		if r.Category < 10 || r.Category > 99 {
			return fmt.Errorf("rule %d: category code %d not in 10..99", i, r.Category)
		}
	}
	return nil
}

// nameSets are the Paperless PK → lowercased name maps the resolver
// needs. Assembled once at the start of Run so per-doc resolve is O(rules).
type nameSets struct {
	tags           map[int64]string
	correspondents map[int64]string
	documentTypes  map[int64]string
	storagePaths   map[int64]string
}

// Resolve returns the JD code chosen by the first matching rule, or 0
// if none matched.
func (m *Mapping) Resolve(doc DocumentFields, ns nameSets) int {
	if m == nil {
		return 0
	}
	for _, r := range m.Rules {
		kind, value, _ := strings.Cut(r.If, ":")
		want := strings.ToLower(value)
		switch kind {
		case "tag":
			for _, tagPK := range doc.Tags {
				if strings.ToLower(ns.tags[tagPK]) == want {
					return r.Category
				}
			}
		case "correspondent":
			if doc.Correspondent != nil && strings.ToLower(ns.correspondents[*doc.Correspondent]) == want {
				return r.Category
			}
		case "document_type":
			if doc.DocumentType != nil && strings.ToLower(ns.documentTypes[*doc.DocumentType]) == want {
				return r.Category
			}
		case "storage_path":
			if doc.StoragePath != nil && strings.ToLower(ns.storagePaths[*doc.StoragePath]) == want {
				return r.Category
			}
		}
	}
	return 0
}
