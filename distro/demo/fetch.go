package demo

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	maxCorpusArchiveBytes   = 256 << 20
	maxCorpusExtractedBytes = 1 << 30
	maxCorpusEntries        = 10_000
	maxChecksumBytes        = 4 << 10
)

// FetchOptions controls where the corpus tarball comes from and
// where it lands on disk after verification + extraction.
type FetchOptions struct {
	// LocalFile — pre-downloaded tarball on disk. Skips HTTP entirely.
	// This is the common path in production (the demo container's
	// Dockerfile bakes the tarball in and passes --corpus-file).
	LocalFile string

	// URL — HTTPS fetch endpoint. Ignored if LocalFile is set.
	// Empty => the corpus release tested with this Suchi build.
	URL string

	// ExpectedSHA256 — hex-encoded sha256. If set, the downloaded /
	// local tarball is verified against it. If empty and a URL is
	// given, the fetcher tries to grab <URL>.sha256 as a sidecar.
	ExpectedSHA256 string

	// CacheDir — where the extracted corpus lands. Defaults to
	// $XDG_CACHE_HOME/suchi/demo-corpus-<version>/ or
	// $HOME/.cache/suchi/demo-corpus-<version>/.
	CacheDir string

	// Version — corpus version string, used only to name the cache
	// subdirectory. Defaults to DemoCorpusVersion.
	Version string
}

// DefaultCorpusURL returns the release asset tested with this build.
func DefaultCorpusURL(version string) string {
	return fmt.Sprintf("https://github.com/johnnybravo-xyz/suchi-demo/releases/download/corpus-%s/corpus-%s.tar.gz", version, version)
}

// Fetch resolves the corpus tarball (local or HTTPS), verifies its
// sha256 if one is available, extracts into CacheDir if the cache is
// cold, and returns the extracted path. Idempotent — a warm cache
// with matching sha short-circuits.
func Fetch(ctx context.Context, opts FetchOptions) (string, error) {
	if opts.Version == "" {
		opts.Version = DemoCorpusVersion
	}
	if opts.CacheDir == "" {
		base, err := defaultCacheBase()
		if err != nil {
			return "", err
		}
		opts.CacheDir = filepath.Join(base, "suchi", "demo-corpus-"+opts.Version)
	}
	if opts.LocalFile == "" && opts.URL == "" {
		opts.URL = DefaultCorpusURL(opts.Version)
	}

	// Warm cache short-circuit: manifest.json exists AND (no sha
	// provided OR .sha256 marker matches).
	manifestPath := filepath.Join(opts.CacheDir, "manifest.json")
	if _, err := os.Stat(manifestPath); err == nil {
		markerPath := opts.CacheDir + ".sha256"
		if opts.ExpectedSHA256 == "" {
			return opts.CacheDir, nil
		}
		if b, err := os.ReadFile(markerPath); err == nil &&
			strings.TrimSpace(string(b)) == opts.ExpectedSHA256 {
			return opts.CacheDir, nil
		}
	}

	// Materialize the tarball bytes.
	var tarBytes []byte
	switch {
	case opts.LocalFile != "":
		info, err := os.Stat(opts.LocalFile)
		if err != nil {
			return "", fmt.Errorf("stat local tarball: %w", err)
		}
		if info.Size() > maxCorpusArchiveBytes {
			return "", fmt.Errorf("local tarball exceeds %d-byte limit", maxCorpusArchiveBytes)
		}
		b, err := os.ReadFile(opts.LocalFile)
		if err != nil {
			return "", fmt.Errorf("read local tarball: %w", err)
		}
		tarBytes = b
	case opts.URL != "":
		b, err := httpGet(ctx, opts.URL, maxCorpusArchiveBytes)
		if err != nil {
			return "", fmt.Errorf("fetch %s: %w", opts.URL, err)
		}
		tarBytes = b
		if opts.ExpectedSHA256 == "" {
			// Try to grab the sidecar; a 404 is not fatal.
			if s, err := httpGet(ctx, opts.URL+".sha256", maxChecksumBytes); err == nil {
				opts.ExpectedSHA256 = firstHexToken(string(s))
			}
		}
	}

	// Verify sha if we have one.
	if opts.ExpectedSHA256 != "" {
		sum := sha256.Sum256(tarBytes)
		got := hex.EncodeToString(sum[:])
		if got != opts.ExpectedSHA256 {
			return "", fmt.Errorf("sha256 mismatch: want %s got %s",
				opts.ExpectedSHA256, got)
		}
	}

	// Extract into a temp sibling of CacheDir, then atomically rename.
	tmp := opts.CacheDir + ".partial"
	if err := os.RemoveAll(tmp); err != nil {
		return "", err
	}
	if err := os.MkdirAll(tmp, 0o755); err != nil {
		return "", err
	}
	if err := extractTarGz(tarBytes, tmp); err != nil {
		return "", fmt.Errorf("extract: %w", err)
	}
	if err := os.RemoveAll(opts.CacheDir); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, opts.CacheDir); err != nil {
		return "", err
	}
	if opts.ExpectedSHA256 != "" {
		if err := os.WriteFile(opts.CacheDir+".sha256", []byte(opts.ExpectedSHA256), 0o644); err != nil {
			return "", err
		}
	}
	return opts.CacheDir, nil
}

func defaultCacheBase() (string, error) {
	if v := os.Getenv("XDG_CACHE_HOME"); v != "" {
		return v, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".cache"), nil
}

func httpGet(ctx context.Context, url string, maxBytes int64) ([]byte, error) {
	c := &http.Client{Timeout: 5 * time.Minute}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("http %d", resp.StatusCode)
	}
	if resp.ContentLength > maxBytes {
		return nil, fmt.Errorf("response exceeds %d-byte limit", maxBytes)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > maxBytes {
		return nil, fmt.Errorf("response exceeds %d-byte limit", maxBytes)
	}
	return b, nil
}

func firstHexToken(s string) string {
	for _, f := range strings.Fields(s) {
		if isHex(f) && len(f) == 64 {
			return f
		}
	}
	return ""
}

func isHex(s string) bool {
	for _, r := range s {
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F')) {
			return false
		}
	}
	return true
}

func extractTarGz(data []byte, dst string) error {
	gz, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	var total int64
	entries := 0
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		entries++
		if entries > maxCorpusEntries {
			return fmt.Errorf("archive exceeds %d-entry limit", maxCorpusEntries)
		}
		if h.Size < 0 || h.Size > maxCorpusExtractedBytes-total {
			return fmt.Errorf("archive exceeds %d-byte extraction limit", maxCorpusExtractedBytes)
		}
		total += h.Size
		// Guard against traversal.
		clean := filepath.Clean(h.Name)
		if clean == ".." || strings.HasPrefix(clean, ".."+string(os.PathSeparator)) || filepath.IsAbs(clean) {
			return fmt.Errorf("unsafe tar entry: %s", h.Name)
		}
		target := filepath.Join(dst, clean)
		switch h.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			f, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
			if err != nil {
				return err
			}
			if _, err := io.Copy(f, tr); err != nil {
				_ = f.Close()
				return err
			}
			if err := f.Close(); err != nil {
				return err
			}
		default:
			// Skip symlinks + others: the demo corpus is regular files only.
		}
	}
}
