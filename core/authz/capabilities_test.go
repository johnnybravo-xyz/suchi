package authz

// Wire-shape guarantees for the per-user capability set. The wire
// side must reject unknowns loudly and dedupe idempotent repeats so
// the admin API can't accidentally silently drop a typo grant.

import (
	"testing"
)

func TestCapabilities_parse_wire(t *testing.T) {
	// Empty input is a valid (member with no caps) empty set.
	set, err := ParseWire(nil)
	if err != nil {
		t.Fatalf("nil input: %v", err)
	}
	if len(set) != 0 {
		t.Fatalf("nil input should produce empty set, got %v", set.SliceStrings())
	}

	set, err = ParseWire([]string{})
	if err != nil {
		t.Fatalf("empty slice: %v", err)
	}
	if len(set) != 0 {
		t.Fatalf("empty slice should produce empty set, got %v", set.SliceStrings())
	}

	// Known slugs pass through.
	set, err = ParseWire([]string{"mailboxes", "share_links"})
	if err != nil {
		t.Fatalf("known slugs: %v", err)
	}
	got := set.SliceStrings()
	if len(got) != 2 || got[0] != "mailboxes" || got[1] != "share_links" {
		t.Fatalf("known slugs projection: got %v", got)
	}

	// Dupes dedupe.
	set, err = ParseWire([]string{"mailboxes", "mailboxes"})
	if err != nil {
		t.Fatalf("dupes: %v", err)
	}
	if len(set) != 1 || !set.Has(CapMailboxes) {
		t.Fatalf("dupes should dedupe to one, got %v", set.SliceStrings())
	}

	// Unknown slug is a loud reject.
	if _, err := ParseWire([]string{"mailboxes", "does_not_exist"}); err == nil {
		t.Fatalf("unknown slug should error, got nil")
	}
}

func TestCapabilities_diff(t *testing.T) {
	prior := NewSet()
	prior.Add(CapMailboxes)

	next := NewSet()
	next.Add(CapShareLinks)

	added, removed := next.Diff(prior)
	if !added.Has(CapShareLinks) || added.Has(CapMailboxes) {
		t.Errorf("added: got %v, want {share_links}", added.SliceStrings())
	}
	if !removed.Has(CapMailboxes) || removed.Has(CapShareLinks) {
		t.Errorf("removed: got %v, want {mailboxes}", removed.SliceStrings())
	}

	// Diff against self is empty.
	added, removed = prior.Diff(prior)
	if len(added) != 0 || len(removed) != 0 {
		t.Errorf("self-diff not empty: added=%v removed=%v",
			added.SliceStrings(), removed.SliceStrings())
	}

	// Empty→populated: everything is added; nothing removed.
	added, removed = next.Diff(NewSet())
	if !added.Has(CapShareLinks) || len(removed) != 0 {
		t.Errorf("empty prior: added=%v removed=%v",
			added.SliceStrings(), removed.SliceStrings())
	}
}
