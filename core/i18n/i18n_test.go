package i18n

import (
	"io"
	"log/slog"
	"testing"
)

func TestNormalizeLocale(t *testing.T) {
	tests := map[string]string{
		"":               "en",
		" EN_us ":        "en-us",
		"pt-BR":          "pt-br",
		"de-DE,de;q=0.9": "de-de",
	}
	for input, want := range tests {
		if got := NormalizeLocale(input); got != want {
			t.Errorf("NormalizeLocale(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestLoadResolvesBaseAndEnglishFallback(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	for _, locale := range []string{"en-US", "fr-FR"} {
		catalog, err := Load(locale, log)
		if err != nil {
			t.Fatalf("Load(%q): %v", locale, err)
		}
		if catalog.Locale != "en" {
			t.Errorf("Load(%q) locale = %q, want en", locale, catalog.Locale)
		}
		if got := catalog.Msg("nav.documents"); got != "Documents" {
			t.Errorf("Load(%q) nav.documents = %q", locale, got)
		}
	}
}

func TestLookupAndMissingMarker(t *testing.T) {
	catalog, err := Load("en", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := catalog.Lookup("nav.documents"); !ok || got != "Documents" {
		t.Fatalf("Lookup(nav.documents) = %q, %v", got, ok)
	}
	if got := catalog.Msg("missing.key"); got != "!missing.key!" {
		t.Fatalf("Msg(missing.key) = %q", got)
	}
}
