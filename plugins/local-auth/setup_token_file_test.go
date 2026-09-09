package localauth

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func privateSetupTokenPath(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the Linux launcher uses Unix runtime-directory permissions")
	}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return filepath.Join(dir, "setup-token")
}

func TestSetupTokenFileLifecycle(t *testing.T) {
	path := privateSetupTokenPath(t)
	base := openTestPlugin(t)
	var logs bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&logs, nil))
	p, err := NewWithOptions(t.Context(), base.db, log, false, false, Options{SetupTokenFile: path})
	if err != nil {
		t.Fatal(err)
	}
	token := p.SetupToken()
	assertTokenFile := func(want string) {
		t.Helper()
		contents, err := os.ReadFile(path)
		if err != nil || string(contents) != want+"\n" {
			t.Fatalf("token file disagrees with in-memory token: %v", err)
		}
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("token file is not private: %v, %v", info, err)
		}
		if strings.Contains(logs.String(), want) {
			t.Fatal("file-based setup leaked its token into logs")
		}
	}
	assertTokenFile(token)
	if _, err := p.applySetup(t.Context(), SetupRequest{Token: "wrong", Email: "owner@example.com", Password: "long-enough"}); err != errSetupBadToken {
		t.Fatalf("setup bypassed token verification: %v", err)
	}
	p.setupTokenIssuedAt = time.Now().Add(-SetupTokenTTL)
	if _, err := p.applySetup(t.Context(), SetupRequest{Token: token, Email: "owner@example.com", Password: "long-enough"}); err != errSetupExpiredFile {
		t.Fatalf("expired file-token error: %v", err)
	}
	replacement := p.SetupToken()
	if replacement == token || replacement == "" {
		t.Fatal("expired token was not replaced")
	}
	assertTokenFile(replacement)
	if _, err := p.applySetup(t.Context(), SetupRequest{Token: replacement, Email: "owner@example.com", Password: "long-enough"}); err != nil {
		t.Fatal(err)
	}
	if p.SetupToken() != "" {
		t.Fatal("successful setup did not consume token")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("consumed token file remains: %v", err)
	}
	// A crashed previous process must not leave a misleading bootstrap token.
	if err := os.WriteFile(path, []byte("stale token"), 0o600); err != nil {
		t.Fatal(err)
	}
	restarted, err := NewWithOptions(t.Context(), base.db, log, false, false, Options{SetupTokenFile: path})
	if err != nil || restarted.SetupToken() != "" {
		t.Fatalf("initialized restart: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("initialized restart retained stale file: %v", err)
	}
}

func TestSetupTokenFileDevAndDisabledCleanup(t *testing.T) {
	for _, disabled := range []bool{false, true} {
		t.Run(map[bool]string{false: "dev admin", true: "disabled setup"}[disabled], func(t *testing.T) {
			path := privateSetupTokenPath(t)
			p := openTestPluginWithOptions(t, Options{SetupTokenFile: path})
			if disabled {
				if _, err := NewWithOptions(t.Context(), p.db, p.log, false, false, Options{DisableSetup: true, SetupTokenFile: path}); err != nil {
					t.Fatal(err)
				}
			} else if err := p.EnsureDevAdmin(t.Context(), DevAdminEmail, DevAdminPassword); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Fatalf("obsolete token file remains: %v", err)
			}
		})
	}
}

func TestSetupTokenFileRejectsUnsafeDirectoryAndFailedPublication(t *testing.T) {
	for _, failure := range []string{"relative path", "missing parent", "public parent", "symlink parent", "directory target"} {
		t.Run(failure, func(t *testing.T) {
			path := privateSetupTokenPath(t)
			dir := filepath.Dir(path)
			switch failure {
			case "relative path":
				path = "relative-setup-token"
			case "missing parent":
				path = filepath.Join(dir, "missing", "setup-token")
			case "public parent":
				if err := os.Chmod(dir, 0o755); err != nil {
					t.Fatal(err)
				}
			case "symlink parent":
				link := filepath.Join(t.TempDir(), "linked-runtime")
				if err := os.Symlink(dir, link); err != nil {
					t.Fatal(err)
				}
				path = filepath.Join(link, "setup-token")
			case "directory target":
				if err := os.Mkdir(path, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			base := openTestPlugin(t)
			var logs bytes.Buffer
			log := slog.New(slog.NewJSONHandler(&logs, nil))
			p, err := NewWithOptions(t.Context(), base.db, log, false, false, Options{SetupTokenFile: path})
			if err == nil || p != nil {
				t.Fatalf("unsafe/failed publication accepted: %v", err)
			}
			if strings.Contains(logs.String(), "token_minted") {
				t.Fatal("failed file publication fell back to logging a token")
			}
			if failure == "directory target" {
				if _, err := NewWithOptions(t.Context(), base.db, log, false, false, Options{DisableSetup: true, SetupTokenFile: path}); err == nil {
					t.Fatal("stale cleanup accepted a directory as a token file")
				}
				if info, err := os.Stat(path); err != nil || !info.IsDir() {
					t.Fatalf("stale cleanup removed an unrelated directory: %v", err)
				}
			}
			entries, err := os.ReadDir(dir)
			if err != nil {
				t.Fatal(err)
			}
			for _, entry := range entries {
				if strings.HasPrefix(entry.Name(), ".setup-token-") {
					t.Fatal("failed publication leaked temporary token file")
				}
			}
		})
	}
}

func TestSetupTokenFileDoesNotFollowTargetSymlink(t *testing.T) {
	path := privateSetupTokenPath(t)
	target := filepath.Join(t.TempDir(), "unrelated-file")
	if err := os.WriteFile(target, []byte("unchanged"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
	p := openTestPluginWithOptions(t, Options{SetupTokenFile: path})
	contents, err := os.ReadFile(target)
	if err != nil || string(contents) != "unchanged" {
		t.Fatalf("token publication changed symlink target: %v", err)
	}
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		t.Fatalf("token was not published as private regular file: %v, %v", info, err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
	if err := p.removeSetupTokenFile(); err != nil {
		t.Fatal(err)
	}
	contents, err = os.ReadFile(target)
	if err != nil || string(contents) != "unchanged" {
		t.Fatalf("token cleanup changed symlink target: %v", err)
	}
}

func TestSetupTokenFileRotationFailureKeepsSetupClosed(t *testing.T) {
	path := privateSetupTokenPath(t)
	p := openTestPluginWithOptions(t, Options{SetupTokenFile: path})
	token := p.SetupToken()
	p.setupTokenIssuedAt = time.Now().Add(-SetupTokenTTL)
	if err := os.Chmod(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := p.applySetup(t.Context(), SetupRequest{Token: token, Email: "owner@example.com", Password: "long-enough"}); err != errSetupMint {
		t.Fatalf("rotation failure did not reject setup: %v", err)
	}
	empty, err := usersEmpty(t.Context(), p.db)
	if err != nil || !empty {
		t.Fatalf("failed token rotation created an account: %v", err)
	}
}

func TestSetupTokenFileCleanupFailureDoesNotUndoAccountCreation(t *testing.T) {
	path := privateSetupTokenPath(t)
	p := openTestPluginWithOptions(t, Options{SetupTokenFile: path})
	var logs bytes.Buffer
	p.log = slog.New(slog.NewJSONHandler(&logs, nil))
	token := p.SetupToken()
	if err := os.Chmod(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := p.applySetup(t.Context(), SetupRequest{Token: token, Email: "owner@example.com", Password: "long-enough"}); err != nil {
		t.Fatalf("file cleanup failure reported successful account creation as failed: %v", err)
	}
	if p.SetupToken() != "" {
		t.Fatal("token remained valid after account creation")
	}
	if !strings.Contains(logs.String(), "token_cleanup_failed") || strings.Contains(logs.String(), token) {
		t.Fatal("cleanup failure was not reported safely")
	}
}
