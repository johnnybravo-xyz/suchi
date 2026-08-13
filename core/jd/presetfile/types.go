// Package presetfile is the parser + validator for suchi-taxonomy/v1
// files: importable taxonomy presets in HuML, TOML, or YAML. See
// project-suchi/suchi-taxonomy-presets-spec.md for the format
// definition.
//
// One internal struct — PresetFile — normalizes across all three
// serializations. Validation runs once against the normalized shape,
// so a HuML preset and its YAML mirror produce identical error
// messages against the same invariants.
//
// The struct field tags carry all three parser namespaces:
//   - `huml:"..."`  → github.com/huml-lang/go-huml
//   - `toml:"..."`  → github.com/BurntSushi/toml
//   - `yaml:"..."`  → gopkg.in/yaml.v3
//   - `json:"..."`  → the API + export path
//
// Symbolic references only: preset files never carry instance row ids.
// jd_category by code, document_type / tag / correspondent by name.
// See spec §2 (Field rules).
package presetfile

// Format is the value the file's `format:` field MUST carry.
const Format = "suchi-taxonomy/v1"

// PresetFile is the normalized shape parsed from HuML / TOML / YAML.
type PresetFile struct {
	Format     string `huml:"format" toml:"format" yaml:"format" json:"format"`
	ID         string `huml:"id" toml:"id" yaml:"id" json:"id"`
	Version    int    `huml:"version" toml:"version" yaml:"version" json:"version"`
	Name       string `huml:"name" toml:"name" yaml:"name" json:"name"`
	Market     string `huml:"market,omitempty" toml:"market,omitempty" yaml:"market,omitempty" json:"market,omitempty"`
	Language   string `huml:"language,omitempty" toml:"language,omitempty" yaml:"language,omitempty" json:"language,omitempty"`
	Maintainer string `huml:"maintainer,omitempty" toml:"maintainer,omitempty" yaml:"maintainer,omitempty" json:"maintainer,omitempty"`
	License    string `huml:"license,omitempty" toml:"license,omitempty" yaml:"license,omitempty" json:"license,omitempty"`
	Flat       bool   `huml:"flat,omitempty" toml:"flat,omitempty" yaml:"flat,omitempty" json:"flat,omitempty"`
	// Inbox is the category code that receives new uploads. Required
	// unless Flat=true (then Validate injects a synthetic 49 under a
	// synthetic System area).
	Inbox int    `huml:"inbox,omitempty" toml:"inbox,omitempty" yaml:"inbox,omitempty" json:"inbox,omitempty"`
	Story string `huml:"story" toml:"story" yaml:"story" json:"story"`
	Areas []Area `huml:"areas,omitempty" toml:"areas,omitempty" yaml:"areas,omitempty" json:"areas,omitempty"`
	// Categories is the flat-mode input surface: when Flat=true, the
	// file carries a single `categories:` list instead of areas, and
	// the parser synthesizes area 10 (named after the preset) around
	// it plus a System area with a 49 inbox. In structured mode this
	// stays empty and Areas is authoritative.
	Categories []Category `huml:"categories,omitempty" toml:"categories,omitempty" yaml:"categories,omitempty" json:"categories,omitempty"`
	Seeds      *Seeds     `huml:"seeds,omitempty" toml:"seeds,omitempty" yaml:"seeds,omitempty" json:"seeds,omitempty"`
}

// Area — one JD area (decade). Code is the decade start (10..90).
type Area struct {
	Code       int        `huml:"code" toml:"code" yaml:"code" json:"code"`
	Name       string     `huml:"name" toml:"name" yaml:"name" json:"name"`
	Categories []Category `huml:"categories" toml:"categories" yaml:"categories" json:"categories"`
}

// Category — one JD category (two-digit code inside an area's decade).
// Keywords are plain substrings; the seeder materializes each one as
// a content_contains → set_jd_category rule.
type Category struct {
	Code            int      `huml:"code" toml:"code" yaml:"code" json:"code"`
	Name            string   `huml:"name" toml:"name" yaml:"name" json:"name"`
	Description     string   `huml:"description,omitempty" toml:"description,omitempty" yaml:"description,omitempty" json:"description,omitempty"`
	Sensitivity     string   `huml:"sensitivity,omitempty" toml:"sensitivity,omitempty" yaml:"sensitivity,omitempty" json:"sensitivity,omitempty"`
	Keywords        []string `huml:"keywords,omitempty" toml:"keywords,omitempty" yaml:"keywords,omitempty" json:"keywords,omitempty"`
	ReviewAfterDays int      `huml:"review_after_days,omitempty" toml:"review_after_days,omitempty" yaml:"review_after_days,omitempty" json:"review_after_days,omitempty"`
}

// Seeds — optional seed rows applied at import time. All entries are
// symbolic (name / code); resolution to instance ids happens in
// core/jd/importer.
type Seeds struct {
	Tags           []string         `huml:"tags,omitempty" toml:"tags,omitempty" yaml:"tags,omitempty" json:"tags,omitempty"`
	DocumentTypes  []string         `huml:"document_types,omitempty" toml:"document_types,omitempty" yaml:"document_types,omitempty" json:"document_types,omitempty"`
	Correspondents []string         `huml:"correspondents,omitempty" toml:"correspondents,omitempty" yaml:"correspondents,omitempty" json:"correspondents,omitempty"`
	Automations    []SeedAutomation `huml:"automations,omitempty" toml:"automations,omitempty" yaml:"automations,omitempty" json:"automations,omitempty"`
}

// SeedAutomation — one row in `seeds.automations`. Matches the wire
// shape of /api/automations/ closely, with symbolic references in
// place of instance ids.
type SeedAutomation struct {
	Name    string   `huml:"name" toml:"name" yaml:"name" json:"name"`
	Trigger Trigger  `huml:"trigger" toml:"trigger" yaml:"trigger" json:"trigger"`
	Actions []Action `huml:"actions" toml:"actions" yaml:"actions" json:"actions"`
}

// Trigger — matches core/automations.Trigger's shape, integer type
// code per spec §2. filter_mailrule_id is deliberately omitted:
// mail-rule ids are instance-specific and can't cross a preset
// boundary.
type Trigger struct {
	// Type is 1=consumption, 2=document_added, 3=document_updated.
	Type                  int    `huml:"type" toml:"type" yaml:"type" json:"type"`
	FilterPath            string `huml:"filter_path,omitempty" toml:"filter_path,omitempty" yaml:"filter_path,omitempty" json:"filter_path,omitempty"`
	FilterFilename        string `huml:"filter_filename,omitempty" toml:"filter_filename,omitempty" yaml:"filter_filename,omitempty" json:"filter_filename,omitempty"`
	FilterContentMatching string `huml:"filter_content_matching,omitempty" toml:"filter_content_matching,omitempty" yaml:"filter_content_matching,omitempty" json:"filter_content_matching,omitempty"`
}

// Action — one automation action. Params carry symbolic refs (a
// `jd_category_code`, `document_type` name, `tag` name, list of `tags`,
// `correspondent` name, etc.). The importer resolves them to instance
// ids at seed time.
type Action struct {
	Kind   string         `huml:"kind" toml:"kind" yaml:"kind" json:"kind"`
	Params map[string]any `huml:"params,omitempty" toml:"params,omitempty" yaml:"params,omitempty" json:"params,omitempty"`
}
