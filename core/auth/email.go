// SPDX-License-Identifier: AGPL-3.0-or-later

package auth

import (
	"errors"
	"regexp"
	"strings"
)

const (
	MaxEmailBytes           = 254
	ReservedDevEmail        = "dev@suchi.local"
	ReservedDemoCorpusEmail = "corpus@demo.suchi.page"
)

var (
	ErrInvalidEmail  = errors.New("invalid email address")
	ErrReservedEmail = errors.New("reserved email address")

	// Intentionally permissive: one local part, one dotted domain, and no
	// whitespace or shell-active delimiters. Full RFC 5322 parsing rejects
	// legitimate identities without improving this local account boundary.
	emailPattern = regexp.MustCompile(`^[^\s@<>"'\\;]+@[^\s@<>"'\\;]+\.[^\s@<>"'\\;]+$`)
)

// NormalizeEmail canonicalizes ordinary account identity input. Internal
// development and demo seeders bypass this function deliberately; no operator,
// HTTP client, or identity provider may claim those reserved identities.
func NormalizeEmail(value string) (string, error) {
	email := strings.ToLower(strings.TrimSpace(value))
	if email == "" || len(email) > MaxEmailBytes || !emailPattern.MatchString(email) {
		return "", ErrInvalidEmail
	}
	switch email {
	case ReservedDevEmail, ReservedDemoCorpusEmail:
		return "", ErrReservedEmail
	default:
		return email, nil
	}
}
