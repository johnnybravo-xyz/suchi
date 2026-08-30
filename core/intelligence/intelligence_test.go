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
