// OpenAPI 3.1 spec served at GET /api/schema/. Summaries and schemas are
// curated; `make schema` synchronizes registered routes and defaults.

package api

import (
	"embed"
	"net/http"
)

//go:embed schema.json
var openapiFS embed.FS

// GetSchema serves the embedded OpenAPI document without authentication.
func (s *Server) GetSchema(w http.ResponseWriter, r *http.Request) {
	b, err := openapiFS.ReadFile("schema.json")
	if err != nil {
		s.serverErr(w, "schema.read", err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "public, max-age=300")
	_, _ = w.Write(b)
}
