package main

// Sanity check for the export helpers — no CLI harness, just proves
// the filename-safe transform + MIME extension mapping stay stable.
// Full end-to-end (real DB + CAS + zip inspection) lives in the
// smoke script; that's a heavier lift than unit tests want.

import (
	"strings"
	"testing"
)

func TestSafeExportName(t *testing.T) {
	cases := []struct {
		title string
		id    int64
		want  string
	}{
		{"Electricity bill March 2026", 42, "Electricity bill March 2026-42"},
		{"receipts/2026/03", 91, "receipts_2026_03-91"},
		{"a\\b:c*d?e\"f<g>h|i", 7, "a_b_c_d_e_f_g_h_i-7"},
		{"", 1, "document-1"},
		{"   ", 2, "document-2"},
		{strings.Repeat("x", 200), 3, strings.Repeat("x", 60) + "-3"},
	}
	for _, c := range cases {
		got := safeExportName(c.title, c.id, "")
		if got != c.want {
			t.Errorf("safeExportName(%q, %d) = %q, want %q", c.title, c.id, got, c.want)
		}
	}
}

func TestExtForMIME(t *testing.T) {
	cases := map[string]string{
		"application/pdf":            ".pdf",
		"image/png":                  ".png",
		"image/jpeg":                 ".jpg",
		"message/rfc822":             ".eml",
		"application/vnd.ms-outlook": ".msg",
		"application/octet-stream":   "",
		"":                           "",
	}
	for mime, want := range cases {
		if got := extForMIME(mime); got != want {
			t.Errorf("extForMIME(%q) = %q, want %q", mime, got, want)
		}
	}
}
