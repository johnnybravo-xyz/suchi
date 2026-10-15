// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestReadyFilePublicationAndMatchingCleanup(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "ready.json")
	record := readyFileRecord{
		Version:       readyFileVersion,
		Nonce:         "launcher-nonce",
		PID:           os.Getpid(),
		PublicURL:     "http://127.0.0.1:8080",
		BuildVersion:  "test-version",
		BuildRevision: "test-revision",
	}
	if err := publishReadyFile(path, record); err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		t.Fatalf("ready file mode = %v, want regular 0600", info.Mode())
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var got readyFileRecord
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, record) {
		t.Fatalf("ready record = %+v, want %+v", got, record)
	}

	mismatch := record
	mismatch.Nonce = "different-launch"
	if err := removeMatchingReadyFile(path, mismatch); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("mismatched cleanup removed ready file: %v", err)
	}
	if err := removeMatchingReadyFile(path, record); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("matching cleanup stat error = %v, want not exist", err)
	}
}

func TestReadyFileRejectsUnsafeRuntimeDirectory(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	record := readyFileRecord{Version: readyFileVersion, Nonce: "nonce", PID: 1, PublicURL: "http://127.0.0.1"}
	if err := publishReadyFile(filepath.Join(dir, "ready.json"), record); err == nil || !strings.Contains(err.Error(), "mode 0700") {
		t.Fatalf("publish error = %v, want private directory rejection", err)
	}
	if err := publishReadyFile("relative.json", record); err == nil || !strings.Contains(err.Error(), "absolute") {
		t.Fatalf("relative publish error = %v, want absolute path rejection", err)
	}
}

func TestParseServeOptionsRequiresCompleteReadyProtocol(t *testing.T) {
	dir := t.TempDir()
	ready := filepath.Join(dir, "ready.json")
	setup := filepath.Join(dir, "setup-token")
	got, err := parseServeOptions([]string{
		"--setup-token-file", setup,
		"--ready-file", ready,
		"--ready-nonce", "launch-1",
		"--exit-on-stdin-close",
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.setupTokenFile != setup || got.readyFile != ready || got.readyNonce != "launch-1" || !got.exitOnStdinClose {
		t.Fatalf("serve options = %+v", got)
	}

	for name, args := range map[string][]string{
		"missing nonce":       {"--ready-file", ready},
		"missing file":        {"--ready-nonce", "launch-1"},
		"relative setup":      {"--setup-token-file", "setup-token"},
		"same file":           {"--setup-token-file", ready, "--ready-file", ready, "--ready-nonce", "launch-1"},
		"invalid nonce":       {"--ready-file", ready, "--ready-nonce", " launch-1"},
		"positional argument": {"unexpected"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := parseServeOptions(args); err == nil {
				t.Fatal("parseServeOptions succeeded, want error")
			}
		})
	}
}
