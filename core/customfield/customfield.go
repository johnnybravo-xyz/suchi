// Package customfield is the per-type dispatch layer for
// document_custom_field_values.
//
// custom_fields.data_type is a closed vocabulary of nine values;
// each behaves differently on write (which column?), read (which
// column matters?), validate (accept what shapes?), and render
// (format for display). Rather than switch-cases scattered across
// the api/, ui/, and importer/ packages, all the per-type logic
// lives here behind a single Handler struct that callers look up
// via Lookup(dataType).
//
// The three operations:
//
//   - Validate(extra, raw) → normalize a raw client value (from JSON,
//     from a sidecar, from a rules engine `then_value`) into the
//     concrete Go type the writer expects. Errors on bad shape.
//   - Write(tx, docID, fieldID, typed) → persist into the right
//     typed column of document_custom_field_values.
//   - Render(row) → pretty-print a stored row for the UI.
//
// Types shipped:
//
//	text          — value_text
//	url           — value_text; validated as an http/https URL
//	number        — value_number
//	monetary      — value_number; rendered with 2 decimals
//	date          — value_date (unix epoch); parses "YYYY-MM-DD" or int
//	bool          — value_bool (0/1)
//	select        — value_text; must be in extra_data.choices
//	multi         — value_text (JSON array); each element in choices
//	documentlink  — value_int; must be an existing document id
package customfield

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// ValueRow is what Read() pulls out of one document_custom_field_values
// row. Only one column is meaningful per data_type — Render() picks
// the right one.
type ValueRow struct {
	Text   sql.NullString
	Number sql.NullFloat64
	Int    sql.NullInt64
	Bool   sql.NullInt64
	Date   sql.NullInt64
}

// Handler carries the per-type behaviour. Zero-Validate → accept any
// value verbatim; zero-Write → typed value stored as text; zero-Render
// → best-effort ladder over the row.
type Handler struct {
	Name     string
	Validate func(extra json.RawMessage, raw any) (any, error)
	Write    func(ctx context.Context, tx *sql.Tx, docID, fieldID int64, typed any) error
	Render   func(row ValueRow) string
}

// Lookup returns the Handler for a data_type. Unknown types get the
// fallback (Text) so importer paths that predate a schema-vocabulary
// bump still land data instead of erroring.
func Lookup(dataType string) *Handler {
	if h, ok := registry[dataType]; ok {
		return h
	}
	return registry["text"]
}

var registry = map[string]*Handler{
	"text":         handlerText,
	"url":          handlerURL,
	"number":       handlerNumber,
	"monetary":     handlerMonetary,
	"date":         handlerDate,
	"bool":         handlerBool,
	"select":       handlerSelect,
	"multi":        handlerMulti,
	"documentlink": handlerDocumentLink,
}

// ---------- shared writers ----------

func writeText(ctx context.Context, tx *sql.Tx, docID, fieldID int64, s string) error {
	if s == "" {
		return deleteRow(ctx, tx, docID, fieldID)
	}
	return upsertValue(ctx, tx, docID, fieldID,
		`INSERT INTO document_custom_field_values(document_id, field_id, value_text)
		 VALUES (?, ?, ?)
		 ON CONFLICT(document_id, field_id)
		 DO UPDATE SET value_text = excluded.value_text,
		               value_number = NULL, value_int = NULL, value_bool = NULL, value_date = NULL`,
		s)
}

func writeNumber(ctx context.Context, tx *sql.Tx, docID, fieldID int64, n float64) error {
	return upsertValue(ctx, tx, docID, fieldID,
		`INSERT INTO document_custom_field_values(document_id, field_id, value_number)
		 VALUES (?, ?, ?)
		 ON CONFLICT(document_id, field_id)
		 DO UPDATE SET value_number = excluded.value_number,
		               value_text = NULL, value_int = NULL, value_bool = NULL, value_date = NULL`,
		n)
}

func writeInt(ctx context.Context, tx *sql.Tx, docID, fieldID int64, i int64) error {
	return upsertValue(ctx, tx, docID, fieldID,
		`INSERT INTO document_custom_field_values(document_id, field_id, value_int)
		 VALUES (?, ?, ?)
		 ON CONFLICT(document_id, field_id)
		 DO UPDATE SET value_int = excluded.value_int,
		               value_text = NULL, value_number = NULL, value_bool = NULL, value_date = NULL`,
		i)
}

func writeBool(ctx context.Context, tx *sql.Tx, docID, fieldID int64, b bool) error {
	v := 0
	if b {
		v = 1
	}
	return upsertValue(ctx, tx, docID, fieldID,
		`INSERT INTO document_custom_field_values(document_id, field_id, value_bool)
		 VALUES (?, ?, ?)
		 ON CONFLICT(document_id, field_id)
		 DO UPDATE SET value_bool = excluded.value_bool,
		               value_text = NULL, value_number = NULL, value_int = NULL, value_date = NULL`,
		v)
}

func writeDate(ctx context.Context, tx *sql.Tx, docID, fieldID int64, unixSec int64) error {
	return upsertValue(ctx, tx, docID, fieldID,
		`INSERT INTO document_custom_field_values(document_id, field_id, value_date)
		 VALUES (?, ?, ?)
		 ON CONFLICT(document_id, field_id)
		 DO UPDATE SET value_date = excluded.value_date,
		               value_text = NULL, value_number = NULL, value_int = NULL, value_bool = NULL`,
		unixSec)
}

func upsertValue(ctx context.Context, tx *sql.Tx, docID, fieldID int64, q string, v any) error {
	_, err := tx.ExecContext(ctx, q, docID, fieldID, v)
	return err
}

func deleteRow(ctx context.Context, tx *sql.Tx, docID, fieldID int64) error {
	_, err := tx.ExecContext(ctx,
		`DELETE FROM document_custom_field_values WHERE document_id = ? AND field_id = ?`,
		docID, fieldID)
	return err
}

// ---------- text ----------

var handlerText = &Handler{
	Name: "text",
	Validate: func(_ json.RawMessage, raw any) (any, error) {
		s, err := coerceString(raw)
		if err != nil {
			return nil, err
		}
		return s, nil
	},
	Write: func(ctx context.Context, tx *sql.Tx, docID, fieldID int64, typed any) error {
		return writeText(ctx, tx, docID, fieldID, typed.(string))
	},
	Render: func(row ValueRow) string {
		if row.Text.Valid {
			return row.Text.String
		}
		return ""
	},
}

// ---------- url ----------

var handlerURL = &Handler{
	Name: "url",
	Validate: func(_ json.RawMessage, raw any) (any, error) {
		s, err := coerceString(raw)
		if err != nil {
			return nil, err
		}
		if s == "" {
			return s, nil
		}
		u, err := url.Parse(s)
		if err != nil {
			return nil, fmt.Errorf("url: parse: %w", err)
		}
		if u.Scheme != "http" && u.Scheme != "https" {
			return nil, fmt.Errorf("url: scheme %q not http/https", u.Scheme)
		}
		return s, nil
	},
	Write: func(ctx context.Context, tx *sql.Tx, docID, fieldID int64, typed any) error {
		return writeText(ctx, tx, docID, fieldID, typed.(string))
	},
	Render: func(row ValueRow) string {
		if row.Text.Valid {
			return row.Text.String
		}
		return ""
	},
}

// ---------- number ----------

var handlerNumber = &Handler{
	Name: "number",
	Validate: func(_ json.RawMessage, raw any) (any, error) {
		return coerceFloat(raw)
	},
	Write: func(ctx context.Context, tx *sql.Tx, docID, fieldID int64, typed any) error {
		return writeNumber(ctx, tx, docID, fieldID, typed.(float64))
	},
	Render: func(row ValueRow) string {
		if !row.Number.Valid {
			return ""
		}
		// Trim trailing zeros: 42.500000 → 42.5, 42.0 → 42.
		s := strconv.FormatFloat(row.Number.Float64, 'f', -1, 64)
		return s
	},
}

// ---------- monetary ----------

var handlerMonetary = &Handler{
	Name: "monetary",
	Validate: func(_ json.RawMessage, raw any) (any, error) {
		return coerceFloat(raw)
	},
	Write: func(ctx context.Context, tx *sql.Tx, docID, fieldID int64, typed any) error {
		return writeNumber(ctx, tx, docID, fieldID, typed.(float64))
	},
	Render: func(row ValueRow) string {
		if !row.Number.Valid {
			return ""
		}
		return strconv.FormatFloat(row.Number.Float64, 'f', 2, 64)
	},
}

// ---------- date ----------

var handlerDate = &Handler{
	Name: "date",
	Validate: func(_ json.RawMessage, raw any) (any, error) {
		return coerceDate(raw)
	},
	Write: func(ctx context.Context, tx *sql.Tx, docID, fieldID int64, typed any) error {
		return writeDate(ctx, tx, docID, fieldID, typed.(int64))
	},
	Render: func(row ValueRow) string {
		if !row.Date.Valid {
			return ""
		}
		return time.Unix(row.Date.Int64, 0).UTC().Format("2006-01-02")
	},
}

// ---------- bool ----------

var handlerBool = &Handler{
	Name: "bool",
	Validate: func(_ json.RawMessage, raw any) (any, error) {
		switch v := raw.(type) {
		case bool:
			return v, nil
		case string:
			s := strings.ToLower(strings.TrimSpace(v))
			switch s {
			case "true", "yes", "y", "1", "on":
				return true, nil
			case "false", "no", "n", "0", "off", "":
				return false, nil
			}
			return nil, fmt.Errorf("bool: cannot parse %q", v)
		case float64:
			return v != 0, nil
		}
		return nil, fmt.Errorf("bool: unsupported type %T", raw)
	},
	Write: func(ctx context.Context, tx *sql.Tx, docID, fieldID int64, typed any) error {
		return writeBool(ctx, tx, docID, fieldID, typed.(bool))
	},
	Render: func(row ValueRow) string {
		if !row.Bool.Valid {
			return ""
		}
		if row.Bool.Int64 == 1 {
			return "Yes"
		}
		return "No"
	},
}

// ---------- select ----------

var handlerSelect = &Handler{
	Name: "select",
	Validate: func(extra json.RawMessage, raw any) (any, error) {
		s, err := coerceString(raw)
		if err != nil {
			return nil, err
		}
		if s == "" {
			return s, nil
		}
		choices, err := parseChoices(extra)
		if err != nil {
			return nil, err
		}
		if !contains(choices, s) {
			return nil, fmt.Errorf("select: %q not in choices %v", s, choices)
		}
		return s, nil
	},
	Write: func(ctx context.Context, tx *sql.Tx, docID, fieldID int64, typed any) error {
		return writeText(ctx, tx, docID, fieldID, typed.(string))
	},
	Render: func(row ValueRow) string {
		if row.Text.Valid {
			return row.Text.String
		}
		return ""
	},
}

// ---------- multi (JSON-encoded list of strings) ----------

var handlerMulti = &Handler{
	Name: "multi",
	Validate: func(extra json.RawMessage, raw any) (any, error) {
		var xs []string
		switch v := raw.(type) {
		case []any:
			xs = make([]string, 0, len(v))
			for _, x := range v {
				s, err := coerceString(x)
				if err != nil {
					return nil, err
				}
				xs = append(xs, s)
			}
		case []string:
			xs = v
		case string:
			// Comma-separated form for sidecar convenience.
			if v == "" {
				return xs, nil
			}
			for _, p := range strings.Split(v, ",") {
				p = strings.TrimSpace(p)
				if p != "" {
					xs = append(xs, p)
				}
			}
		default:
			return nil, fmt.Errorf("multi: unsupported type %T", raw)
		}
		choices, err := parseChoices(extra)
		if err != nil {
			return nil, err
		}
		for _, x := range xs {
			if !contains(choices, x) {
				return nil, fmt.Errorf("multi: %q not in choices %v", x, choices)
			}
		}
		return xs, nil
	},
	Write: func(ctx context.Context, tx *sql.Tx, docID, fieldID int64, typed any) error {
		xs := typed.([]string)
		if len(xs) == 0 {
			return deleteRow(ctx, tx, docID, fieldID)
		}
		b, err := json.Marshal(xs)
		if err != nil {
			return err
		}
		return writeText(ctx, tx, docID, fieldID, string(b))
	},
	Render: func(row ValueRow) string {
		if !row.Text.Valid || row.Text.String == "" {
			return ""
		}
		var xs []string
		if err := json.Unmarshal([]byte(row.Text.String), &xs); err != nil {
			return row.Text.String
		}
		return strings.Join(xs, ", ")
	},
}

// ---------- documentlink (references another doc id) ----------

var handlerDocumentLink = &Handler{
	Name: "documentlink",
	Validate: func(_ json.RawMessage, raw any) (any, error) {
		return coerceInt64(raw)
	},
	Write: func(ctx context.Context, tx *sql.Tx, docID, fieldID int64, typed any) error {
		target := typed.(int64)
		if target == 0 {
			return deleteRow(ctx, tx, docID, fieldID)
		}
		// FK-style check: target must exist and be alive.
		var exists int64
		err := tx.QueryRowContext(ctx,
			`SELECT id FROM documents WHERE id = ? AND trashed_at IS NULL`, target).Scan(&exists)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("documentlink: doc %d not found or trashed", target)
		}
		if err != nil {
			return err
		}
		return writeInt(ctx, tx, docID, fieldID, target)
	},
	Render: func(row ValueRow) string {
		if !row.Int.Valid {
			return ""
		}
		return "#" + strconv.FormatInt(row.Int.Int64, 10)
	},
}

// ---------- coerce helpers ----------

func coerceString(raw any) (string, error) {
	switch v := raw.(type) {
	case string:
		return strings.TrimSpace(v), nil
	case json.Number:
		return v.String(), nil
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64), nil
	case bool:
		if v {
			return "true", nil
		}
		return "false", nil
	case nil:
		return "", nil
	}
	return "", fmt.Errorf("string: unsupported type %T", raw)
}

func coerceFloat(raw any) (float64, error) {
	switch v := raw.(type) {
	case float64:
		return v, nil
	case json.Number:
		return v.Float64()
	case int:
		return float64(v), nil
	case int64:
		return float64(v), nil
	case string:
		s := strings.TrimSpace(v)
		if s == "" {
			return 0, nil
		}
		return strconv.ParseFloat(s, 64)
	case nil:
		return 0, nil
	}
	return 0, fmt.Errorf("number: unsupported type %T", raw)
}

func coerceInt64(raw any) (int64, error) {
	switch v := raw.(type) {
	case float64:
		return int64(v), nil
	case int:
		return int64(v), nil
	case int64:
		return v, nil
	case json.Number:
		return v.Int64()
	case string:
		s := strings.TrimSpace(v)
		if s == "" {
			return 0, nil
		}
		return strconv.ParseInt(s, 10, 64)
	case nil:
		return 0, nil
	}
	return 0, fmt.Errorf("int: unsupported type %T", raw)
}

// coerceDate accepts a unix epoch (int or numeric string) OR a
// "YYYY-MM-DD" string. Returns unix seconds at UTC midnight.
func coerceDate(raw any) (int64, error) {
	switch v := raw.(type) {
	case float64:
		return int64(v), nil
	case int:
		return int64(v), nil
	case int64:
		return v, nil
	case json.Number:
		return v.Int64()
	case string:
		s := strings.TrimSpace(v)
		if s == "" {
			return 0, nil
		}
		if t, err := time.Parse("2006-01-02", s); err == nil {
			return t.UTC().Unix(), nil
		}
		return strconv.ParseInt(s, 10, 64)
	case nil:
		return 0, nil
	}
	return 0, fmt.Errorf("date: unsupported type %T", raw)
}

// parseChoices reads the `choices` array out of extra_data JSON. Missing
// or malformed extra_data means "no restriction" — callers can decide
// whether that's an error at the field-definition boundary.
func parseChoices(extra json.RawMessage) ([]string, error) {
	if len(extra) == 0 || string(extra) == "{}" {
		return nil, errors.New("choices not defined (extra_data.choices missing)")
	}
	var m struct {
		Choices []string `json:"choices"`
	}
	if err := json.Unmarshal(extra, &m); err != nil {
		return nil, fmt.Errorf("extra_data parse: %w", err)
	}
	if len(m.Choices) == 0 {
		return nil, errors.New("choices empty")
	}
	return m.Choices, nil
}

func contains(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}
