package authz

// Wire-shape guarantees for the per-user capability set. The wire
// side must reject unknowns loudly and dedupe idempotent repeats so
// the admin API can't accidentally silently drop a typo grant.

import (
	"encoding/json"
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
	set, err = ParseWire([]string{"share_views", "mailboxes", "share_links", "archive_intelligence", "archive_chat"})
	if err != nil {
		t.Fatalf("known slugs: %v", err)
	}
	got := set.SliceStrings()
	if len(got) != 5 || got[0] != "archive_chat" || got[1] != "archive_intelligence" || got[2] != "mailboxes" || got[3] != "share_links" || got[4] != "share_views" {
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

func TestCapabilitiesMarshalAsSortedArray(t *testing.T) {
	set := NewSet()
	set.Add(CapShareLinks)
	set.Add(CapArchiveChat)
	got, err := json.Marshal(set)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != `["archive_chat","share_links"]` {
		t.Fatalf("JSON = %s", got)
	}
}
