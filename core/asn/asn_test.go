// SPDX-License-Identifier: AGPL-3.0-or-later

package asn

import (
	"errors"
	"testing"
)

func TestValidatePrefix(t *testing.T) {
	for _, prefix := range []string{"ASN", "House-2:", "a.b_c"} {
		if err := ValidatePrefix(prefix); err != nil {
			t.Errorf("ValidatePrefix(%q): %v", prefix, err)
		}
	}
	for _, prefix := range []string{"", "1ASN", " ASN", "ASN ", `"ASN"`, "ÄSN", "ASN/"} {
		if err := ValidatePrefix(prefix); !errors.Is(err, ErrInvalidPrefix) {
			t.Errorf("ValidatePrefix(%q) = %v, want ErrInvalidPrefix", prefix, err)
		}
	}
}

func TestParseLabel(t *testing.T) {
	tests := []struct {
		name    string
		text    string
		value   int64
		matched bool
		bad     bool
	}{
		{name: "plain", text: "ASN42", value: 42, matched: true},
		{name: "zero padded", text: "ASN000042", value: 42, matched: true},
		{name: "safe maximum", text: "ASN9007199254740991", value: MaxValue, matched: true},
		{name: "different case", text: "asn42"},
		{name: "unrelated QR", text: "invoice:42"},
		{name: "empty suffix", text: "ASN", matched: true, bad: true},
		{name: "zero", text: "ASN0", matched: true, bad: true},
		{name: "negative", text: "ASN-1", matched: true, bad: true},
		{name: "signed", text: "ASN+1", matched: true, bad: true},
		{name: "space", text: "ASN 1", matched: true, bad: true},
		{name: "suffix", text: "ASN1x", matched: true, bad: true},
		{name: "non-ASCII digit", text: "ASN１", matched: true, bad: true},
		{name: "above safe range", text: "ASN9007199254740992", matched: true, bad: true},
		{name: "uint overflow", text: "ASN18446744073709551616", matched: true, bad: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			value, matched, err := ParseLabel("ASN", tc.text)
			if matched != tc.matched || value != tc.value {
				t.Fatalf("ParseLabel = (%d,%v,%v), want (%d,%v,...)", value, matched, err, tc.value, tc.matched)
			}
			if tc.bad && !errors.Is(err, ErrMalformedLabel) {
				t.Fatalf("ParseLabel error = %v, want ErrMalformedLabel", err)
			}
			if !tc.bad && err != nil {
				t.Fatalf("ParseLabel error = %v", err)
			}
		})
	}
}

func TestValidateValue(t *testing.T) {
	for _, value := range []int64{1, MaxValue} {
		if err := ValidateValue(value); err != nil {
			t.Errorf("ValidateValue(%d): %v", value, err)
		}
	}
	for _, value := range []int64{-1, 0, MaxValue + 1} {
		if err := ValidateValue(value); !errors.Is(err, ErrInvalidValue) {
			t.Errorf("ValidateValue(%d) = %v, want ErrInvalidValue", value, err)
		}
	}
}

func TestCanonicalLabel(t *testing.T) {
	got, err := CanonicalLabel("ASN", 42)
	if err != nil {
		t.Fatal(err)
	}
	if got != "ASN42" {
		t.Fatalf("CanonicalLabel = %q, want ASN42", got)
	}
}
