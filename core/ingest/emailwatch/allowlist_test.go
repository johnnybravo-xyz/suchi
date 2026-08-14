package emailwatch_test

import (
	"testing"

	"github.com/johnnybravo-xyz/suchi/core/ingest/emailwatch"
)

// Behavior contract for MatchFromAllowlist. Each row encodes one rule
// from the schema comment on accounts.from_allowlist so a future
// rewrite of the helper can be validated against the same table.
func TestMatchFromAllowlist(t *testing.T) {
	cases := []struct {
		name      string
		from      string
		allowlist string
		want      bool
	}{
		{
			name:      "empty allowlist accepts anything",
			from:      "alice@example.com",
			allowlist: "",
			want:      true,
		},
		{
			name:      "empty allowlist accepts empty from",
			from:      "",
			allowlist: "",
			want:      true,
		},
		{
			name:      "whitespace-only allowlist accepts anything",
			from:      "alice@example.com",
			allowlist: "   ",
			want:      true,
		},
		{
			name:      "empty from with a configured allowlist is rejected",
			from:      "",
			allowlist: "alice@example.com",
			want:      false,
		},
		{
			name:      "exact address match with display name",
			from:      "Alice <alice@example.com>",
			allowlist: "alice@example.com",
			want:      true,
		},
		{
			name:      "case-insensitive address match",
			from:      "ALICE@EXAMPLE.COM",
			allowlist: "alice@example.com",
			want:      true,
		},
		{
			name:      "mixed-case entry matches lowercase from",
			from:      "alice@example.com",
			allowlist: "Alice@Example.COM",
			want:      true,
		},
		{
			name:      "address that isn't in the list is rejected",
			from:      "mallory@example.com",
			allowlist: "alice@example.com, bob@example.com",
			want:      false,
		},
		{
			name:      "domain suffix match on the exact domain",
			from:      "bills@bescom.co.in",
			allowlist: "@bescom.co.in",
			want:      true,
		},
		{
			name:      "domain suffix does not match subdomain",
			from:      "a@foo.bescom.co.in",
			allowlist: "@bescom.co.in",
			want:      false,
		},
		{
			name:      "domain suffix case-insensitive",
			from:      "Bills <bills@BESCOM.co.in>",
			allowlist: "@bescom.co.in",
			want:      true,
		},
		{
			name:      "multiple entries, address match wins",
			from:      "bob@x.io",
			allowlist: "alice@example.com, @bescom.co.in , bob@x.io",
			want:      true,
		},
		{
			name:      "multiple entries, domain match wins",
			from:      "meter@bescom.co.in",
			allowlist: "alice@example.com, @bescom.co.in , bob@x.io",
			want:      true,
		},
		{
			name:      "multiple entries, no match",
			from:      "eve@evil.example",
			allowlist: "alice@example.com, @bescom.co.in , bob@x.io",
			want:      false,
		},
		{
			name:      "empty entries from double comma and trailing comma are ignored",
			from:      "alice@example.com",
			allowlist: ",, alice@example.com,,",
			want:      true,
		},
		{
			name:      "allowlist of only commas has no valid entries and rejects",
			from:      "alice@example.com",
			allowlist: ",,,",
			want:      false,
		},
		{
			name:      "malformed from falls back to raw equality — match",
			from:      "random garbage",
			allowlist: "random garbage",
			want:      true,
		},
		{
			name:      "malformed from falls back to raw equality — case-insensitive",
			from:      "Random Garbage",
			allowlist: "random garbage",
			want:      true,
		},
		{
			name:      "malformed from does not match unrelated entry",
			from:      "random garbage",
			allowlist: "alice@example.com, @bescom.co.in",
			want:      false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := emailwatch.MatchFromAllowlist(tc.from, tc.allowlist)
			if got != tc.want {
				t.Fatalf("MatchFromAllowlist(from=%q, allowlist=%q) = %v, want %v",
					tc.from, tc.allowlist, got, tc.want)
			}
		})
	}
}
