// Package intelligence defines validated, reviewable facts extracted from
// documents. Storage and approval stay generic; each intelligence type owns a
// small explicit value contract.
package intelligence

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	TypeDate = "date"
)

type Candidate struct {
	Type         string
	Role         string
	ValueJSON    string
	SortValue    string
	RawText      string
	EvidenceText string
	Confidence   float64
}

type DateValue struct {
	Date      string `json:"date"`
	Precision string `json:"precision"`
}

var dateRoles = map[string]bool{
	"issued": true, "due": true, "start": true, "end": true,
	"expiry": true, "renewal": true, "service": true, "other": true,
}

var datePrecisions = map[string]bool{
	"day": true, "month": true, "year": true,
}

func KnownType(value string) bool {
	return value == TypeDate
}

func ValidRole(candidateType, role string) bool {
	switch candidateType {
	case TypeDate:
		return dateRoles[role]
	default:
		return false
	}
}

func DateRoles() []string {
	return []string{"issued", "due", "start", "end", "expiry", "renewal", "service", "other"}
}

func NewDateCandidate(role, value, precision, rawText, evidence string, confidence float64) (Candidate, error) {
	dateValue := DateValue{Date: strings.TrimSpace(value), Precision: strings.ToLower(strings.TrimSpace(precision))}
	encoded, err := json.Marshal(dateValue)
	if err != nil {
		return Candidate{}, err
	}
	return Validate(Candidate{
		Type: TypeDate, Role: strings.ToLower(strings.TrimSpace(role)),
		ValueJSON: string(encoded), SortValue: dateValue.Date,
		RawText: rawText, EvidenceText: evidence, Confidence: confidence,
	})
}

func DecodeDate(valueJSON string) (DateValue, error) {
	var value DateValue
	if err := json.Unmarshal([]byte(valueJSON), &value); err != nil {
		return DateValue{}, fmt.Errorf("decode date intelligence: %w", err)
	}
	value.Date = strings.TrimSpace(value.Date)
	value.Precision = strings.ToLower(strings.TrimSpace(value.Precision))
	if err := validateDateValue(value); err != nil {
		return DateValue{}, err
	}
	return value, nil
}

func Validate(candidate Candidate) (Candidate, error) {
	candidate.Type = strings.ToLower(strings.TrimSpace(candidate.Type))
	candidate.Role = strings.ToLower(strings.TrimSpace(candidate.Role))
	candidate.RawText = cleanText(candidate.RawText)
	candidate.EvidenceText = cleanText(candidate.EvidenceText)
	if !KnownType(candidate.Type) {
		return Candidate{}, fmt.Errorf("unknown intelligence type %q", candidate.Type)
	}
	if !ValidRole(candidate.Type, candidate.Role) {
		return Candidate{}, fmt.Errorf("invalid %s role %q", candidate.Type, candidate.Role)
	}
	if candidate.RawText == "" || candidate.EvidenceText == "" {
		return Candidate{}, errors.New("raw text and evidence are required")
	}
	if utf8.RuneCountInString(candidate.RawText) > 200 || utf8.RuneCountInString(candidate.EvidenceText) > 500 {
		return Candidate{}, errors.New("intelligence evidence is too long")
	}
	if math.IsNaN(candidate.Confidence) || math.IsInf(candidate.Confidence, 0) ||
		candidate.Confidence < 0 || candidate.Confidence > 1 {
		return Candidate{}, errors.New("intelligence confidence must be between 0 and 1")
	}

	switch candidate.Type {
	case TypeDate:
		value, err := DecodeDate(candidate.ValueJSON)
		if err != nil {
			return Candidate{}, err
		}
		encoded, _ := json.Marshal(value)
		candidate.ValueJSON = string(encoded)
		candidate.SortValue = value.Date
	}
	return candidate, nil
}

func validateDateValue(value DateValue) error {
	parsed, err := time.Parse("2006-01-02", value.Date)
	if err != nil || parsed.Format("2006-01-02") != value.Date {
		return fmt.Errorf("date value %q must use a valid YYYY-MM-DD date", value.Date)
	}
	if !datePrecisions[value.Precision] {
		return fmt.Errorf("invalid date precision %q", value.Precision)
	}
	if value.Precision == "month" && parsed.Day() != 1 {
		return errors.New("month-precision dates must use day 01")
	}
	if value.Precision == "year" && (parsed.Month() != time.January || parsed.Day() != 1) {
		return errors.New("year-precision dates must use January 01")
	}
	return nil
}

func cleanText(value string) string {
	return strings.Join(strings.Fields(value), " ")
}
