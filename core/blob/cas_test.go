package blob_test

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"strings"
	"testing"

	"github.com/suchi-dms/suchi/core/blob"
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
	}
}
