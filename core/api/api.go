// Package api owns the JSON HTTP surface — endpoints under /api/*.
//
// This is where machine clients (mobile apps, agents, ingest producers,
// automation scripts) talk to suchi. Everything is JSON in and JSON out;
// no HTML, no template rendering. The UI package handles the browser
// surface.
//
// Register(mux) attaches every route this package owns. Each handler is
// a plain http.HandlerFunc — no framework, no reflection, no middleware
// magic. The auth chain runs upstream via httpx.Authenticate.
package api

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"github.com/suchi-dms/suchi/core/blob"
	"github.com/suchi-dms/suchi/core/db"
)

// Server bundles the state every /api handler needs. Constructed once
// at boot; safe for concurrent use.
type Server struct {
	DB  *db.DB
	CAS *blob.CAS
	Log *slog.Logger
}

// New returns a Server. The zero value isn't runnable — every field is
// required.
func New(d *db.DB, cas *blob.CAS, log *slog.Logger) (*Server, error) {
	if d == nil || cas == nil || log == nil {
		return nil, errors.New("api.New: DB, CAS, and Log are required")
	}
	return &Server{DB: d, CAS: cas, Log: log.With("component", "api")}, nil
}

// Register attaches every /api route this package owns to mux. Called
// from main.go after the auth chain is wired — httpx.Authenticate runs
// upstream, so handlers here can rely on auth.FromContext.
func (s *Server) Register(mux *http.ServeMux) {
	// Documents.
	mux.HandleFunc("POST /api/documents/", s.UploadDocument)
	mux.HandleFunc("DELETE /api/documents/{id}", s.SoftDeleteDocument)
	mux.HandleFunc("POST /api/documents/{id}/restore", s.RestoreDocument)
}

// ---------- shared helpers ----------

// writeJSON writes v as JSON with the given status code. Errors from
// the encoder are logged but otherwise swallowed — by the time we start
// writing the body, we can't send a different status.
func (s *Server) writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		s.Log.Warn("api.writeJSON", "err", err.Error())
	}
}

// writeError emits the canonical error shape:
//
//	{"error": "human message", "code": "snake_case_kind"}
//
// The code is programmatically stable across releases; error text isn't.
// See docs/api.md#errors.
func (s *Server) writeError(w http.ResponseWriter, status int, code, msg string) {
	s.writeJSON(w, status, errBody{Code: code, Error: msg})
}

type errBody struct {
	Error string `json:"error"`
	Code  string `json:"code"`
}
