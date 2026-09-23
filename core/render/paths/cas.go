// SPDX-License-Identifier: AGPL-3.0-or-later

package paths

import (
	"path/filepath"
	"strings"
)

// MatchesCASLink accepts canonical CAS links, including links into an old data
// root after a whole-directory restore. The caller must independently establish
// that hash belongs to the document/path; a filename alone does not prove ownership.
func MatchesCASLink(link, hash string) bool {
	if len(hash) != 64 {
		return false
	}
	for _, c := range hash {
		if !(c >= '0' && c <= '9') && !(c >= 'a' && c <= 'f') {
			return false
		}
	}
	if !filepath.IsAbs(link) || filepath.Clean(link) != link {
		return false
	}
	suffix := filepath.Join("blobs", "sha256", hash[:2], hash[2:4], hash[4:6], hash)
	return strings.HasSuffix(link, string(filepath.Separator)+suffix)
}
