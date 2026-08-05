// OpenAPI 3.1 spec served at GET /api/schema/. Hand-curated because
// suchi has no code-gen dependency and no runtime reflection over
// route handlers — a machine-generated spec would need one or the
// other. Curation cost is small (one file, one commit at each API
// change) and the spec is deterministic and reviewable.
//
// Every mutation to the /api/ surface should land alongside a matching
// change here. If it doesn't, we fail the compat contract with the
// clients that consume this spec.

package api

import (
	"embed"
	"net/http"
)

//go:embed schema.json
var openapiFS embed.FS

// GetSchema — GET /api/schema/. Serves the embedded OpenAPI 3.1
// document verbatim. Unauthenticated on purpose — the spec documents
// the surface, which is public information; every operation inside
// still enforces its own auth.
func (s *Server) GetSchema(w http.ResponseWriter, r *http.Request) {
	b, err := openapiFS.ReadFile("schema.json")
	if err != nil {
		s.serverErr(w, "schema.read", err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	// Short cache — schema evolves per release, mobile clients can
	// re-fetch on connect.
	w.Header().Set("Cache-Control", "public, max-age=300")
	_, _ = w.Write(b)
}
