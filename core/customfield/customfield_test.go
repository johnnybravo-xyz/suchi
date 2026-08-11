package customfield

import (
	"database/sql"
	"encoding/json"
	"testing"
	"time"
)

func TestValidate_Text(t *testing.T) {
	h := Lookup("text")
	got, err := h.Validate(nil, "  hello  ")
	if err != nil {
		t.Fatal(err)
	}
	if got.(string) != "hello" {
		t.Fatalf("got %q want %q", got, "hello")
	}
}

func TestValidate_URL(t *testing.T) {
	h := Lookup("url")
	if _, err := h.Validate(nil, "https://example.com"); err != nil {
		t.Fatalf("valid https: %v", err)
	}
	if _, err := h.Validate(nil, "ftp://example.com"); err == nil {
		t.Fatal("ftp should fail")
	}
	if _, err := h.Validate(nil, ""); err != nil {
		t.Fatalf("empty should be OK: %v", err)
	}
}

func TestValidate_Number(t *testing.T) {
	h := Lookup("number")
	cases := map[any]float64{
		42.5:       42.5,
		"3.14":     3.14,
		float64(1): 1.0,
		int(7):     7.0,
	}
	for in, want := range cases {
		got, err := h.Validate(nil, in)
		if err != nil {
			t.Errorf("%v: %v", in, err)
			continue
		}
		if got.(float64) != want {
			t.Errorf("%v → %v want %v", in, got, want)
		}
	}
}

func TestValidate_Date(t *testing.T) {
	h := Lookup("date")
	got, err := h.Validate(nil, "2026-08-04")
	if err != nil {
		t.Fatal(err)
	}
	want := time.Date(2026, 8, 4, 0, 0, 0, 0, time.UTC).Unix()
	if got.(int64) != want {
		t.Errorf("got %v want %v", got, want)
	}
	// Unix input passes through.
	got, err = h.Validate(nil, int64(1000000))
	if err != nil {
		t.Fatal(err)
	}
	if got.(int64) != 1000000 {
		t.Errorf("unix passthrough got %v want %v", got, 1000000)
	}
}

func TestValidate_Bool(t *testing.T) {
	h := Lookup("bool")
	cases := map[any]bool{
		true:  true,
		false: false,
		"yes": true,
		"NO":  false,
		"1":   true,
		"0":   false,
		1.0:   true,
		0.0:   false,
	}
	for in, want := range cases {
		got, err := h.Validate(nil, in)
		if err != nil {
			t.Errorf("%v: %v", in, err)
			continue
		}
		if got.(bool) != want {
			t.Errorf("%v → %v want %v", in, got, want)
		}
	}
	if _, err := h.Validate(nil, "maybe"); err == nil {
		t.Fatal("maybe should fail")
	}
}

func TestValidate_Select(t *testing.T) {
	h := Lookup("select")
	extra := json.RawMessage(`{"choices":["red","green","blue"]}`)
	if _, err := h.Validate(extra, "green"); err != nil {
		t.Fatalf("green: %v", err)
	}
	if _, err := h.Validate(extra, "purple"); err == nil {
		t.Fatal("purple should fail")
	}
	// Empty extra_data → error.
	if _, err := h.Validate(json.RawMessage(`{}`), "green"); err == nil {
		t.Fatal("empty choices should fail")
	}
}

func TestValidate_Multi(t *testing.T) {
	h := Lookup("multi")
	extra := json.RawMessage(`{"choices":["a","b","c"]}`)

	// Slice form.
	got, err := h.Validate(extra, []any{"a", "b"})
	if err != nil {
		t.Fatal(err)
	}
	xs := got.([]string)
	if len(xs) != 2 || xs[0] != "a" || xs[1] != "b" {
		t.Errorf("slice: got %v", xs)
	}

	// Comma-separated string form.
	got, err = h.Validate(extra, "a, c")
	if err != nil {
		t.Fatal(err)
	}
	xs = got.([]string)
	if len(xs) != 2 || xs[0] != "a" || xs[1] != "c" {
		t.Errorf("csv: got %v", xs)
	}

	// Out-of-vocabulary element fails.
	if _, err := h.Validate(extra, []any{"a", "z"}); err == nil {
		t.Fatal("z should fail")
	}
}

// TestRender is a table over Handler.Render — one row per (kind, input)
// pair. Adding a new format-quirk is a one-line row.
func TestRender(t *testing.T) {
	dateTs := time.Date(2026, 8, 4, 0, 0, 0, 0, time.UTC).Unix()
	cases := []struct {
		kind string
		val  ValueRow
		want string
	}{
		{"monetary", ValueRow{Number: sql.NullFloat64{Float64: 4523, Valid: true}}, "4523.00"},
		{"number", ValueRow{Number: sql.NullFloat64{Float64: 42.5, Valid: true}}, "42.5"},
		{"number", ValueRow{Number: sql.NullFloat64{Float64: 42, Valid: true}}, "42"},
		{"date", ValueRow{Date: sql.NullInt64{Int64: dateTs, Valid: true}}, "2026-08-04"},
		{"bool", ValueRow{Bool: sql.NullInt64{Int64: 1, Valid: true}}, "Yes"},
		{"bool", ValueRow{Bool: sql.NullInt64{Int64: 0, Valid: true}}, "No"},
		{"bool", ValueRow{}, ""},
		{"multi", ValueRow{Text: sql.NullString{String: `["a","b","c"]`, Valid: true}}, "a, b, c"},
		{"documentlink", ValueRow{Int: sql.NullInt64{Int64: 42, Valid: true}}, "#42"},
	}
	for _, tc := range cases {
		t.Run(tc.kind+"/"+tc.want, func(t *testing.T) {
			got := Lookup(tc.kind).Render(tc.val)
			if got != tc.want {
				t.Errorf("%s: got %q want %q", tc.kind, got, tc.want)
			}
		})
	}
}

func TestLookup_UnknownFallsBackToText(t *testing.T) {
	h := Lookup("frobnicator")
	if h.Name != "text" {
		t.Errorf("unknown → %q want text", h.Name)
	}
}
