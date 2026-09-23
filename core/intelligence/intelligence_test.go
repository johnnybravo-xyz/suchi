// SPDX-License-Identifier: AGPL-3.0-or-later

package intelligence

import "testing"

func TestDateCandidateCanonicalizesGenericValue(t *testing.T) {
	candidate, err := NewDateCandidate(
		" Renewal ", "2026-09-01", "DAY",
		"  September 1, 2026 ", "Renews on September 1, 2026.", 0.92,
	)
	if err != nil {
		t.Fatal(err)
	}
	if candidate.Type != TypeDate || candidate.Role != "renewal" || candidate.SortValue != "2026-09-01" {
		t.Fatalf("candidate=%+v", candidate)
	}
	value, err := DecodeDate(candidate.ValueJSON)
	if err != nil {
		t.Fatal(err)
	}
	if value.Date != "2026-09-01" || value.Precision != "day" {
		t.Fatalf("date=%+v", value)
	}
}

func TestIntelligenceRejectsUnknownAndInvalidValues(t *testing.T) {
	if _, err := Validate(Candidate{Type: "amount"}); err == nil {
		t.Fatal("unknown type was accepted")
	}
	if _, err := NewDateCandidate("renewal", "2026-02-30", "day", "Feb 30", "Due Feb 30", 0.8); err == nil {
		t.Fatal("invalid date was accepted")
	}
	if _, err := NewDateCandidate("birthday", "2026-02-01", "day", "Feb 1", "Birthday Feb 1", 0.8); err == nil {
		t.Fatal("invalid role was accepted")
	}
}

func TestEvidenceRejectsMissingAlteredAndContradictoryQuotes(t *testing.T) {
	for _, tc := range []struct {
		name, content, raw, evidence, date string
		valid                              bool
	}{
		{"original UTF8", "İ — Due 06 Aug 2026", "06 Aug 2026", "Due 06 Aug 2026", "2026-08-06", true},
		{"invented evidence", "06 Aug 2026", "06 Aug 2026", "Due 06 Aug 2026", "2026-08-06", false},
		{"case altered", "Due 06 AUG 2026", "06 Aug 2026", "Due 06 Aug 2026", "2026-08-06", false},
		{"contradictory value", "Due 06 Aug 2026", "06 Aug 2026", "Due 06 Aug 2026", "2026-08-07", false},
		{"missing year", "Due Aug 6", "Aug 6", "Due Aug 6", "2026-08-06", false},
		{"multiline exact", "Due\n06 Aug 2026", "06 Aug 2026", "Due\n06 Aug 2026", "2026-08-06", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			candidate, err := NewDateCandidate("due", tc.date, "day", tc.raw, tc.evidence, 1)
			if err != nil {
				t.Fatal(err)
			}
			start, valid := EvidenceStart(tc.content, candidate)
			if valid != tc.valid {
				t.Fatalf("evidence valid=%v want=%v", valid, tc.valid)
			}
			if valid && tc.content[start:][:len(tc.raw)] != tc.raw {
				t.Fatal("evidence offset is not in original source")
			}
		})
	}
}

func TestEvidenceDoesNotInventDatePrecision(t *testing.T) {
	for _, tc := range []struct {
		raw, date, precision string
		valid                bool
	}{
		{"2026", "2026-01-01", "year", true},
		{"2026", "2026-01-01", "day", false},
		{"August 2026", "2026-08-01", "month", true},
		{"August 2026", "2026-08-01", "day", false},
	} {
		candidate, err := NewDateCandidate("due", tc.date, tc.precision, tc.raw, tc.raw, 1)
		if err != nil {
			t.Fatal(err)
		}
		if _, valid := EvidenceStart(tc.raw, candidate); valid != tc.valid {
			t.Fatalf("%q precision=%s valid=%v", tc.raw, tc.precision, valid)
		}
	}
}
