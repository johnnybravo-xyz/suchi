// Package sidecar is the JSON sidecar spec every ingest producer
// (fs-watch, email-ingest, third-party scanners, cron scripts) can
// target.
//
// Contract, from the design doc:
//
//	{
//	  "suchi_sidecar": 1,
//	  "title":          "Electricity bill March 2026",
//	  "correspondent":  "BESCOM",
//	  "tags":           ["utilities", "source:email"],
//	  "created":        "2026-03-02",
//	  "notes":          "auto-fetched",
//	  "jd_category":    31,
//	  "custom_fields":  { "Financial Year": "2025-26" }
//	}
//
// All fields except `suchi_sidecar` are optional. Unknown fields are
// ignored with a warning at Parse time — this file grows accessors
// as producers ask for new metadata.
package sidecar

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// Version is the current sidecar-format version. Producers must set
// suchi_sidecar to this value; anything else fails Parse.
//
// Bump when a required-field change would break existing producers.
// Additive optional fields don't require a bump.
const Version = 1

// V1 is the parsed shape. Zero-value fields mean "producer didn't
// specify" — the ingest caller decides the default.
type V1 struct {
	Version int    `json:"suchi_sidecar"`
	Title   string `json:"title,omitempty"`

	// Correspondent is the primary/sender-role name. Kept singular for
	// backwards compat with pre-Phase-2 producers. When Correspondents
	// is also set, the array wins and this field is treated as
	// redundant with the first sender entry.
	Correspondent string `json:"correspondent,omitempty"`

	// Correspondents is the multi-party form. Each entry pairs a
	// correspondent name with a role (sender|recipient|cc|other).
	// Applied via document_correspondents.
	Correspondents []Correspondent `json:"correspondents,omitempty"`

	Tags         []string       `json:"tags,omitempty"`
	Created      string         `json:"created,omitempty"` // ISO8601
	Notes        string         `json:"notes,omitempty"`
	JDCategory   int            `json:"jd_category,omitempty"`
	CustomFields map[string]any `json:"custom_fields,omitempty"`
}

// Correspondent is one entry in the sidecar's Correspondents array.
type Correspondent struct {
	Name string `json:"name"`
	Role string `json:"role,omitempty"` // defaults to "sender" if empty
}

// Parse decodes a sidecar payload. Handles both the native suchi
// shape (versioned via `suchi_sidecar: 1`) and a flat-JSON compat
// shape emitted by external producers (post-consume scripts, mail
// intake, third-party scanners) — flat JSON with `title`, `created`,
// `correspondent`, `tags`. The compat path lets you drop suchi into
// an existing ingest chain without patching the producer.
//
// Returns an error only on bad JSON or an explicit-but-mismatched
// suchi_sidecar version. Absence of the version key means "producer
// isn't announcing v1 explicitly" and drops to the compat parse.
func Parse(b []byte) (*V1, error) {
	var s V1
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, fmt.Errorf("sidecar: decode: %w", err)
	}
	// Compat: no version key → assume flat-JSON compat shape.
	// Every field the compat producer sets lives at the same key
	// name, so the initial Unmarshal already populated the shared
	// fields; just tag the record as version 1 for downstream
	// consumers.
	if s.Version == 0 {
		if !looksLikeFlatSidecar(b) {
			return nil, errors.New("sidecar: missing suchi_sidecar version and no recognized flat-shape keys")
		}
		s.Version = Version
		// Compat `correspondent` may be a "Name <email>" address
		// string. Peel to just the display name so tag lookups
		// aren't confused by the angle-bracket suffix.
		s.Correspondent = normalizeCorrespondent(s.Correspondent)
		return &s, nil
	}
	if s.Version != Version {
		return nil, fmt.Errorf("sidecar: unsupported version %d (this build understands v%d)", s.Version, Version)
	}
	return &s, nil
}

// looksLikeFlatSidecar is the cheap sniff for "this JSON came from
// a flat-shape compat producer, not from a broken suchi one." True
// when any of the well-known flat-shape keys is present.
func looksLikeFlatSidecar(b []byte) bool {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		return false
	}
	for _, k := range []string{"title", "created", "correspondent", "tags"} {
		if _, ok := m[k]; ok {
			return true
		}
	}
	return false
}

// normalizeCorrespondent strips a trailing " <address>" from a
// "Name <address>" string. Idempotent on plain names.
func normalizeCorrespondent(s string) string {
	if i := indexByte(s, '<'); i > 0 {
		return trimSpaceRight(s[:i])
	}
	return s
}

func indexByte(s string, c byte) int {
	for i := 0; i < len(s); i++ {
		if s[i] == c {
			return i
		}
	}
	return -1
}

func trimSpaceRight(s string) string {
	for len(s) > 0 && (s[len(s)-1] == ' ' || s[len(s)-1] == '\t') {
		s = s[:len(s)-1]
	}
	return s
}

// CreatedUnix returns the created date as unix seconds, or 0 if the
// producer didn't set one (or set an unparseable one). Accepts the
// common ISO layouts.
func (s *V1) CreatedUnix() int64 {
	if s == nil || s.Created == "" {
		return 0
	}
	for _, layout := range []string{
		time.RFC3339Nano,
		time.RFC3339,
		"2006-01-02T15:04:05.000Z07:00",
		"2006-01-02",
	} {
		if t, err := time.Parse(layout, s.Created); err == nil {
			return t.Unix()
		}
	}
	return 0
}
