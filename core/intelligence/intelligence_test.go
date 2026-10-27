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

func TestEvidenceStartPreservesWrappedUnambiguousNumericDateOrder(t *testing.T) {
	for _, tc := range []struct {
		name, raw, date string
		valid           bool
	}{
		{"bare ISO swapped", "2026-03-04", "2026-04-03", false},
		{"bare slash swapped", "2026/03/04", "2026-04-03", false},
		{"bare Japanese swapped", "2026年3月4日", "2026-04-03", false},
		{"bare Korean swapped", "2026년 3월 4일", "2026-04-03", false},
		{"wrapped ISO correct", "Expiry: 2026-03-04", "2026-03-04", true},
		{"wrapped ISO swapped", "Expiry: 2026-03-04", "2026-04-03", false},
		{"wrapped Japanese correct", "2026年3月4日まで", "2026-03-04", true},
		{"wrapped Japanese swapped", "2026年3月4日まで", "2026-04-03", false},
		{"longer leading number", "Expiry: 12026-03-04", "2026-03-04", false},
		{"extra trailing number", "Expiry: 2026-03-04 account 7", "2026-03-04", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			candidate, err := NewDateCandidate("due", tc.date, "day", tc.raw, tc.raw, 1)
			if err != nil {
				t.Fatal(err)
			}
			if _, valid := EvidenceStart(tc.raw, candidate); valid != tc.valid {
				t.Fatalf("evidence valid=%v want=%v", valid, tc.valid)
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

func TestDateQuotesInManyLanguagesAndScripts(t *testing.T) {
	for _, tc := range []struct {
		raw, date, precision string
		valid                bool
	}{
		{"13. Oktober 2026", "2026-10-13", "day", true},
		{"13. Okt. 2026", "2026-10-13", "day", true},
		{"Dienstag, 13. Oktober 2026", "2026-10-13", "day", true},
		{"13. MÄRZ 2026", "2026-03-13", "day", true},
		{"13 octobre 2026", "2026-10-13", "day", true},
		{"13 de octubre de 2026", "2026-10-13", "day", true},
		{"13 ottobre 2026", "2026-10-13", "day", true},
		{"13 października 2026", "2026-10-13", "day", true},
		{"13. lokakuuta 2026", "2026-10-13", "day", true},
		{"13 октября 2026 г.", "2026-10-13", "day", true},
		{"13 ekim 2026", "2026-10-13", "day", true},
		{"2026年10月13日", "2026-10-13", "day", true},
		{"令和8年10月13日", "2026-10-13", "day", true},
		{"２０２６年１０月１３日", "2026-10-13", "day", true},
		{"2026년 10월 13일", "2026-10-13", "day", true},
		{"١٣/١٠/٢٠٢٦", "2026-10-13", "day", true},
		{"13 ตุลาคม 2569", "2026-10-13", "day", true},
		{"13.10.26", "2026-10-13", "day", true},
		{"October 13th, 2026", "2026-10-13", "day", true},
		{"2026-10-13", "2026-10-13", "day", true},
		{"2026-10-12", "2026-12-10", "day", false},
		{"2024–03–05", "2024-03-05", "day", true},
		{"2024–03–05", "2024-05-03", "day", false},
		{"2024—03—05", "2024-03-05", "day", true},
		{"2024—03—05", "2024-05-03", "day", false},
		{"2024 03 05", "2024-03-05", "day", true},
		{"2024 03 05", "2024-05-03", "day", false},
		{"2024_03_05", "2024-03-05", "day", true},
		{"2024_03_05", "2024-05-03", "day", false},
		{"2024,03,05", "2024-03-05", "day", true},
		{"2024,03,05", "2024-05-03", "day", false},
		{"2024. 03. 05.", "2024-03-05", "day", true},
		{"2024. 03. 05.", "2024-05-03", "day", false},
		{"2567-03-05", "2024-03-05", "day", true},
		{"2567-03-05", "2024-05-03", "day", false},
		{"2024-03", "2024-03-01", "month", true},
		{"2024-03", "2024-01-01", "month", false},
		{"2024 – 03", "2024-03-01", "month", true},
		{"2024 – 03", "2024-01-01", "month", false},
		{"24年3月5日", "2024-03-05", "day", true},
		{"24年3月5日", "2024-05-03", "day", false},
		{"令和6年3月5日", "2024-03-05", "day", true},
		{"令和6年3月5日", "2024-05-03", "day", false},
		{"2024년 3월", "2024-03-01", "month", true},
		{"2024년 3월", "2024-01-01", "month", false},
		{"2024年", "2024-01-01", "year", true},
		{"05.03.2024", "2024-03-05", "day", true},
		{"05.03.2024", "2024-05-03", "day", true},
		{"12/05/2024", "2024-12-05", "day", true},
		{"12/05/2024", "2024-05-12", "day", true},
		{"2026-03-04", "2026-04-03", "day", false},
		{"2026/03/04", "2026-04-03", "day", false},
		{"2026年3月4日", "2026-03-04", "day", true},
		{"2026年3月4日", "2026-04-03", "day", false},
		{"2026년 3월 4일", "2026-04-03", "day", false},
		{"02/01/2006", "2006-01-02", "day", true},
		{"02/01/2006", "2006-02-01", "day", true},
		{"Oktober 2026", "2026-10-01", "month", true},
		{"2026年10月", "2026-10-01", "month", true},
		{"10/2026", "2026-10-01", "month", true},
		{"14. Oktober 2026", "2026-10-13", "day", false},
		{"13. November 2026", "2026-10-13", "day", false},
		{"13.10.2026 14:30", "2026-10-13", "day", false},
		{"Oktober 2026", "2026-10-13", "day", false},
		{"martes 13 2026", "2026-10-13", "day", false},
		{"13 Oktober November 2026", "2026-10-13", "day", false},
		{"2026年10月", "2026-10-13", "day", false},
		{"Steuerjahr 2025", "2025-01-01", "year", true},
		{"Oktober 2026", "2026-01-01", "year", false},
	} {
		value := DateValue{Date: tc.date, Precision: tc.precision}
		if got := dateTextMatches(tc.raw, value); got != tc.valid {
			t.Errorf("%q as %s/%s = %v, want %v", tc.raw, tc.date, tc.precision, got, tc.valid)
		}
	}
}

func TestMonthSpellingsNameOneMonth(t *testing.T) {
	seen := map[string]int{}
	for language, months := range monthWords {
		for i, spellings := range months {
			for _, spelling := range spellings {
				key := foldWord(spelling)
				if month, ok := seen[key]; ok && month != i+1 {
					t.Errorf("%s %q names month %d and %d", language, spelling, month, i+1)
				}
				seen[key] = i + 1
			}
		}
	}
}
