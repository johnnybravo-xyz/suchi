package pipeconfig

import (
	"os"
	"testing"
	"time"
)

// TestFileChainReachesFuncs proves the "config file → env → default"
// chain reaches the pipeline func-defaults. Package-init `var =` would
// fail this because it captures env before LoadFile-style Setenv.
func TestFileChainReachesFuncs(t *testing.T) {
	const key = "SUCHI_TEST_PIPECONFIG_CHAIN"
	// Simulate LoadFile setting env at runtime AFTER package init has
	// already happened. If Duration reads at call time, this must land.
	os.Setenv(key, "7m")
	defer os.Unsetenv(key)
	got := Duration(key, 1*time.Second)
	if got != 7*time.Minute {
		t.Fatalf("chain broken: got %v want 7m", got)
	}
}

func TestDuration(t *testing.T) {
	const key = "SUCHI_TEST_PIPECONFIG_DURATION"

	t.Run("unset returns fallback", func(t *testing.T) {
		t.Setenv(key, "")
		if got := Duration(key, 30*time.Second); got != 30*time.Second {
			t.Fatalf("got %v want 30s", got)
		}
	})
	t.Run("valid parses", func(t *testing.T) {
		t.Setenv(key, "2m30s")
		if got := Duration(key, 30*time.Second); got != 2*time.Minute+30*time.Second {
			t.Fatalf("got %v want 2m30s", got)
		}
	})
	t.Run("invalid falls back and warns", func(t *testing.T) {
		t.Setenv(key, "not-a-duration")
		if got := Duration(key, 45*time.Second); got != 45*time.Second {
			t.Fatalf("got %v want 45s", got)
		}
	})
}

func TestBytes(t *testing.T) {
	const key = "SUCHI_TEST_PIPECONFIG_BYTES"

	t.Run("unset returns fallback", func(t *testing.T) {
		t.Setenv(key, "")
		if got := Bytes(key, 100*1024*1024); got != 100*1024*1024 {
			t.Fatalf("got %d want 100MiB", got)
		}
	})
	t.Run("raw bytes parses", func(t *testing.T) {
		t.Setenv(key, "12345")
		if got := Bytes(key, 0); got != 12345 {
			t.Fatalf("got %d want 12345", got)
		}
	})
	t.Run("M suffix parses", func(t *testing.T) {
		t.Setenv(key, "250M")
		if got := Bytes(key, 0); got != 250*1024*1024 {
			t.Fatalf("got %d want 250MiB", got)
		}
	})
	t.Run("invalid falls back", func(t *testing.T) {
		t.Setenv(key, "not-a-size")
		if got := Bytes(key, 42); got != 42 {
			t.Fatalf("got %d want 42", got)
		}
	})
}
