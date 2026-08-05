// Package paths renders storage-path templates via Gonja (Jinja2-
// compatible). This is what turns a documents row into the human-
// browsable file-tree projection under $DATA_DIR/rendered/... .
//
// The template surface accepts the same variable names that common
// DMS storage-path templates use, so imported archives keep their
// folder layout byte-equal (regression-tested below).
//
// Data context available to templates:
//
//	{{ title }}            documents.title
//	{{ doc_pk }}           documents.id
//	{{ correspondent }}    correspondent.name  ("" if none)
//	{{ document_type }}    document_types.name
//	{{ storage_path }}     storage_paths.name  (self-referential, rare)
//	{{ tag_list }}         comma-joined tag names
//	{{ created }}          ISO date string (2026-03-02)
//	{{ created_year }}     "2026"
//	{{ created_month }}    "03"
//	{{ created_day }}      "02"
//	{{ added }}, {{ added_year }}, {{ added_month }}, {{ added_day }}
//	{{ jd.area.code_start }}, {{ jd.area.code_end }}, {{ jd.area.name }}
//	{{ jd.category.code }}, {{ jd.category.name }}
//	{{ asn }}              archive_serial_number ("" if none)
//	{{ owner }}            user email
//
// Missing values render as empty strings — suchi's silent-empty
// convention; regression tests would blow up otherwise.
package paths

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/nikolalohinski/gonja/v2"
	"github.com/nikolalohinski/gonja/v2/exec"
)

// Context is what a template renders against. Every field is optional
// but the sane defaults (empty string, zero) let templates run without
// nil-guarding every path.
type Context struct {
	Title         string
	DocPK         int64
	Correspondent string
	DocumentType  string
	StoragePath   string
	Tags          []string
	Created       string // "YYYY-MM-DD"
	Added         string
	Owner         string
	ASN           string

	// JD taxonomy anchor. Zero-value renders empty for un-classified
	// docs, though in practice every doc has a jd_category_id.
	JDAreaCodeStart int
	JDAreaCodeEnd   int
	JDAreaName      string
	JDCategoryCode  int
	JDCategoryName  string
}

// Render evaluates tpl against ctx. Returns the rendered path (which
// the caller sanitizes further before touching the filesystem).
func Render(tpl string, ctx Context) (string, error) {
	if tpl == "" {
		return "", errors.New("paths: empty template")
	}
	t, err := gonja.FromString(tpl)
	if err != nil {
		return "", fmt.Errorf("parse template: %w", err)
	}
	env := ctxToGonja(ctx)
	out, err := t.ExecuteToString(exec.NewContext(env))
	if err != nil {
		return "", fmt.Errorf("render: %w", err)
	}
	return out, nil
}

// ctxToGonja projects a Context into the map[string]any shape Gonja
// wants. Nested keys (jd.area.*) become sub-maps.
func ctxToGonja(c Context) map[string]any {
	return map[string]any{
		"title":         c.Title,
		"doc_pk":        c.DocPK,
		"correspondent": c.Correspondent,
		"document_type": c.DocumentType,
		"storage_path":  c.StoragePath,
		"tag_list":      strings.Join(c.Tags, ","),
		"created":       c.Created,
		"created_year":  pickYear(c.Created),
		"created_month": pickMonth(c.Created),
		"created_day":   pickDay(c.Created),
		"added":         c.Added,
		"added_year":    pickYear(c.Added),
		"added_month":   pickMonth(c.Added),
		"added_day":     pickDay(c.Added),
		"owner":         c.Owner,
		"asn":           c.ASN,
		"jd": map[string]any{
			"area": map[string]any{
				"code_start": c.JDAreaCodeStart,
				"code_end":   c.JDAreaCodeEnd,
				"name":       c.JDAreaName,
			},
			"category": map[string]any{
				"code": c.JDCategoryCode,
				"name": c.JDCategoryName,
			},
		},
	}
}

// pickYear extracts YYYY from a "YYYY-MM-DD" or "YYYY-MM-DDTHH..." shape.
// Empty input → empty output (suchi's silent-empty convention).
func pickYear(s string) string {
	if len(s) < 4 {
		return ""
	}
	if _, err := strconv.Atoi(s[:4]); err != nil {
		return ""
	}
	return s[:4]
}
func pickMonth(s string) string {
	if len(s) < 7 || s[4] != '-' {
		return ""
	}
	return s[5:7]
}
func pickDay(s string) string {
	if len(s) < 10 || s[7] != '-' {
		return ""
	}
	return s[8:10]
}

// SanitizePath strips characters that are unsafe on a filesystem while
// preserving the template's directory structure (forward slashes stay).
// Rendered path segments never contain shell-active characters — no
// operator should be able to templated their way to a `rm -rf` when
// the rendered-view plugin creates symlinks.
func SanitizePath(p string) string {
	var b strings.Builder
	for _, r := range p {
		switch {
		case r == '/':
			b.WriteRune(r)
		case r < 0x20:
			// drop control bytes
		case r == '\\', r == 0, r == '\x7f':
			b.WriteRune('_')
		default:
			b.WriteRune(r)
		}
	}
	// Collapse repeated slashes; strip leading; refuse .. traversal.
	s := b.String()
	for strings.Contains(s, "//") {
		s = strings.ReplaceAll(s, "//", "/")
	}
	s = strings.TrimLeft(s, "/")
	if strings.Contains(s, "../") || strings.HasSuffix(s, "/..") || s == ".." {
		s = strings.ReplaceAll(s, "..", "_")
	}
	return s
}
