// Demo-mode support endpoint. Consumed by the SPA to decide whether to
// render the "resets daily" banner + the /app/#/demo landing panel.
//
// The endpoint is public (no auth) — the SPA calls it before login on
// every boot. It reveals only whether the running instance is a public
// demo, which is not sensitive.

package api

import (
	"net/http"
)

// DemoMode is toggled at boot from cfg.DemoMode. Left as a plain field
// so tests can flip it without a config wire-up.
//
// nextResetUnix is the wall-clock (Unix seconds) of the next scheduled
// full reset. Zero means "no scheduled reset visible to the API"; the
// SPA falls back to "resets daily" copy.
type DemoConfig struct {
	Enabled       bool  `json:"enabled"`
	NextResetUnix int64 `json:"next_reset_unix,omitempty"`
}

// SetDemo is wired at boot from main.go. Nil is fine — the endpoint
// then reports enabled=false, matching a normal deployment.
func (s *Server) SetDemo(cfg DemoConfig) { s.demo = cfg }

// GetDemoMode — GET /api/demo/mode. Returns the current demo-mode
// state. Unauthenticated on purpose — see file header.
func (s *Server) GetDemoMode(w http.ResponseWriter, r *http.Request) {
	s.writeJSON(w, http.StatusOK, s.demo)
}
