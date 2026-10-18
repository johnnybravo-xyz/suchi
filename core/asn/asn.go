// SPDX-License-Identifier: AGPL-3.0-or-later

// Package asn validates archive serial numbers and their external QR labels.
package asn

import (
	"errors"
	"strconv"
	"strings"
)

// MaxValue is the largest archive serial number represented exactly by JSON
// numbers in every supported client.
const MaxValue int64 = 9007199254740991

var (
	ErrInvalidPrefix  = errors.New("invalid archive serial number prefix")
	ErrInvalidValue   = errors.New("invalid archive serial number")
	ErrMalformedLabel = errors.New("malformed archive serial number label")
)

// ValidatePrefix accepts the case-sensitive ASCII prefix grammar
// [A-Za-z][A-Za-z0-9._:-]*.
func ValidatePrefix(prefix string) error {
	if prefix == "" || !asciiLetter(prefix[0]) {
		return ErrInvalidPrefix
	}
	for i := 1; i < len(prefix); i++ {
		c := prefix[i]
		if !asciiLetter(c) && (c < '0' || c > '9') && c != '.' && c != '_' && c != ':' && c != '-' {
			return ErrInvalidPrefix
		}
	}
	return nil
}

// ValidateValue accepts positive serial numbers in the safe JSON integer range.
func ValidateValue(value int64) error {
	if value <= 0 || value > MaxValue {
		return ErrInvalidValue
	}
	return nil
}

// ParseLabel recognizes an exact prefix followed immediately by ASCII digits.
// matched is false only when text does not begin with prefix. Prefix-looking
// malformed labels return matched=true with ErrMalformedLabel.
func ParseLabel(prefix, text string) (value int64, matched bool, err error) {
	if err := ValidatePrefix(prefix); err != nil {
		return 0, false, err
	}
	if !strings.HasPrefix(text, prefix) {
		return 0, false, nil
	}
	suffix := text[len(prefix):]
	if suffix == "" {
		return 0, true, ErrMalformedLabel
	}
	for i := 0; i < len(suffix); i++ {
		if suffix[i] < '0' || suffix[i] > '9' {
			return 0, true, ErrMalformedLabel
		}
	}
	n, parseErr := strconv.ParseUint(suffix, 10, 64)
	if parseErr != nil || n == 0 || n > uint64(MaxValue) {
		return 0, true, ErrMalformedLabel
	}
	return int64(n), true, nil
}

// CanonicalLabel renders a validated value without preserving zero padding.
func CanonicalLabel(prefix string, value int64) (string, error) {
	if err := ValidatePrefix(prefix); err != nil {
		return "", err
	}
	if err := ValidateValue(value); err != nil {
		return "", err
	}
	return prefix + strconv.FormatInt(value, 10), nil
}

func asciiLetter(c byte) bool {
	return c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z'
}
