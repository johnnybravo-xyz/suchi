package auth

import (
	"testing"

	pluginapi "github.com/suchi-dms/suchi/plugin-api"
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
		ScopeAgentTasks, ScopeAdminWebhooks,
	} {
		if !HasScope(p, s) {
			t.Errorf("session user should have %s", s)
		}
	}
}

func TestHasScope_ExactTokenMatch(t *testing.T) {
	p := &pluginapi.Principal{Kind: "token", Scopes: []string{ScopeDocumentsWrite}}
	if !HasScope(p, ScopeDocumentsWrite) {
		t.Fatal("exact match should pass")
	}
	if HasScope(p, ScopeAgentTasks) {
		t.Fatal("token without agent:tasks must fail")
	}
}

func TestHasScope_LegacyWriteIsWildcard(t *testing.T) {
	p := &pluginapi.Principal{Kind: "token", Scopes: []string{"read", "write"}}
	for _, need := range []string{
		ScopeDocumentsRead, ScopeDocumentsWrite,
		ScopeAgentTasks, ScopeAdminWebhooks,
	} {
		if !HasScope(p, need) {
			t.Errorf("legacy write should grant %s", need)
		}
	}
}

func TestHasScope_LegacyReadIsReadOnly(t *testing.T) {
	p := &pluginapi.Principal{Kind: "token", Scopes: []string{"read"}}
	if !HasScope(p, ScopeDocumentsRead) {
		t.Fatal("legacy read should grant documents:read")
	}
	if HasScope(p, ScopeDocumentsWrite) {
		t.Fatal("legacy read must NOT grant documents:write")
	}
}
