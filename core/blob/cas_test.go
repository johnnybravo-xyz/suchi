// SPDX-License-Identifier: AGPL-3.0-or-later

package blob_test

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/johnnybravo-xyz/suchi/core/blob"
	pluginapi "github.com/johnnybravo-xyz/suchi/plugin-api"
)

func TestPutGetStatDelete(t *testing.T) {
	cas, err := blob.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	payload := []byte("hello, suchi")
	want := sha256.Sum256(payload)
	wantHex := hex.EncodeToString(want[:])

	ref, err := cas.Put(bytes.NewReader(payload))
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	if ref.SHA256 != wantHex {
		t.Errorf("hash %q, want %q", ref.SHA256, wantHex)
	}
	if ref.Size != int64(len(payload)) {
		t.Errorf("size %d, want %d", ref.Size, len(payload))
	}

	// Stat must round-trip.
	st, err := cas.Stat(wantHex)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if st != ref {
		t.Errorf("stat mismatch %+v vs %+v", st, ref)
	}

	// Get must round-trip content.
	rc, err := cas.Get(wantHex)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	got, err := io.ReadAll(rc)
	rc.Close()
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Errorf("payload mismatch")
	}
	path, err := cas.Path(wantHex)
	if err != nil {
		t.Fatalf("path: %v", err)
	}
	if !filepath.IsAbs(path) {
		t.Fatalf("blob path is relative: %s", path)
	}
	stored, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(stored, payload) {
		t.Fatalf("read blob path: content = %q, err = %v", stored, err)
	}

	// Duplicate put is a no-op — same ref back.
	ref2, err := cas.Put(bytes.NewReader(payload))
	if err != nil {
		t.Fatalf("re-put: %v", err)
	}
	if ref2 != ref {
		t.Errorf("dedup broken: %+v vs %+v", ref, ref2)
	}

	// Delete is idempotent.
	if err := cas.Delete(wantHex); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if err := cas.Delete(wantHex); err != nil {
		t.Errorf("second delete errored: %v", err)
	}
	if _, err := cas.Get(wantHex); err != blob.ErrNotFound {
		t.Errorf("post-delete get: %v, want ErrNotFound", err)
	}
}

type synchronizedReader struct {
	data    []byte
	offset  int
	ready   *sync.WaitGroup
	release <-chan struct{}
}

func (r *synchronizedReader) Read(p []byte) (int, error) {
	if r.offset < len(r.data) {
		n := copy(p, r.data[r.offset:])
		r.offset += n
		return n, nil
	}
	if r.ready != nil {
		r.ready.Done()
		r.ready = nil
	}
	<-r.release
	return 0, io.EOF
}

func TestConcurrentDuplicatePuts(t *testing.T) {
	root := t.TempDir()
	cas, err := blob.New(root)
	if err != nil {
		t.Fatal(err)
	}
	payload := []byte("one immutable payload")
	const writers = 64
	var ready sync.WaitGroup
	ready.Add(writers)
	release := make(chan struct{})
	results := make(chan struct {
		ref pluginapi.BlobRef
		err error
	}, writers)
	for range writers {
		go func() {
			ref, err := cas.Put(&synchronizedReader{data: payload, ready: &ready, release: release})
			results <- struct {
				ref pluginapi.BlobRef
				err error
			}{ref: ref, err: err}
		}()
	}
	ready.Wait()
	close(release)

	var want pluginapi.BlobRef
	for range writers {
		result := <-results
		if result.err != nil {
			t.Fatalf("concurrent put: %v", result.err)
		}
		if want.SHA256 == "" {
			want = result.ref
		} else if result.ref != want {
			t.Fatalf("concurrent refs differ: %+v and %+v", want, result.ref)
		}
	}
	rc, err := cas.Get(want.SHA256)
	if err != nil {
		t.Fatal(err)
	}
	stored, err := io.ReadAll(rc)
	rc.Close()
	if err != nil || !bytes.Equal(stored, payload) {
		t.Fatalf("stored payload = %q, error = %v", stored, err)
	}
	partials, err := filepath.Glob(filepath.Join(root, "blobs", "sha256", ".put-*.tmp"))
	if err != nil || len(partials) != 0 {
		t.Fatalf("concurrent put partials = %v, error = %v", partials, err)
	}
}

func TestBigPutStreamsWithoutBuffering(t *testing.T) {
	// 8 MiB random payload — proves io.Copy path handles > memory-cheap sizes.
	buf := make([]byte, 8<<20)
	if _, err := rand.Read(buf); err != nil {
		t.Fatal(err)
	}
	cas, err := blob.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ref, err := cas.Put(bytes.NewReader(buf))
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	if ref.Size != int64(len(buf)) {
		t.Fatalf("size mismatch: got %d want %d", ref.Size, len(buf))
	}
}

func TestRejectBadHash(t *testing.T) {
	cas, err := blob.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"", "not-hex", "abcd", "ABCDEF" + strings.Repeat("0", 58)} {
		if _, err := cas.Get(bad); err == nil {
			t.Errorf("Get(%q) accepted a bad hash", bad)
		}
		if path, err := cas.Path(bad); err == nil || path != "" {
			t.Errorf("Path(%q) = %q, %v; want empty path and error", bad, path, err)
		}
	}
}
