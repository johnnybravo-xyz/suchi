// Avatar upload + serve for /api/users/me/avatar and
// /api/users/{id}/avatar.
//
// Security posture:
//   - Multipart size cap is enforced by BodyLimit middleware upstream
//     (default 500M, room for a 2MB image; a dedicated 2MB check
//     here bounds a decompression bomb regardless).
//   - Only image/png + image/jpeg accepted. Content-Type is sniffed
//     server-side; the multipart Content-Type header is a hint only.
//   - DecodeConfig reads only the header, so a "PNG that claims
//     50000×50000" gets refused before Decode allocates.
//   - Decode + re-encode as PNG. That strips EXIF, kills polyglot
//     tricks (nothing after the PNG IEND is emitted), and normalizes
//     the on-disk representation. The original bytes are never stored.
//   - CAS put by sha256 of the re-encoded bytes; users.avatar_sha
//     holds the hex. Immutable ETag on serve — a re-upload always
//     produces a fresh sha because the pixel data changes, so client
//     caches invalidate automatically.

package api

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"image"
	_ "image/jpeg" // registers jpeg decoder
	"image/png"
	"io"
	"net/http"
	"strconv"

	"github.com/suchi-dms/suchi/core/audit"
	"github.com/suchi-dms/suchi/core/auth"
	"github.com/suchi-dms/suchi/core/blob"
)

// avatarMaxBytes is a per-upload hard cap. BodyLimit already bounds
// the multipart body upstream, but keeping this local means the
// image-decoder path stays predictable even if BodyLimit gets tuned
// higher for large PDFs.
const avatarMaxBytes = 2 << 20 // 2 MiB

// avatarMaxDim rejects images whose declared dimensions would decode
// into more pixels than we're willing to spend RAM on (dimension^2
// * 4 bytes per RGBA pixel). 2048×2048 = ~16MB, comfortable.
const avatarMaxDim = 2048

// PostSelfAvatar serves POST /api/users/me/avatar. Multipart with a
// field named `avatar`. Returns the fresh UserSelf on success.
func (s *Server) PostSelfAvatar(w http.ResponseWriter, r *http.Request) {
	p := auth.FromContext(r.Context())
	if p == nil {
		s.writeError(w, http.StatusUnauthorized, "unauthorized", "auth required")
		return
	}
	if err := r.ParseMultipartForm(avatarMaxBytes); err != nil {
		s.writeError(w, http.StatusBadRequest, "bad_multipart", err.Error())
		return
	}
	file, hdr, err := r.FormFile("avatar")
	if err != nil {
		s.writeError(w, http.StatusBadRequest, "no_file",
			"multipart field `avatar` is required")
		return
	}
	defer file.Close()

	// Cap reads at avatarMaxBytes+1 so we can detect "too big" without
	// allocating unbounded memory when a multipart part slipped past
	// BodyLimit (e.g. a proxy chunk-transfer quirk).
	raw, err := io.ReadAll(io.LimitReader(file, avatarMaxBytes+1))
	if err != nil {
		s.writeError(w, http.StatusBadRequest, "read_failed", err.Error())
		return
	}
	if int64(len(raw)) > avatarMaxBytes {
		s.writeError(w, http.StatusRequestEntityTooLarge, "too_large",
			"avatar must be 2 MiB or smaller")
		return
	}
	if hdr.Size > 0 && hdr.Size > avatarMaxBytes {
		s.writeError(w, http.StatusRequestEntityTooLarge, "too_large",
			"avatar must be 2 MiB or smaller")
		return
	}

	// Sniff. http.DetectContentType is the same magic-byte table
	// mimesniff.go uses on upload; keeping the check server-side
	// means a client faking Content-Type gets a 400, not a partial
	// write.
	mime := http.DetectContentType(raw)
	if mime != "image/png" && mime != "image/jpeg" {
		s.writeError(w, http.StatusBadRequest, "bad_mime",
			"avatar must be image/png or image/jpeg")
		return
	}

	// Bound dimensions before Decode allocates. DecodeConfig reads
	// only the header block.
	cfg, _, err := image.DecodeConfig(bytes.NewReader(raw))
	if err != nil {
		s.writeError(w, http.StatusBadRequest, "bad_image", err.Error())
		return
	}
	if cfg.Width > avatarMaxDim || cfg.Height > avatarMaxDim {
		s.writeError(w, http.StatusBadRequest, "image_too_big",
			"avatar dimensions must be 2048x2048 or smaller")
		return
	}

	img, _, err := image.Decode(bytes.NewReader(raw))
	if err != nil {
		s.writeError(w, http.StatusBadRequest, "decode_failed", err.Error())
		return
	}

	// Re-encode as PNG. Strips EXIF (PNG has no equivalent) and any
	// JPEG-specific hidden metadata; drops appended polyglot bytes
	// because png.Encode writes only the required chunks.
	var out bytes.Buffer
	if err := png.Encode(&out, img); err != nil {
		s.serverErr(w, "avatar.encode", err)
		return
	}
	encoded := out.Bytes()

	// Store in CAS.
	ref, err := s.CAS.Put(bytes.NewReader(encoded))
	if err != nil {
		s.serverErr(w, "avatar.cas_put", err)
		return
	}

	// Snapshot old sha for the audit event + update the user row.
	var old string
	_ = s.DB.Read.QueryRowContext(r.Context(),
		"SELECT COALESCE(avatar_sha, '') FROM users WHERE id = ?", p.UserID).Scan(&old)

	if _, err := s.DB.Write.ExecContext(r.Context(),
		"UPDATE users SET avatar_sha = ?, updated_at = unixepoch() WHERE id = ?",
		ref.SHA256, p.UserID); err != nil {
		s.serverErr(w, "avatar.write", err)
		return
	}

	audit.Log(r.Context(), s.DB, s.Log, audit.Event{
		Actor: p, Action: "user.avatar_changed",
		ObjectKind: "user", ObjectID: p.UserID,
		Before: map[string]any{"avatar_sha": old},
		After:  map[string]any{"avatar_sha": ref.SHA256, "size": len(encoded)},
	})

	self, err := loadSelf(r.Context(), s.DB.Read, p)
	if err != nil {
		s.serverErr(w, "avatar.reload", err)
		return
	}
	s.writeJSON(w, http.StatusOK, self)
}

// GetUserAvatar serves GET /api/users/{id}/avatar. Public within the
// authed graph — any authed user can see any other user's avatar,
// same posture as display_name. 404 when no avatar is set.
//
// Immutable ETag: users.avatar_sha changes on every upload, so
// max-age=31536000 + strong ETag lets browsers cache aggressively and
// invalidate automatically on the next upload.
func (s *Server) GetUserAvatar(w http.ResponseWriter, r *http.Request) {
	if auth.FromContext(r.Context()) == nil {
		s.writeError(w, http.StatusUnauthorized, "unauthorized", "auth required")
		return
	}
	rawID := r.PathValue("id")
	uid, err := strconv.ParseInt(rawID, 10, 64)
	if err != nil || uid <= 0 {
		s.writeError(w, http.StatusBadRequest, "bad_id", "user id must be a positive integer")
		return
	}
	var sha string
	err = s.DB.Read.QueryRowContext(r.Context(),
		"SELECT COALESCE(avatar_sha, '') FROM users WHERE id = ?", uid).Scan(&sha)
	if err != nil || sha == "" {
		s.writeError(w, http.StatusNotFound, "no_avatar", "user has no avatar")
		return
	}

	// ETag + If-None-Match short-circuit. Wrap sha in quotes per
	// RFC 9110 (strong validator syntax).
	etag := `"` + sha + `"`
	w.Header().Set("ETag", etag)
	w.Header().Set("Cache-Control", "private, max-age=31536000, immutable")
	if match := r.Header.Get("If-None-Match"); match != "" && match == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}

	rc, err := s.CAS.Get(sha)
	if errors.Is(err, blob.ErrNotFound) {
		// Sha in DB but blob missing — treat as no avatar so a
		// broken CAS doesn't nuke the topbar.
		s.writeError(w, http.StatusNotFound, "no_avatar", "avatar blob missing")
		return
	}
	if err != nil {
		s.serverErr(w, "avatar.get", err)
		return
	}
	defer rc.Close()

	// Content-Type is fixed: PostSelfAvatar always re-encodes as PNG.
	w.Header().Set("Content-Type", "image/png")
	if _, err := io.Copy(w, rc); err != nil {
		s.Log.Warn("avatar.copy", "err", err.Error())
	}
}

// _ = hex.EncodeToString is a compile-time reference kept out of the
// hot path — used indirectly via CAS.Put -> BlobRef.SHA256.
var _ = hex.EncodeToString

// _ = sha256.New — same reason; CAS handles the hashing.
var _ = sha256.New
