package main

import (
	"runtime/debug"
	"testing"
)

func TestBuildIdentity(t *testing.T) {
	oldVersion, oldRevision := version, revision
	t.Cleanup(func() { version, revision = oldVersion, oldRevision })
	local := &debug.BuildInfo{
		Main: debug.Module{Version: "(devel)"},
		Settings: []debug.BuildSetting{
			{Key: "vcs.revision", Value: "1234567890abcdef1234567890abcdef12345678"},
		},
	}
	dirty := &debug.BuildInfo{Main: local.Main, Settings: append([]debug.BuildSetting{
		{Key: "vcs.modified", Value: "true"},
	}, local.Settings...)}
	for _, tc := range []struct {
		name, version, revision string
		info                    *debug.BuildInfo
		wantVersion, wantRev    string
		wantCLI                 string
	}{
		{name: "unknown", wantVersion: "dev", wantCLI: "dev"},
		{name: "local", info: local, wantVersion: "dev", wantRev: "1234567890ab", wantCLI: "dev+1234567890ab"},
		{name: "dirty", info: dirty, wantVersion: "dev", wantRev: "1234567890ab.dirty", wantCLI: "dev+1234567890ab.dirty"},
		{name: "release", version: "v0.1.0-beta.2", info: local, wantVersion: "v0.1.0-beta.2", wantRev: "1234567890ab", wantCLI: "v0.1.0-beta.2"},
		{name: "container", version: "v0.1.0-beta.2", revision: "abcdef1234567890", wantVersion: "v0.1.0-beta.2", wantRev: "abcdef123456", wantCLI: "v0.1.0-beta.2"},
		{name: "explicit revision", revision: "abcdef", info: local, wantVersion: "dev", wantRev: "abcdef", wantCLI: "dev+abcdef"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			version, revision = tc.version, tc.revision
			v, rev := buildIdentity(tc.info)
			if v != tc.wantVersion || rev != tc.wantRev {
				t.Fatalf("build identity = %q, %q; want %q, %q", v, rev, tc.wantVersion, tc.wantRev)
			}
			if got := buildVersion(tc.info); got != tc.wantCLI {
				t.Fatalf("CLI version = %q; want %q", got, tc.wantCLI)
			}
		})
	}
}
