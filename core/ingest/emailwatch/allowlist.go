package emailwatch

import (
	"net/mail"
	"strings"

	"github.com/johnnybravo-xyz/suchi/core/emailaccounts"
)

// MatchAddressCriteria reports whether an address matches a comma- or
// newline-separated list of full addresses and @domain suffixes. An empty list
// accepts everything.
//
// Entries are either a full email address (case-insensitive equality
// on the address part, display name ignored) or a `@domain` suffix
// (case-insensitive equality on the exact domain — no wildcard glob,
// no implicit subdomain match, to keep the semantics predictable for
// the owner writing the list).
//
// from is a raw From header value. Well-formed headers get parsed by
// net/mail; malformed ones fall through to raw-string equality against
// each entry so operators can still gate on the literal header text
// when a sender emits something net/mail refuses.
func MatchAddressCriteria(from, allowlist string) bool {
	allowlist = strings.TrimSpace(allowlist)
	if allowlist == "" {
		return true
	}
	from = strings.TrimSpace(from)
	if from == "" {
		return false
	}

	var addr, domain string
	if parsed, err := mail.ParseAddress(from); err == nil {
		addr = strings.ToLower(parsed.Address)
		if i := strings.LastIndex(addr, "@"); i >= 0 {
			domain = addr[i+1:]
		}
	}
	rawLower := strings.ToLower(from)

	for _, entry := range emailaccounts.SplitPolicyValues(allowlist) {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		entryLower := strings.ToLower(entry)

		if strings.HasPrefix(entryLower, "@") {
			if domain != "" && domain == entryLower[1:] {
				return true
			}
			continue
		}
		if addr != "" && addr == entryLower {
			return true
		}
		// Fallback for headers net/mail refused to parse — compare the
		// raw header text to the entry as-written.
		if addr == "" && rawLower == entryLower {
			return true
		}
	}
	return false
}
