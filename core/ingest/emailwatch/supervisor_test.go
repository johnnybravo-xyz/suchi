package emailwatch

import (
	"testing"

	"github.com/johnnybravo-xyz/suchi/core/emailaccounts"
)

func TestFingerprintIgnoresSyncBookkeeping(t *testing.T) {
	a := &emailaccounts.Account{
		OwnerID: 1, Provider: emailaccounts.ProviderCustom,
		Host: "mail.example.com", Port: 993, UseTLS: true,
		Folder: "INBOX", PollIntervalMin: 10,
		AuthMethod: emailaccounts.AuthPassword, Username: "alice",
		SealedSecret: []byte("sealed"), Enabled: true,
	}
	want := fingerprint(a)
	a.LastUIDSeen = 42
	a.UIDValiditySeen = 7
	a.LastSyncAt = 1234
	a.LastError = "temporary failure"
	a.UpdatedAt = 5678
	if got := fingerprint(a); got != want {
		t.Fatal("sync bookkeeping changed the runtime fingerprint")
	}
}

func TestFingerprintIncludesRuntimePolicyAndSecret(t *testing.T) {
	base := &emailaccounts.Account{
		OwnerID: 1, Provider: emailaccounts.ProviderCustom,
		Host: "mail.example.com", Port: 993, UseTLS: true,
		Folder: "INBOX", PollIntervalMin: 10,
		AuthMethod: emailaccounts.AuthPassword, Username: "alice",
		SealedSecret: []byte("sealed"), Enabled: true,
	}
	wantDifferent := []*emailaccounts.Account{
		func() *emailaccounts.Account { c := *base; c.ProcessedFolder = "Processed"; return &c }(),
		func() *emailaccounts.Account { c := *base; c.MarkSeen = true; return &c }(),
		func() *emailaccounts.Account { c := *base; c.FromAllowlist = "@example.com"; return &c }(),
		func() *emailaccounts.Account { c := *base; c.SealedSecret = []byte("rotated"); return &c }(),
	}
	for i, changed := range wantDifferent {
		if fingerprint(changed) == fingerprint(base) {
			t.Fatalf("runtime change %d did not change fingerprint", i)
		}
	}
}
