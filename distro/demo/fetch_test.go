// SPDX-License-Identifier: AGPL-3.0-or-later

package demo_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/johnnybravo-xyz/suchi/distro/demo"
)

func TestFetchRequiresExactlyOneSource(t *testing.T) {
	for _, test := range []struct {
		name string
		opts demo.FetchOptions
	}{
		{name: "missing"},
		{name: "local and URL", opts: demo.FetchOptions{
			LocalFile: "/tmp/corpus.tar.gz",
			URL:       "https://example.test/corpus.tar.gz",
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := demo.Fetch(context.Background(), test.opts)
			if err == nil || !strings.Contains(err.Error(), "exactly one of LocalFile and URL") {
				t.Fatalf("Fetch error = %v", err)
			}
		})
	}
}

func TestFetchVerifiedWarmCacheRequiresMatchingMarker(t *testing.T) {
	root := t.TempDir()
	cache := filepath.Join(root, "corpus")
	checksum := strings.Repeat("a", 64)
	primeCache(t, cache, `{"name":"verified"}`, checksum)

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	got, err := demo.Fetch(cancelled, demo.FetchOptions{
		CacheDir:       cache,
		LocalFile:      filepath.Join(root, "not-read.tar.gz"),
		ExpectedSHA256: checksum,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got != cache {
		t.Fatalf("Fetch returned %q, want %q", got, cache)
	}
}

func TestFetchUnverifiedLocalFileReplacesWarmCacheAndClearsMarker(t *testing.T) {
	root := t.TempDir()
	cache := filepath.Join(root, "corpus")
	primeCache(t, cache, `{"name":"warm"}`, strings.Repeat("a", 64))
	archive := corpusArchive(t, `{"name":"custom-file"}`)
	archivePath := filepath.Join(root, "custom.tar.gz")
	if err := os.WriteFile(archivePath, archive, 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := demo.Fetch(context.Background(), demo.FetchOptions{
		CacheDir:  cache,
		LocalFile: archivePath,
	}); err != nil {
		t.Fatal(err)
	}
	assertManifest(t, cache, `{"name":"custom-file"}`)
	assertNoChecksumMarker(t, cache)
}

func TestFetchUnverifiedURLReplacesWarmCacheAndClearsMarker(t *testing.T) {
	cache := filepath.Join(t.TempDir(), "corpus")
	primeCache(t, cache, `{"name":"warm"}`, strings.Repeat("a", 64))
	archive := corpusArchive(t, `{"name":"custom-url"}`)
	var archiveRequests atomic.Int32
	var sidecarRequests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/corpus.tar.gz":
			archiveRequests.Add(1)
			_, _ = w.Write(archive)
		case "/corpus.tar.gz.sha256":
			sidecarRequests.Add(1)
			http.NotFound(w, r)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)

	if _, err := demo.Fetch(context.Background(), demo.FetchOptions{
		CacheDir: cache,
		URL:      server.URL + "/corpus.tar.gz",
	}); err != nil {
		t.Fatal(err)
	}
	if got := sidecarRequests.Load(); got != 1 {
		t.Fatalf("sidecar requests = %d, want 1", got)
	}
	if got := archiveRequests.Load(); got != 1 {
		t.Fatalf("archive requests = %d, want 1", got)
	}
	assertManifest(t, cache, `{"name":"custom-url"}`)
	assertNoChecksumMarker(t, cache)
}

func TestFetchResolvesURLSidecarBeforeWarmCacheReuse(t *testing.T) {
	cache := filepath.Join(t.TempDir(), "corpus")
	archive := corpusArchive(t, `{"name":"verified"}`)
	checksum := archiveChecksum(archive)
	primeCache(t, cache, `{"name":"warm"}`, checksum)
	var archiveRequests atomic.Int32
	var sidecarRequests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/corpus.tar.gz":
			archiveRequests.Add(1)
			_, _ = w.Write(archive)
		case "/corpus.tar.gz.sha256":
			sidecarRequests.Add(1)
			_, _ = io.WriteString(w, strings.ToUpper(checksum)+"  corpus.tar.gz\n")
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)

	if _, err := demo.Fetch(context.Background(), demo.FetchOptions{
		CacheDir: cache,
		URL:      server.URL + "/corpus.tar.gz",
	}); err != nil {
		t.Fatal(err)
	}
	if got := sidecarRequests.Load(); got != 1 {
		t.Fatalf("sidecar requests = %d, want 1", got)
	}
	if got := archiveRequests.Load(); got != 0 {
		t.Fatalf("archive requests = %d, want warm-cache reuse", got)
	}
	assertManifest(t, cache, `{"name":"warm"}`)
}

func TestFetchVerifiedReplacementWritesMarker(t *testing.T) {
	root := t.TempDir()
	cache := filepath.Join(root, "corpus")
	primeCache(t, cache, `{"name":"stale"}`, strings.Repeat("a", 64))
	archive := corpusArchive(t, `{"name":"verified"}`)
	checksum := archiveChecksum(archive)
	archivePath := filepath.Join(root, "verified.tar.gz")
	if err := os.WriteFile(archivePath, archive, 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := demo.Fetch(context.Background(), demo.FetchOptions{
		CacheDir:       cache,
		LocalFile:      archivePath,
		ExpectedSHA256: checksum,
	}); err != nil {
		t.Fatal(err)
	}
	assertManifest(t, cache, `{"name":"verified"}`)
	marker, err := os.ReadFile(cache + ".sha256")
	if err != nil {
		t.Fatal(err)
	}
	if got := string(marker); got != checksum {
		t.Fatalf("checksum marker = %q, want %q", got, checksum)
	}
}

func TestFetchRejectsChecksumMismatch(t *testing.T) {
	root := t.TempDir()
	archivePath := filepath.Join(root, "corpus.tar.gz")
	if err := os.WriteFile(archivePath, corpusArchive(t, `{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := demo.Fetch(context.Background(), demo.FetchOptions{
		CacheDir:       filepath.Join(root, "cache"),
		LocalFile:      archivePath,
		ExpectedSHA256: strings.Repeat("0", 64),
	})
	if err == nil || !strings.Contains(err.Error(), "sha256 mismatch") {
		t.Fatalf("Fetch error = %v", err)
	}
}

func TestFetchRejectsOversizedLocalArchive(t *testing.T) {
	root := t.TempDir()
	archivePath := filepath.Join(root, "oversized.tar.gz")
	file, err := os.Create(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Truncate((256 << 20) + 1); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	_, err = demo.Fetch(context.Background(), demo.FetchOptions{
		CacheDir:  filepath.Join(root, "cache"),
		LocalFile: archivePath,
	})
	if err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("Fetch error = %v", err)
	}
}

func TestFetchRejectsUnsafeArchiveEntry(t *testing.T) {
	root := t.TempDir()
	archivePath := filepath.Join(root, "unsafe.tar.gz")
	archive := archiveWithEntry(t, "../outside", []byte("escape"))
	if err := os.WriteFile(archivePath, archive, 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := demo.Fetch(context.Background(), demo.FetchOptions{
		CacheDir:  filepath.Join(root, "cache"),
		LocalFile: archivePath,
	})
	if err == nil || !strings.Contains(err.Error(), "unsafe tar entry") {
		t.Fatalf("Fetch error = %v", err)
	}
}

func corpusArchive(t *testing.T, manifest string) []byte {
	t.Helper()
	return archiveWithEntry(t, "manifest.json", []byte(manifest))
}

func archiveWithEntry(t *testing.T, name string, content []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{
		Name: name,
		Mode: 0o644,
		Size: int64(len(content)),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(content); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func archiveChecksum(archive []byte) string {
	sum := sha256.Sum256(archive)
	return hex.EncodeToString(sum[:])
}

func primeCache(t *testing.T, cache, manifest, checksum string) {
	t.Helper()
	if err := os.Mkdir(cache, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cache, "manifest.json"), []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cache+".sha256", []byte(checksum), 0o600); err != nil {
		t.Fatal(err)
	}
}

func assertManifest(t *testing.T, cache, want string) {
	t.Helper()
	got, err := os.ReadFile(filepath.Join(cache, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != want {
		t.Fatalf("manifest = %q, want %q", got, want)
	}
}

func assertNoChecksumMarker(t *testing.T, cache string) {
	t.Helper()
	if _, err := os.Stat(cache + ".sha256"); !os.IsNotExist(err) {
		t.Fatalf("checksum marker still exists: %v", err)
	}
}
