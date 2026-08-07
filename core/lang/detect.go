// Package lang is the extensibility seam for per-document
// language detection. suchi doesn't bundle any Go-native
// language detector — the two live candidates are whatlanggo
// (abandoned) and lingua-go (heavy, unmeasured binary impact).
// Instead this package defines a small Detector interface + a
// Chain that composes multiple detectors, and the default suchi
// build wires only the cheap metadata-hint sources (PDF /Lang,
// email Content-Language) plus the LLM classifier response
// field (see plugins/llm-classifier).
//
// Ops teams that want richer offline detection register a plugin
// detector at boot via Chain.Register — the natural home for a
// future xberg-sidecar or vendored lingua-go integration.
//
// Storage format: documents.languages stores comma-bracketed
// ISO-639-1 codes (,de,  ,de,en,). Empty = not detected yet.
// Callers should Format() before writing to the column and
// Parse() when reading back into a []string.

package lang

import (
	"regexp"
	"strings"
)

// Result is one detector's finding. Code is an ISO-639-1
// lowercase 2-letter code (or "" if unknown); Confidence is
// 0.0..1.0 — detectors that don't produce a real confidence
// number should report 1.0 for "trust me" (metadata hints do
// this) or 0.0 to force the chain to move on.
type Result struct {
	Code       string
	Confidence float64
}

// Detector produces language candidates for a chunk of text.
// Detectors are called at post-ingest time, after content is
// extracted; they must be safe for concurrent use — one document
// per goroutine, but the same Detector may be invoked from many
// goroutines at once.
type Detector interface {
	// Name is a short identifier for logging and skip-tracking.
	// Convention: kebab-case, e.g. "pdf-lang-tag", "llm-classifier",
	// "xberg-sidecar".
	Name() string

	// Detect returns candidates. Ordered best-first is convenient
	// but the Chain re-inspects each Result against its threshold
	// anyway. Empty slice (or nil) means "no opinion" — the chain
	// moves on. Errors are logged and treated as "no opinion".
	Detect(text string) ([]Result, error)
}

// iso6391Re accepts the ISO-639-1 shape (2 lowercase letters).
// Some cousin standards like ISO-639-3 use 3 letters; we
// tolerate 3-letter codes as a passthrough so plugins can emit
// e.g. `kok` (Konkani, not in 639-1) without the chain rejecting
// them. Length checks in Format() enforce sanity, not identity.
var iso6391Re = regexp.MustCompile(`^[a-z]{2,3}$`)

// Format writes an ISO code (single, or comma-separated) as the
// storage form used by documents.languages: comma-bracketed and
// duplicate-free.
//
//	Format("de")        == ",de,"
//	Format("de,en")     == ",de,en,"
//	Format("DE, en, ")  == ",de,en,"
//	Format("de,de,en")  == ",de,en,"
//	Format("")          == ""
//	Format("bad code")  == ""    // sanitised out
func Format(codes string) string {
	codes = strings.TrimSpace(codes)
	if codes == "" {
		return ""
	}
	parts := strings.Split(codes, ",")
	out := make([]string, 0, len(parts))
	seen := map[string]bool{}
	for _, p := range parts {
		p = strings.ToLower(strings.TrimSpace(p))
		if p == "" || seen[p] || !iso6391Re.MatchString(p) {
			continue
		}
		seen[p] = true
		out = append(out, p)
	}
	if len(out) == 0 {
		return ""
	}
	return "," + strings.Join(out, ",") + ","
}

// Parse decodes a stored value ",de,en," into ["de", "en"].
// Empty stored value returns nil.
func Parse(stored string) []string {
	stored = strings.Trim(stored, ",")
	if stored == "" {
		return nil
	}
	parts := strings.Split(stored, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// Primary returns the first code in a stored value, or "" if
// none. Used to feed a single-language hint into the LLM
// classifier or an OCR autodetect call.
func Primary(stored string) string {
	codes := Parse(stored)
	if len(codes) == 0 {
		return ""
	}
	return codes[0]
}
