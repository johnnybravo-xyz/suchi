// Package blob is the content-addressed store.
//
// One implementation, filesystem-backed, sharded three levels deep:
//
//	$DATA_DIR/blobs/sha256/ab/cd/ef/abcdef...ff
//
// Three levels caps any single directory at ~4k entries even with
// millions of blobs — matters on ext4/xfs where a single directory
// with hundreds of thousands of entries turns O(1) opens into O(N)
// disk seeks, and matters for backup tooling (rsync, restic) that
// walks the tree.
//
// Writes stream through sha256 and land via atomic rename from a
// per-put temp file in the same sharded directory (so the rename is
// same-device). Duplicate puts are cheap: same hash → same path → we
// keep the existing file and return the ref.
//
// Interface stays concrete for Phase 1. When a second backend arrives
// (S3, blob-crypt), we lift Put/Get/Stat/Delete into plugin-api and
// promote this to plugins/fs-cas.
package blob

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"

	pluginapi "github.com/johnnybravo-xyz/suchi/plugin-api"
)

// CAS is the filesystem content-addressed store.
type CAS struct {
	root string // absolute; contains a "sha256" subdir
}

// ErrNotFound is returned by Get/Stat when the requested hash is absent.
var ErrNotFound = errors.New("blob not found")

// New returns a CAS rooted at dir/blobs. Creates the shard root if
// missing. The blob directory is 0o750 — group readable so a sidecar
// container can serve /preview/ without needing root.
func New(dir string) (*CAS, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	root := filepath.Join(abs, "blobs", "sha256")
	if err := os.MkdirAll(root, 0o750); err != nil {
		return nil, fmt.Errorf("mkdir cas root: %w", err)
	}
	return &CAS{root: root}, nil
}

// Put streams r into the store, returning the BlobRef. If the content
// already exists at its computed hash, Put is a no-op that still returns
// the ref — dedup is a property of the CAS, not the caller.
//
// The temp file is created inside the destination shard directory so the
// final rename is always same-device (POSIX atomic).
func (c *CAS) Put(r io.Reader) (pluginapi.BlobRef, error) {
	// Bootstrap: we don't know the hash yet, so we can't pick the final
	// shard until after we've read the stream. Use a top-level temp in
	// the CAS root and move it into the shard directory once we know the
	// hash. Same device (both under c.root) so rename remains atomic.
	tmp, err := os.CreateTemp(c.root, ".put-*.tmp")
	if err != nil {
		return pluginapi.BlobRef{}, err
	}
	tmpName := tmp.Name()
	// If anything fails between here and success, clean up the temp.
	defer func() {
		if tmpName != "" {
			_ = os.Remove(tmpName)
		}
	}()

	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(tmp, h), r)
	if err != nil {
		_ = tmp.Close()
		return pluginapi.BlobRef{}, fmt.Errorf("stream: %w", err)
	}
	// fsync so the rename below cannot land a torn write on power loss.
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return pluginapi.BlobRef{}, fmt.Errorf("fsync: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return pluginapi.BlobRef{}, err
	}

	sum := hex.EncodeToString(h.Sum(nil))
	dst := c.path(sum)
	if err := os.MkdirAll(filepath.Dir(dst), 0o750); err != nil {
		return pluginapi.BlobRef{}, fmt.Errorf("mkdir shard: %w", err)
	}

	// Dedup: if the target already exists AND is the right size, keep it.
	// A truncated pre-existing file would be caught by size mismatch; a
	// forged file with the same size but wrong content would need a
	// separate scrub tool, not a Put-time check.
	if fi, statErr := os.Stat(dst); statErr == nil {
		if fi.Size() == n {
			tmpName = "" // don't remove temp — actually we still want to remove
			_ = os.Remove(tmp.Name())
			return pluginapi.BlobRef{SHA256: sum, Size: n}, nil
		}
		// Fall through — rename below will replace the corrupt one.
	}

	if err := os.Rename(tmp.Name(), dst); err != nil {
		return pluginapi.BlobRef{}, fmt.Errorf("rename: %w", err)
	}
	tmpName = "" // rename consumed the temp

	// Normalize permissions to 0640 — owner rw, group r. os.CreateTemp
	// makes the file 0600; the chmod broadens to group so a sidecar
	// (rendered-view, etc.) can serve blobs without root. Failure here
	// is unusual (fs doesn't support chmod, or we're not the owner);
	// return it so callers see a real diagnosis instead of a downstream
	// "permission denied" on Get.
	if err := os.Chmod(dst, 0o640); err != nil {
		return pluginapi.BlobRef{}, fmt.Errorf("chmod %s: %w", dst, err)
	}

	return pluginapi.BlobRef{SHA256: sum, Size: n}, nil
}

// Get opens a blob for reading.
func (c *CAS) Get(sum string) (io.ReadCloser, error) {
	if !validHash(sum) {
		return nil, fmt.Errorf("bad hash %q", sum)
	}
	f, err := os.Open(c.path(sum))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, ErrNotFound
	}
	return f, err
}

// Stat returns the BlobRef for a hash if it exists.
func (c *CAS) Stat(sum string) (pluginapi.BlobRef, error) {
	if !validHash(sum) {
		return pluginapi.BlobRef{}, fmt.Errorf("bad hash %q", sum)
	}
	fi, err := os.Stat(c.path(sum))
	if errors.Is(err, fs.ErrNotExist) {
		return pluginapi.BlobRef{}, ErrNotFound
	}
	if err != nil {
		return pluginapi.BlobRef{}, err
	}
	return pluginapi.BlobRef{SHA256: sum, Size: fi.Size()}, nil
}

// Delete removes the blob file. Returns nil for a hash that was already
// gone — idempotent.
func (c *CAS) Delete(sum string) error {
	if !validHash(sum) {
		return fmt.Errorf("bad hash %q", sum)
	}
	err := os.Remove(c.path(sum))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}

// List walks every stored blob, calling fn with its ref. Order is
// filesystem-defined, i.e. essentially undefined. Used by `suchi gc`.
func (c *CAS) List(fn func(pluginapi.BlobRef) error) error {
	return filepath.WalkDir(c.root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		name := d.Name()
		if !validHash(name) {
			return nil // skip stray files / temps
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		return fn(pluginapi.BlobRef{SHA256: name, Size: fi.Size()})
	})
}

// path returns the sharded path for a hash under the current
// three-level layout. Never called on user input without validHash
// first.
func (c *CAS) path(sum string) string {
	return filepath.Join(c.root, sum[0:2], sum[2:4], sum[4:6], sum)
}

// validHash checks that a string is exactly 64 lower-case hex chars.
// Anything else is suspicious — refuse to touch it.
func validHash(s string) bool {
	if len(s) != 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}
