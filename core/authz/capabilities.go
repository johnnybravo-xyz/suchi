// Per-user capabilities — orthogonal to role.
//
// Roles are coarse (admin / member). Capabilities are the fine-grained,
// admin-grantable feature switches on top of member. Wire shape is a
// JSON array of slugs on users.capabilities; in-memory we round-trip
// through Set to dedupe + validate.
//
// Admins are implicitly capable of everything — role check
// short-circuits before the capability check on the authz path.
//
// Growth: add a Capability const, add it to KnownCapabilities, and
// (optionally) register a revoke hook in core/api/users.go.

package authz

import (
	"encoding/json"
	"errors"
	"slices"
)

type Capability string

const (
	CapMailboxes  Capability = "mailboxes"
	CapShareLinks Capability = "share_links"
	CapShareViews Capability = "share_views"
)

// KnownCapabilities gates what the admin API will accept on the wire.
// Unknown slugs are rejected loudly so a typo doesn't silently grant
// nothing.
var KnownCapabilities = map[Capability]bool{
	CapMailboxes:  true,
	CapShareLinks: true,
	CapShareViews: true,
}

// Set is a value-typed capability set. Zero-value is the empty set
// (a member with no capabilities).
type Set map[Capability]struct{}

func NewSet() Set { return Set{} }

func (s Set) Has(c Capability) bool { _, ok := s[c]; return ok }

func (s Set) Add(c Capability) { s[c] = struct{}{} }

func (s Set) Remove(c Capability) { delete(s, c) }

// Slice returns the caps as a stable, alpha-sorted []Capability so
// the wire projection is deterministic (helpful for tests + audit
// diffs).
func (s Set) Slice() []Capability {
	out := make([]Capability, 0, len(s))
	for c := range s {
		out = append(out, c)
	}
	slices.Sort(out)
	return out
}

// SliceStrings is Slice() as []string — convenience for JSON
// projection where Capability's underlying string is what we want on
// the wire.
func (s Set) SliceStrings() []string {
	xs := s.Slice()
	out := make([]string, len(xs))
	for i, c := range xs {
		out[i] = string(c)
	}
	return out
}

// Diff returns (added, removed) between s (new) and prior (old).
// Used by the admin PATCH path to run revoke hooks on the removed set.
func (s Set) Diff(prior Set) (added, removed Set) {
	added, removed = NewSet(), NewSet()
	for c := range s {
		if !prior.Has(c) {
			added.Add(c)
		}
	}
	for c := range prior {
		if !s.Has(c) {
			removed.Add(c)
		}
	}
	return
}

// ParseWire validates a []string from the API wire and returns a Set
// with unknown slugs rejected. Empty / nil input is a valid empty set.
func ParseWire(raw []string) (Set, error) {
	out := NewSet()
	for _, r := range raw {
		c := Capability(r)
		if !KnownCapabilities[c] {
			return nil, errors.New("unknown capability: " + r)
		}
		out.Add(c)
	}
	return out, nil
}

// ParseJSON decodes users.capabilities column bytes. Empty input
// (never happens with the NOT NULL DEFAULT '[]' migration but be
// defensive) returns an empty set.
func ParseJSON(raw []byte) (Set, error) {
	if len(raw) == 0 {
		return NewSet(), nil
	}
	var xs []string
	if err := json.Unmarshal(raw, &xs); err != nil {
		return nil, err
	}
	out := NewSet()
	for _, r := range xs {
		out.Add(Capability(r)) // no whitelist check on read — tolerate legacy
	}
	return out, nil
}

// MarshalJSON emits the sorted []string projection.
func (s Set) MarshalJSON() ([]byte, error) {
	return json.Marshal(s.SliceStrings())
}
