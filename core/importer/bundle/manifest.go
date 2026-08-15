// Package bundle imports an export bundle from an existing DMS.
//
// Bundle shape (default `document_exporter` output):
//
//	<root>/
//	  manifest.json          -- array of Django-style {model,pk,fields} objects
//	  originals/*.pdf        -- original ingested bytes
//	  archive/*.pdf          -- OCR-rewritten PDFs (optional)
//
// `--split-manifest` produces a per-document sidecar json in the root
// directory alongside `manifest.json`. This importer accepts either.
//
// Contract:
//   - Idempotent + resumable: re-running the same command against the
//     same bundle re-imports nothing (legacy_id is UNIQUE).
//   - No partial state: each document import is a single transaction
//     that writes doc row + junction rows + notes together.
//   - Verbatim metadata: tags, correspondents, document_types,
//     storage_paths, custom_fields, notes flow through unchanged.
//   - Blobs land in the CAS by content hash. Two docs with identical
//     bytes will dedup in the CAS layer even if the source kept both.
//
// Not yet implemented (deferred to follow-ups, not the MVP importer):
//   - --map-jd: rule-based JD-category assignment. Until it lands,
//     imported docs go to the inbox category.
//   - --flat / --verify flags.
package bundle

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Object is the Django-style manifest row shape. fields is deferred to
// per-model decoding — the reflection tax on a shared struct is not worth
// it for a handful of models.
type Object struct {
	Model  string          `json:"model"`
	PK     int64           `json:"pk"`
	Fields json.RawMessage `json:"fields"`
}

// Manifest is the top-level shape shared by manifest.json and
// per-document sidecar files. Split-manifest files carry a single
// document.Object + associated custom-field-instance + note rows.
type Manifest []Object

// LoadManifests scans root for manifest.json + any split-manifest
// sidecar files and returns every Object across them. Duplicate PKs
// are the caller's problem; we do not merge here.
func LoadManifests(root string) (Manifest, error) {
	var out Manifest

	// Top-level manifest.json — the canonical location.
	if _, err := os.Stat(filepath.Join(root, "manifest.json")); err == nil {
		m, err := loadOne(filepath.Join(root, "manifest.json"))
		if err != nil {
			return nil, err
		}
		out = append(out, m...)
	}

	// Split-manifest sidecars: `<root>/*.json` other than manifest.json.
	// paperless v3 with -p writes them under `json/`; v2 -sm without -p
	// wrote them under `documents/`; older versions kept them at root.
	// Look in all three.
	dirs := []string{root, filepath.Join(root, "documents"), filepath.Join(root, "json")}
	for _, d := range dirs {
		entries, err := os.ReadDir(d)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
				continue
			}
			if e.Name() == "manifest.json" {
				continue
			}
			m, err := loadOne(filepath.Join(d, e.Name()))
			if err != nil {
				return nil, err
			}
			out = append(out, m...)
		}
	}

	if len(out) == 0 {
		return nil, fmt.Errorf("no manifest data found under %s", root)
	}
	return out, nil
}

func loadOne(path string) (Manifest, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	// The file may be either an array (manifest.json) or a single object
	// (a split-manifest document sidecar can be an array-of-one). Handle
	// both shapes.
	trim := trimSpaces(b)
	if len(trim) > 0 && trim[0] == '{' {
		var one Object
		if err := json.Unmarshal(trim, &one); err != nil {
			return nil, fmt.Errorf("decode object %s: %w", path, err)
		}
		return Manifest{one}, nil
	}
	var m Manifest
	if err := json.Unmarshal(trim, &m); err != nil {
		return nil, fmt.Errorf("decode array %s: %w", path, err)
	}
	return m, nil
}

func trimSpaces(b []byte) []byte {
	// exporter serializations are UTF-8 with occasional BOM. Strip
	// whitespace + BOM before probing the first byte.
	for len(b) > 0 {
		switch b[0] {
		case ' ', '\t', '\r', '\n':
			b = b[1:]
			continue
		case 0xef:
			if len(b) >= 3 && b[1] == 0xbb && b[2] == 0xbf {
				b = b[3:]
				continue
			}
		}
		break
	}
	return b
}

// ---------- typed field decoders per model ----------

// TagFields is what documents.tag rows look like in the manifest.
type TagFields struct {
	Name        string `json:"name"`
	Slug        string `json:"slug"`
	Color       string `json:"color"`
	IsInboxTag  bool   `json:"is_inbox_tag"`
	MatchAlg    int    `json:"matching_algorithm"`
	Match       string `json:"match"`
	Insensitive bool   `json:"is_insensitive"`
}

// CorrespondentFields, DocumentTypeFields, StoragePathFields share the
// bulk of their shape.
type CorrespondentFields struct {
	Name        string `json:"name"`
	Slug        string `json:"slug"`
	MatchAlg    int    `json:"matching_algorithm"`
	Match       string `json:"match"`
	Insensitive bool   `json:"is_insensitive"`
}

type DocumentTypeFields = CorrespondentFields

type StoragePathFields struct {
	Name        string `json:"name"`
	Slug        string `json:"slug"`
	Path        string `json:"path"`
	MatchAlg    int    `json:"matching_algorithm"`
	Match       string `json:"match"`
	Insensitive bool   `json:"is_insensitive"`
}

// CustomFieldFields is one field DEFINITION. Values live in CustomFieldInstance rows.
//
// paperless v3 emits data_type as a string enum ("text"/"monetary"/etc.);
// v2 used ints. We accept both via json.RawMessage and normalize at
// upsert time.
type CustomFieldFields struct {
	Name      string          `json:"name"`
	DataType  json.RawMessage `json:"data_type"`
	ExtraData json.RawMessage `json:"extra_data,omitempty"`
}

type CustomFieldInstance struct {
	Document int64 `json:"document"`
	Field    int64 `json:"field"`
	// v2 packed the value into a single polymorphic `value` field. v3 uses
	// separate typed columns — exactly one is non-null per row. Accept
	// both shapes and normalize at write time.
	Value          json.RawMessage `json:"value,omitempty"`
	ValueText      *string         `json:"value_text,omitempty"`
	ValueLongText  *string         `json:"value_long_text,omitempty"`
	ValueBool      *bool           `json:"value_bool,omitempty"`
	ValueURL       *string         `json:"value_url,omitempty"`
	ValueDate      *string         `json:"value_date,omitempty"`
	ValueInt       *int64          `json:"value_int,omitempty"`
	ValueFloat     *float64        `json:"value_float,omitempty"`
	ValueMonetary  *string         `json:"value_monetary,omitempty"`
	ValueSelect    json.RawMessage `json:"value_select,omitempty"`
	ValueDocuments json.RawMessage `json:"value_document_ids,omitempty"`
}

// EffectiveValue returns the JSON encoding of whichever v2/v3 value slot
// carries data. Returns nil (nil, nil) when the row is empty.
func (c CustomFieldInstance) EffectiveValue() json.RawMessage {
	if len(c.Value) > 0 && string(c.Value) != "null" {
		return c.Value
	}
	switch {
	case c.ValueText != nil:
		b, _ := json.Marshal(*c.ValueText)
		return b
	case c.ValueLongText != nil:
		b, _ := json.Marshal(*c.ValueLongText)
		return b
	case c.ValueBool != nil:
		b, _ := json.Marshal(*c.ValueBool)
		return b
	case c.ValueURL != nil:
		b, _ := json.Marshal(*c.ValueURL)
		return b
	case c.ValueDate != nil:
		b, _ := json.Marshal(*c.ValueDate)
		return b
	case c.ValueInt != nil:
		b, _ := json.Marshal(*c.ValueInt)
		return b
	case c.ValueFloat != nil:
		b, _ := json.Marshal(*c.ValueFloat)
		return b
	case c.ValueMonetary != nil:
		b, _ := json.Marshal(*c.ValueMonetary)
		return b
	case len(c.ValueSelect) > 0:
		return c.ValueSelect
	case len(c.ValueDocuments) > 0:
		return c.ValueDocuments
	}
	return nil
}

// NoteFields matches documents.note rows.
type NoteFields struct {
	Document int64  `json:"document"`
	User     *int64 `json:"user"`
	Note     string `json:"note"`
	Created  string `json:"created"` // the exporter writes ISO8601
}

// DocumentFields is the main event. the exporter serializes many optional
// fields; we consume only what suchi needs and log any surprises.
type DocumentFields struct {
	Title            string  `json:"title"`
	Content          string  `json:"content"`
	MimeType         string  `json:"mime_type"`
	Checksum         string  `json:"checksum"`         // md5 of original
	ArchiveChecksum  *string `json:"archive_checksum"` // md5 of archive; nil when no archive
	OriginalFilename string  `json:"original_filename"`
	ArchiveFilename  *string `json:"archive_filename"`
	StorageType      string  `json:"storage_type"`
	ArchiveSerialNo  *int64  `json:"archive_serial_number"`
	Created          string  `json:"created"`
	Modified         string  `json:"modified"`
	Added            string  `json:"added"`
	Correspondent    *int64  `json:"correspondent"`
	DocumentType     *int64  `json:"document_type"`
	StoragePath      *int64  `json:"storage_path"`
	Tags             []int64 `json:"tags"`
	Owner            *int64  `json:"owner"`
}

// Model constants — the "documents.foo" strings that appear in the
// manifest's Model field.
const (
	ModelTag           = "documents.tag"
	ModelCorrespondent = "documents.correspondent"
	ModelDocumentType  = "documents.documenttype"
	ModelStoragePath   = "documents.storagepath"
	ModelCustomField   = "documents.customfield"
	ModelFieldInstance = "documents.customfieldinstance"
	ModelDocument      = "documents.document"
	ModelNote          = "documents.note"
	ModelUser          = "auth.user"

	ModelSavedView           = "documents.savedview"
	ModelSavedViewFilterRule = "documents.savedviewfilterrule"
)

// FilePaths resolves the location of a document's original + archive
// files on disk relative to the bundle root.
//
// paperless v2 without `-p` writes files at `<root>/{original_filename}`.
// paperless v3 with `-p` names files as `originals/{date} {correspondent}
// {original_filename}.{ext}` (the "storage template") — no clean 1:1
// with original_filename. We try in order:
//
//  1. originals/{original_filename}          -- v2 & v3 without -p
//  2. root-level match: {original_filename}  -- older exporters
//  3. originals/ scan for a file whose name contains original_filename
//     -- v3 with -p
//
// The archive path uses the same fallback chain.
func FilePaths(root string, d DocumentFields) (original string, archive string) {
	original = resolveExportedFile(root, "originals", d.OriginalFilename)
	if d.ArchiveFilename != nil && *d.ArchiveFilename != "" {
		archive = resolveExportedFile(root, "archive", *d.ArchiveFilename)
	}
	return
}

// resolveExportedFile searches for a file matching name in the given
// subdirectory (and root). Returns the first existing path, or the
// nominal one so the caller can report a clean "no such file" error.
func resolveExportedFile(root, subdir, name string) string {
	if name == "" {
		return ""
	}
	direct := filepath.Join(root, subdir, name)
	if _, err := os.Stat(direct); err == nil {
		return direct
	}
	fallback := filepath.Join(root, name)
	if _, err := os.Stat(fallback); err == nil {
		return fallback
	}
	// v3 -p scan: files named like "{created} {correspondent} {name}.{ext}".
	subdirPath := filepath.Join(root, subdir)
	entries, err := os.ReadDir(subdirPath)
	if err == nil {
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			if strings.Contains(e.Name(), name) {
				return filepath.Join(subdirPath, e.Name())
			}
		}
	}
	return direct
}

// FirstExisting is a small helper for "here or there" file lookups; some
// operators pass in a root that already has originals/ inline, others
// have originals nested one deeper.
func FirstExisting(candidates ...string) (string, error) {
	for _, p := range candidates {
		if _, err := os.Stat(p); err == nil {
			return p, nil
		} else if !isNotExist(err) {
			return "", err
		}
	}
	return "", fs.ErrNotExist
}

func isNotExist(err error) bool {
	return err != nil && os.IsNotExist(err)
}

// ParseTime accepts the source's ISO8601 variants and returns a Unix
// timestamp. Returns 0 on empty/unparseable input — the caller decides
// whether to treat that as an error or a "use now()".
func ParseTime(s string) int64 {
	if s == "" {
		return 0
	}
	for _, layout := range []string{
		time.RFC3339Nano,
		time.RFC3339,
		"2006-01-02T15:04:05.000Z07:00",
		"2006-01-02",
	} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.Unix()
		}
	}
	return 0
}
