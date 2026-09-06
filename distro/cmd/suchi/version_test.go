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
	pseudo := &debug.BuildInfo{
		Main:     debug.Module{Version: "v0.1.0-beta.1.0.20260906185741-1234567890ab"},
		Settings: local.Settings,
	}
	dirtyPseudo := &debug.BuildInfo{Main: pseudo.Main, Settings: dirty.Settings}
	for _, tc := range []struct {
		name, version, revision string
		info                    *debug.BuildInfo
		wantVersion, wantRev    string
		wantCLI                 string
	}{
		{name: "unknown", wantVersion: developmentVersion, wantCLI: developmentVersion},
		{name: "no vcs", info: &debug.BuildInfo{Main: local.Main}, wantVersion: developmentVersion, wantCLI: developmentVersion},
		{name: "local", info: local, wantVersion: developmentVersion, wantRev: "1234567890ab", wantCLI: developmentVersion + "+1234567890ab"},
		{name: "dirty", info: dirty, wantVersion: developmentVersion, wantRev: "1234567890ab.dirty", wantCLI: developmentVersion + "+1234567890ab.dirty"},
		{name: "local pseudo-version", info: pseudo, wantVersion: developmentVersion, wantRev: "1234567890ab", wantCLI: developmentVersion + "+1234567890ab"},
		{name: "dirty pseudo-version", info: dirtyPseudo, wantVersion: developmentVersion, wantRev: "1234567890ab.dirty", wantCLI: developmentVersion + "+1234567890ab.dirty"},
		{name: "release overrides pseudo-version", version: "v0.1.0-beta.2", info: pseudo, wantVersion: "v0.1.0-beta.2", wantRev: "1234567890ab", wantCLI: "v0.1.0-beta.2"},
		{name: "explicit revision with pseudo-version", revision: "abcdef", info: pseudo, wantVersion: developmentVersion, wantRev: "abcdef", wantCLI: developmentVersion + "+abcdef"},
		{name: "release", version: "v0.1.0-beta.2", info: local, wantVersion: "v0.1.0-beta.2", wantRev: "1234567890ab", wantCLI: "v0.1.0-beta.2"},
		{name: "dirty release", version: "v0.1.0-beta.2", info: dirty, wantVersion: "v0.1.0-beta.2", wantRev: "1234567890ab.dirty", wantCLI: "v0.1.0-beta.2"},
		{name: "container", version: "v0.1.0-beta.2", revision: "abcdef1234567890", wantVersion: "v0.1.0-beta.2", wantRev: "abcdef123456", wantCLI: "v0.1.0-beta.2"},
		{name: "container dev", version: "dev", revision: "abcdef1234567890", wantVersion: developmentVersion, wantRev: "abcdef123456", wantCLI: developmentVersion + "+abcdef123456"},
		{name: "container dev without revision", version: "dev", wantVersion: developmentVersion, wantCLI: developmentVersion},
		{name: "explicit devel", version: "(devel)", info: local, wantVersion: developmentVersion, wantRev: "1234567890ab", wantCLI: developmentVersion + "+1234567890ab"},
		{name: "explicit development version", version: developmentVersion, info: local, wantVersion: developmentVersion, wantRev: "1234567890ab", wantCLI: developmentVersion + "+1234567890ab"},
		{name: "explicit revision", revision: "abcdef", info: local, wantVersion: developmentVersion, wantRev: "abcdef", wantCLI: developmentVersion + "+abcdef"},
		{name: "module release", info: &debug.BuildInfo{Main: debug.Module{Version: "v0.1.0-beta.2"}}, wantVersion: "v0.1.0-beta.2", wantCLI: "v0.1.0-beta.2"},
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
