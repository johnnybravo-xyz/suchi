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

// Parse decodes a sidecar payload. Returns an error only on bad JSON
// or a version mismatch. Unknown fields decode into the struct's zero
// values; producers that need surprise-field visibility should use
// json.Decoder.DisallowUnknownFields explicitly.
func Parse(b []byte) (*V1, error) {
	var s V1
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, fmt.Errorf("sidecar: decode: %w", err)
	}
	if s.Version == 0 {
		return nil, errors.New("sidecar: missing suchi_sidecar version")
	}
	if s.Version != Version {
		return nil, fmt.Errorf("sidecar: unsupported version %d (this build understands v%d)", s.Version, Version)
	}
	return &s, nil
}

// CreatedUnix returns the created date as unix seconds, or 0 if the
// producer didn't set one (or set an unparseable one). Accepts the
// bundle-style ISO layouts.
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
