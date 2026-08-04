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

func TestRender_Monetary(t *testing.T) {
	h := Lookup("monetary")
	got := h.Render(ValueRow{Number: sql.NullFloat64{Float64: 4523, Valid: true}})
	if got != "4523.00" {
		t.Errorf("got %q want %q", got, "4523.00")
	}
}

func TestRender_Number_NoTrailingZeros(t *testing.T) {
	h := Lookup("number")
	got := h.Render(ValueRow{Number: sql.NullFloat64{Float64: 42.5, Valid: true}})
	if got != "42.5" {
		t.Errorf("got %q want %q", got, "42.5")
	}
	got = h.Render(ValueRow{Number: sql.NullFloat64{Float64: 42, Valid: true}})
	if got != "42" {
		t.Errorf("got %q want %q", got, "42")
	}
}

func TestRender_Date(t *testing.T) {
	h := Lookup("date")
	ts := time.Date(2026, 8, 4, 0, 0, 0, 0, time.UTC).Unix()
	got := h.Render(ValueRow{Date: sql.NullInt64{Int64: ts, Valid: true}})
	if got != "2026-08-04" {
		t.Errorf("got %q want %q", got, "2026-08-04")
	}
}

func TestRender_Bool(t *testing.T) {
	h := Lookup("bool")
	if got := h.Render(ValueRow{Bool: sql.NullInt64{Int64: 1, Valid: true}}); got != "Yes" {
		t.Errorf("true → %q", got)
	}
	if got := h.Render(ValueRow{Bool: sql.NullInt64{Int64: 0, Valid: true}}); got != "No" {
		t.Errorf("false → %q", got)
	}
	if got := h.Render(ValueRow{}); got != "" {
		t.Errorf("null → %q", got)
	}
}

func TestRender_Multi(t *testing.T) {
	h := Lookup("multi")
	got := h.Render(ValueRow{Text: sql.NullString{String: `["a","b","c"]`, Valid: true}})
	if got != "a, b, c" {
		t.Errorf("got %q want %q", got, "a, b, c")
	}
}

func TestRender_DocumentLink(t *testing.T) {
	h := Lookup("documentlink")
	got := h.Render(ValueRow{Int: sql.NullInt64{Int64: 42, Valid: true}})
	if got != "#42" {
		t.Errorf("got %q want %q", got, "#42")
	}
}

func TestLookup_UnknownFallsBackToText(t *testing.T) {
	h := Lookup("frobnicator")
	if h.Name != "text" {
		t.Errorf("unknown → %q want text", h.Name)
	}
}
