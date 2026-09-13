package paths

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestMatchesCASLink(t *testing.T) {
	hash := "abcdef" + strings.Repeat("0123456789", 5) + "01234567"
	root := t.TempDir()
	canonical := filepath.Join(root, "blobs", "sha256", "ab", "cd", "ef", hash)
	for _, tc := range []struct {
		name, link, hash string
		want             bool
	}{
		{"canonical", canonical, hash, true},
		{"restored", filepath.Join(root, "old-data", "blobs", "sha256", "ab", "cd", "ef", hash), hash, true},
		{"filename only", filepath.Join(root, hash), hash, false},
		{"relative", filepath.Join("blobs", "sha256", "ab", "cd", "ef", hash), hash, false},
		{"unclean", root + string(filepath.Separator) + "unused" + string(filepath.Separator) + ".." + string(filepath.Separator) + filepath.Join("blobs", "sha256", "ab", "cd", "ef", hash), hash, false},
		{"wrong shard", filepath.Join(root, "blobs", "sha256", "00", "cd", "ef", hash), hash, false},
		{"empty hash", canonical, "", false},
		{"short hash", canonical, "abc", false},
		{"long hash", canonical, hash + "0", false},
		{"uppercase hash", canonical, strings.ToUpper(hash), false},
		{"nonhex hash", canonical, "g" + hash[1:], false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := MatchesCASLink(tc.link, tc.hash); got != tc.want {
				t.Fatalf("MatchesCASLink(%q, %q)=%v, want %v", tc.link, tc.hash, got, tc.want)
			}
		})
	}
}
