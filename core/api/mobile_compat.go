// Mobile wire-compat surface. Handlers here implement a small,
// stable REST shape that third-party mobile document-management
// clients speak. Keeping the compat handlers in one file gives
// mobile clients a single wire-level dependency to audit; when the
// shape changes, the diff lives here and nowhere else.
//
// Every field, path, and response shape here is what suchi emits.
// suchi does not derive from, adapt, or reproduce any other project's
// code. Where the wire protocol happens to overlap with what other
// document-management systems accept, that's a convergent-design
// outcome, not a compat claim against any specific implementation.

package api

import (
	"database/sql"
	"encoding/json"
	"net/http"

	"github.com/johnnybravo-xyz/suchi/core/auth"
)

// wireVersionTag is the version this compat surface reports. It's
// suchi's own version of the wire protocol — not a claim to be
// version-compatible with any external project. Bumps land alongside
// golden-transcript re-recording; clients that pin a specific string
// should treat this as suchi's protocol version, not anyone else's.
const wireVersionTag = "0.1.0"

// suchiVersionTag identifies this specific implementation so
// operators can distinguish suchi instances in logs and support
// scripts. Currently the same as wireVersionTag; split if the wire
// shape stabilizes independently of the codebase.
const suchiVersionTag = "0.1.0"

// RemoteVersion returns the version handshake mobile clients use to
// gate feature detection.
//
//	GET /api/remote_version/
//	→ {"version": "0.1.0", "update_available": false, "suchi": "0.1.0"}
//
// update_available is always false — suchi doesn't self-update. The
// "suchi" field is unambiguous provenance for operators reading
// diagnostics.
func (s *Server) RemoteVersion(w http.ResponseWriter, r *http.Request) {
	if auth.FromContext(r.Context()) == nil {
		s.writeError(w, http.StatusUnauthorized, "unauthorized", "auth required")
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{
		"version":          wireVersionTag,
		"update_available": false,
		"suchi":            suchiVersionTag,
	})
}

// NextASN returns MAX(archive_serial_number) + 1 over live documents.
// Mobile scanners use this to pre-print a QR/barcode with the next
// ASN so it can be baked into the scan itself. Trashed docs excluded
// so undelete doesn't collide.
//
//	GET /api/next_asn/
//	→ 42
//
// Body is a bare integer (no JSON wrapping) — matches what mobile
// clients expect from this endpoint shape.
func (s *Server) NextASN(w http.ResponseWriter, r *http.Request) {
	if auth.FromContext(r.Context()) == nil {
		s.writeError(w, http.StatusUnauthorized, "unauthorized", "auth required")
		return
	}
	var maxASN sql.NullInt64
	err := s.DB.Read.QueryRowContext(r.Context(),
		`SELECT MAX(archive_serial_number) FROM documents WHERE trashed_at IS NULL`,
	).Scan(&maxASN)
	if err != nil {
		s.serverErr(w, "next_asn.query", err)
		return
	}
	next := int64(1)
	if maxASN.Valid {
		next = maxASN.Int64 + 1
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(next)
}
