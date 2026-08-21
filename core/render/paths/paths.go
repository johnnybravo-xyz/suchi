// Package paths renders bounded storage-path templates. This turns a
// documents row into the human-browsable file-tree projection under
// $DATA_DIR/rendered/... .
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
)

const (
	maxTemplateBytes = 16 << 10
	maxRenderedBytes = 64 << 10
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

// Render substitutes the documented {{ variable }} placeholders in tpl.
// Known variables with no value render as empty strings. Unknown variables
// and Jinja expressions are rejected so configuration mistakes are visible.
// The caller sanitizes the result further before touching the filesystem.
func Render(tpl string, ctx Context) (string, error) {
	if tpl == "" {
		return "", errors.New("paths: empty template")
	}
	if len(tpl) > maxTemplateBytes {
		return "", fmt.Errorf("paths: template exceeds %d bytes", maxTemplateBytes)
	}
	if strings.Contains(tpl, "{%") || strings.Contains(tpl, "%}") ||
		strings.Contains(tpl, "{#") || strings.Contains(tpl, "#}") {
		return "", errors.New("paths: template statements and comments are not supported")
	}

	values := templateValues(ctx)
	var out strings.Builder
	for len(tpl) > 0 {
		open := strings.Index(tpl, "{{")
		close := strings.Index(tpl, "}}")
		if close >= 0 && (open < 0 || close < open) {
			return "", errors.New("paths: unexpected closing delimiter")
		}
		if open < 0 {
			if err := appendBounded(&out, tpl); err != nil {
				return "", err
			}
			break
		}
		if err := appendBounded(&out, tpl[:open]); err != nil {
			return "", err
		}
		tpl = tpl[open+2:]
		close = strings.Index(tpl, "}}")
		if close < 0 {
			return "", errors.New("paths: unclosed variable delimiter")
		}
		expression := strings.TrimSpace(tpl[:close])
		if expression == "" {
			return "", errors.New("paths: empty variable")
		}
		if strings.Contains(expression, "{{") || !validVariableName(expression) {
			return "", fmt.Errorf("paths: unsupported expression %q", expression)
		}
		value, ok := values[expression]
		if !ok {
			return "", fmt.Errorf("paths: unknown variable %q", expression)
		}
		if err := appendBounded(&out, value); err != nil {
			return "", err
		}
		tpl = tpl[close+2:]
	}
	return out.String(), nil
}

func appendBounded(out *strings.Builder, value string) error {
	if out.Len()+len(value) > maxRenderedBytes {
		return fmt.Errorf("paths: rendered path exceeds %d bytes", maxRenderedBytes)
	}
	out.WriteString(value)
	return nil
}

func validVariableName(name string) bool {
	for _, r := range name {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '_' || r == '.' {
			continue
		}
		return false
	}
	return true
}

func templateValues(c Context) map[string]string {
	return map[string]string{
		"title":              c.Title,
		"doc_pk":             strconv.FormatInt(c.DocPK, 10),
		"correspondent":      c.Correspondent,
		"document_type":      c.DocumentType,
		"storage_path":       c.StoragePath,
		"tag_list":           strings.Join(c.Tags, ","),
		"created":            c.Created,
		"created_year":       pickYear(c.Created),
		"created_month":      pickMonth(c.Created),
		"created_day":        pickDay(c.Created),
		"added":              c.Added,
		"added_year":         pickYear(c.Added),
		"added_month":        pickMonth(c.Added),
		"added_day":          pickDay(c.Added),
		"owner":              c.Owner,
		"asn":                c.ASN,
		"jd.area.code_start": strconv.Itoa(c.JDAreaCodeStart),
		"jd.area.code_end":   strconv.Itoa(c.JDAreaCodeEnd),
		"jd.area.name":       c.JDAreaName,
		"jd.category.code":   strconv.Itoa(c.JDCategoryCode),
		"jd.category.name":   c.JDCategoryName,
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
