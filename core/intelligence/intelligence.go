// SPDX-License-Identifier: AGPL-3.0-or-later

// Package intelligence defines validated, reviewable facts extracted from
// documents. Storage and approval stay generic; each intelligence type owns a
// small explicit value contract.
package intelligence

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"
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

func ValidateSortValue(candidateType, value string) error {
	if value == "" {
		return nil
	}
	switch candidateType {
	case TypeDate:
		parsed, err := time.Parse("2006-01-02", value)
		if err != nil || parsed.Format("2006-01-02") != value {
			return fmt.Errorf("date sort value %q must use YYYY-MM-DD", value)
		}
		return nil
	default:
		return fmt.Errorf("unknown intelligence type %q", candidateType)
	}
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
	candidate.RawText = strings.TrimSpace(candidate.RawText)
	candidate.EvidenceText = strings.TrimSpace(candidate.EvidenceText)
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

// EvidenceStart validates exact evidence against the original UTF-8 source.
// Offsets are byte offsets into that source, never into case-folded text.
func EvidenceStart(content string, candidate Candidate) (int64, bool) {
	validated, err := Validate(candidate)
	if err != nil || validated.SortValue != candidate.SortValue || !utf8.ValidString(content) {
		return 0, false
	}
	value, err := DecodeDate(candidate.ValueJSON)
	if err != nil {
		return 0, false
	}
	start := strings.Index(content, candidate.EvidenceText)
	raw := strings.Index(candidate.EvidenceText, candidate.RawText)
	if start < 0 || raw < 0 || !dateTextMatches(candidate.RawText, value) {
		return 0, false
	}
	return int64(start + raw), true
}

// A quote's presence alone does not ground a contradictory normalized date.
// Unambiguous year-first and explicitly labelled numeric forms retain their
// positions; locale-ambiguous numeric forms may assign month and day either way.
// Extra numbers or a contradicting month word fail closed.
func dateTextMatches(raw string, value DateValue) bool {
	parsed, err := time.Parse("2006-01-02", value.Date)
	if err != nil {
		return false
	}
	normalized := normalizeDateText(raw)
	numbers, months := dateTokens(normalized)
	if len(months) > 1 || len(months) == 1 && months[0] != int(parsed.Month()) {
		return false
	}
	isYear := func(n string) bool {
		v, _ := strconv.Atoi(n)
		return len(n) == 4 && (v == parsed.Year() || v == parsed.Year()+543) ||
			len(n) == 2 && v == parsed.Year()%100
	}
	isMonth := func(n string) bool {
		v, _ := strconv.Atoi(n)
		return len(n) <= 2 && v == int(parsed.Month())
	}
	isDay := func(n string) bool {
		v, _ := strconv.Atoi(n)
		return len(n) <= 2 && v == parsed.Day()
	}
	if len(months) == 0 {
		if matched, valid := positionalDateMatch(normalized, numbers, value.Precision, isYear, isMonth, isDay); matched {
			return valid
		}
	}
	switch value.Precision {
	case "year":
		return len(months) == 0 && len(numbers) == 1 && isYear(numbers[0])
	case "month":
		if len(months) == 1 {
			return len(numbers) == 1 && isYear(numbers[0])
		}
		return assignable(numbers, isYear, isMonth)
	default:
		if len(months) == 1 {
			return assignable(numbers, isYear, isDay)
		}
		return assignable(numbers, isYear, isMonth, isDay)
	}
}

func positionalDateMatch(normalized string, numbers []string, precision string, isYear, isMonth, isDay func(string) bool) (bool, bool) {
	var patterns []*regexp.Regexp
	requiredNumbers := 0
	switch precision {
	case "day":
		patterns = []*regexp.Regexp{yearFirstNumericDay, labelledNumericDay}
		requiredNumbers = 3
	case "month":
		patterns = []*regexp.Regexp{yearFirstNumericMonth, labelledNumericMonth}
		requiredNumbers = 2
	default:
		return false, false
	}
	for _, pattern := range patterns {
		if parts := pattern.FindStringSubmatch(normalized); parts != nil {
			valid := len(numbers) == requiredNumbers && isYear(parts[1]) && isMonth(parts[2])
			if precision == "day" {
				valid = valid && isDay(parts[3])
			}
			return true, valid
		}
	}
	return false, false
}

// assignable reports whether each number fills exactly one distinct role.
func assignable(numbers []string, roles ...func(string) bool) bool {
	if len(numbers) != len(roles) {
		return false
	}
	used := make([]bool, len(numbers))
	var fill func(int) bool
	fill = func(role int) bool {
		if role == len(roles) {
			return true
		}
		for i, n := range numbers {
			if !used[i] && roles[role](n) {
				used[i] = true
				if fill(role + 1) {
					return true
				}
				used[i] = false
			}
		}
		return false
	}
	return fill(0)
}

var (
	japaneseEra = regexp.MustCompile(`(令和|平成|昭和)([0-9]+|元)年`)
	// A separator is any run of dashes, spaces, underscores, commas, dots or
	// slashes, so "2024–03–05" and "2024 03 05" are positional too.
	yearFirstNumericDay   = regexp.MustCompile(`(?:^|[^0-9])([0-9]{4})[\p{Pd}\p{Z}\s_,./]+([0-9]{1,2})[\p{Pd}\p{Z}\s_,./]+([0-9]{1,2})(?:[^0-9]|$)`)
	yearFirstNumericMonth = regexp.MustCompile(`(?:^|[^0-9])([0-9]{4})[\p{Pd}\p{Z}\s_,./]+([0-9]{1,2})(?:[^0-9]|$)`)
	labelledNumericDay    = regexp.MustCompile(`(?:^|[^0-9])([0-9]{2,4})[[:space:]]*(?:年|년)[[:space:]]*([0-9]{1,2})[[:space:]]*(?:月|월)[[:space:]]*([0-9]{1,2})[[:space:]]*(?:日|일)?(?:[^0-9]|$)`)
	labelledNumericMonth  = regexp.MustCompile(`(?:^|[^0-9])([0-9]{2,4})[[:space:]]*(?:年|년)[[:space:]]*([0-9]{1,2})[[:space:]]*(?:月|월)(?:[^0-9]|$)`)
)

var japaneseEraStart = map[string]int{"令和": 2018, "平成": 1988, "昭和": 1925}

// Zero code points of decimal digit blocks that NFKC leaves as they are.
var digitZeros = []rune{0x0660, 0x06F0, 0x0966, 0x09E6, 0x0E50}

func normalizeDateText(raw string) string {
	text := []rune(norm.NFKC.String(raw))
	for i, r := range text {
		for _, zero := range digitZeros {
			if r >= zero && r <= zero+9 {
				text[i] = '0' + (r - zero)
			}
		}
	}
	return japaneseEra.ReplaceAllStringFunc(string(text), func(era string) string {
		parts := japaneseEra.FindStringSubmatch(era)
		year := 1
		if parts[2] != "元" {
			year, _ = strconv.Atoi(parts[2])
		}
		return strconv.Itoa(japaneseEraStart[parts[1]]+year) + "年"
	})
}

// dateTokens returns normalized text's ASCII number runs and the months named
// by its letter runs. Letter runs that are not month spellings are separators.
func dateTokens(normalized string) (numbers []string, months []int) {
	var number, word strings.Builder
	flush := func() {
		if number.Len() > 0 {
			numbers = append(numbers, number.String())
			number.Reset()
		}
		if word.Len() > 0 {
			if month, ok := monthLookup[foldWord(word.String())]; ok {
				months = append(months, month)
			}
			word.Reset()
		}
	}
	for _, r := range normalized {
		switch {
		case r >= '0' && r <= '9':
			if word.Len() > 0 {
				flush()
			}
			number.WriteRune(r)
		case unicode.IsLetter(r) || unicode.IsMark(r):
			if number.Len() > 0 {
				flush()
			}
			word.WriteRune(r)
		default:
			flush()
		}
	}
	flush()
	return numbers, months
}

// foldWord lowercases and drops combining marks, so "März", "MÄRZ" and
// "Marz" compare equal.
func foldWord(word string) string {
	var out strings.Builder
	for _, r := range norm.NFD.String(strings.ToLower(word)) {
		if !unicode.Is(unicode.Mn, r) {
			out.WriteRune(r)
		}
	}
	return out.String()
}

// monthLookup maps folded month spellings to 1–12. A spelling that names
// different months in different languages is left out, never guessed.
var monthLookup = func() map[string]int {
	lookup := map[string]int{}
	conflicts := map[string]bool{}
	for _, months := range monthWords {
		for i, spellings := range months {
			for _, spelling := range spellings {
				key := foldWord(spelling)
				if existing, ok := lookup[key]; ok && existing != i+1 {
					conflicts[key] = true
				}
				lookup[key] = i + 1
			}
		}
	}
	for key := range conflicts {
		delete(lookup, key)
	}
	return lookup
}()
