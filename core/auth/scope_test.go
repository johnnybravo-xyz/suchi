package auth

import (
	"testing"

	pluginapi "github.com/johnnybravo-xyz/suchi/plugin-api"
)

func TestHasScope_NilPrincipal(t *testing.T) {
	if HasScope(nil, ScopeDocumentsWrite) {
		t.Fatal("nil principal must never pass")
	}
}

func TestHasScope_SessionUserAlwaysAllowed(t *testing.T) {
	p := &pluginapi.Principal{Kind: "user"}
	for _, s := range []string{
		ScopeDocumentsRead, ScopeDocumentsWrite,
	} {
		if !HasScope(p, s) {
			t.Errorf("session user should have %s", s)
		}
	}
}

func TestHasScope_DemoAnonAlwaysAllowed(t *testing.T) {
	// httpx.DemoReadOnly filters out demo-anon writes before this
	// layer, so demo-anon reaching HasScope is a read the anon
	// visitor is entitled to. Same short-circuit as Kind="user".
	p := &pluginapi.Principal{Kind: "demo-anon"}
	for _, s := range []string{ScopeDocumentsRead, ScopeEventsRead} {
		if !HasScope(p, s) {
			t.Errorf("demo-anon should have %s", s)
		}
	}
}

func TestHasScope_ExactTokenMatch(t *testing.T) {
	p := &pluginapi.Principal{Kind: "token", Scopes: []string{ScopeDocumentsWrite}}
	if !HasScope(p, ScopeDocumentsWrite) {
		t.Fatal("exact match should pass")
	}
	if HasScope(p, ScopeEventsRead) {
		t.Fatal("token without events:read must fail")
	}
}

func TestHasScope_CoarseScopesAreNotExpanded(t *testing.T) {
	p := &pluginapi.Principal{Kind: "token", Scopes: []string{"read", "write"}}
	for _, need := range []string{
		ScopeDocumentsRead, ScopeDocumentsWrite,
		ScopeEventsRead,
	} {
		if HasScope(p, need) {
			t.Errorf("coarse scopes must not grant %s", need)
		}
	}
}

func TestIsKnownScope(t *testing.T) {
	for _, scope := range []string{
		ScopeDocumentsRead, ScopeDocumentsWrite, ScopeEventsRead,
	} {
		if !IsKnownScope(scope) {
			t.Errorf("scope %q should be known", scope)
		}
	}
	for _, scope := range []string{"", "read", "write", "documents:*"} {
		if IsKnownScope(scope) {
			t.Errorf("scope %q should be rejected", scope)
		}
	}
}
