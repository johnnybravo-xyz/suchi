// Demo-mode surface. Three endpoints:
//
//   - GET  /api/demo/mode              — probe (unauthenticated).
//   - POST /api/demo/session           — mint an anonymous read-only
//                                        token (unauthenticated). No DB
//                                        write. This is the load-plane
//                                        wire: most visitors only ever
//                                        touch this endpoint + reads.
//   - POST /api/demo/session/upgrade   — trade an anonymous token for a
//                                        scratch user + API token. Only
//                                        invoked when the visitor tries
//                                        something that needs a writer,
//                                        i.e. `POST /api/documents/`.
//
// The upgrade endpoint costs one INSERT-user + one INSERT-token; the
// reset ticker (distro/demo/ticker.go) sweeps both on TTL.

package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"time"

	"github.com/johnnybravo-xyz/suchi/core/auth"
)

// PrincipalKindDemoAnon is the auth chain's tag for anonymous demo
// visitors. Duplicated from distro/demo.PrincipalKind to keep core/api
// free of a distro/* import. If the two ever drift, the upgrade
// endpoint will silently 401 — a startup assertion in main.go pins
// them together.
const PrincipalKindDemoAnon = "demo-anon"

// DemoConfig carries the mode flag + next-reset hint to the SPA. Kept
// small so the endpoint's response fits in a single TCP packet.
//
// nextResetUnix is the wall-clock (Unix seconds) of the next scheduled
// full reset. Zero means "no scheduled reset visible to the API"; the
// SPA falls back to "resets daily" copy.
type DemoConfig struct {
	Enabled       bool  `json:"enabled"`
	NextResetUnix int64 `json:"next_reset_unix,omitempty"`
}

// DemoTokenMinter is the seam the anonymous-token authenticator plugs
// into. Wired at boot in main.go so this package doesn't import
// distro/demo/*.
//
//	Mint(ttl) → (token, expiry, err)
//
// Nil disables the session endpoints (503).
type DemoTokenMinter func(ttl time.Duration) (token string, expiry time.Time, err error)

// DemoScratchUserProvisioner creates a per-visitor scratch user and
// returns its id. Wired at boot; nil disables /upgrade (503).
type DemoScratchUserProvisioner func(ctx context.Context, email, displayName string) (userID int64, err error)

// SetDemo is wired at boot from main.go. Nil is fine — the endpoint
// then reports enabled=false, matching a normal deployment.
func (s *Server) SetDemo(cfg DemoConfig) { s.demo = cfg }

// SetDemoMinter wires the anonymous-token minter. Only used when demo
// mode is on; safe to call with nil in tests.
func (s *Server) SetDemoMinter(m DemoTokenMinter) { s.demoMint = m }

// SetDemoScratchProvisioner wires the scratch-user factory. Only
// meaningful when demo mode is on.
func (s *Server) SetDemoScratchProvisioner(p DemoScratchUserProvisioner) { s.demoScratch = p }

// GetDemoMode — GET /api/demo/mode. Returns the current demo-mode
// state. Unauthenticated on purpose — see file header.
func (s *Server) GetDemoMode(w http.ResponseWriter, r *http.Request) {
	s.writeJSON(w, http.StatusOK, s.demo)
}

type demoSessionResp struct {
	Kind        string `json:"kind"`  // "anon" or "scratch"
	Token       string `json:"token"` // opaque; the SPA sends it back
	ExpiresUnix int64  `json:"expires_unix,omitempty"`
	Header      string `json:"header"` // header the SPA should send it in
}

// PostDemoSession — POST /api/demo/session. Unauthenticated. Mints an
// anonymous read-only token. Idempotent per-visitor at the wire level:
// the SPA is expected to cache the token in sessionStorage and call
// this only when it doesn't have one.
//
// Failure modes:
//   - 404 demo_disabled — demo mode is off.
//   - 503 demo_unwired  — mint wire is missing (bootstrap bug).
func (s *Server) PostDemoSession(w http.ResponseWriter, r *http.Request) {
	if !s.demo.Enabled {
		s.writeError(w, http.StatusNotFound, "demo_disabled", "demo mode is not enabled on this instance")
		return
	}
	if s.demoMint == nil {
		s.writeError(w, http.StatusServiceUnavailable, "demo_unwired", "demo minter is not wired")
		return
	}
	token, exp, err := s.demoMint(0) // 0 → default TTL from the minter
	if err != nil {
		s.Log.Warn("api.demo.session.mint_err", "err", err.Error())
		s.writeError(w, http.StatusInternalServerError, "mint_failed", "could not mint demo token")
		return
	}
	s.writeJSON(w, http.StatusOK, demoSessionResp{
		Kind:        "anon",
		Token:       token,
		ExpiresUnix: exp.Unix(),
		Header:      "X-Suchi-Demo-Token",
	})
}

// PostDemoSessionUpgrade — POST /api/demo/session/upgrade. Requires an
// anonymous token (the auth chain has already vetted it). Provisions a
// scratch user `visitor-<nanoid>@demo.local`, mints an API token bound
// to it, returns the token. The SPA then swaps its X-Suchi-Demo-Token
// header for `Authorization: Token <new>` and retries the write.
//
// Failure modes:
//   - 401 unauthorized      — no valid anon token on the request.
//   - 404 demo_disabled     — demo mode is off.
//   - 503 demo_unwired      — provisioner or token issuer missing.
func (s *Server) PostDemoSessionUpgrade(w http.ResponseWriter, r *http.Request) {
	if !s.demo.Enabled {
		s.writeError(w, http.StatusNotFound, "demo_disabled", "demo mode is not enabled on this instance")
		return
	}
	if s.demoScratch == nil || s.TokenIssuer == nil {
		s.writeError(w, http.StatusServiceUnavailable, "demo_unwired", "demo scratch provisioner is not wired")
		return
	}
	// Guard: only anonymous-demo callers upgrade. A real user hitting
	// this by mistake gets a clean 401 instead of a stray scratch row.
	p := auth.FromContext(r.Context())
	if p == nil || p.Kind != PrincipalKindDemoAnon {
		s.writeError(w, http.StatusUnauthorized, "unauthorized", "an anonymous demo token is required to upgrade")
		return
	}

	suffix, err := randomSuffix()
	if err != nil {
		s.Log.Warn("api.demo.upgrade.rand_err", "err", err.Error())
		s.writeError(w, http.StatusInternalServerError, "provision_failed", "could not mint scratch user")
		return
	}
	email := "visitor-" + suffix + "@demo.local"
	uid, err := s.demoScratch(r.Context(), email, "Demo visitor")
	if err != nil {
		s.Log.Warn("api.demo.upgrade.provision_err", "err", err.Error())
		s.writeError(w, http.StatusInternalServerError, "provision_failed", "could not mint scratch user")
		return
	}
	token, err := s.TokenIssuer(r.Context(), uid, "demo-visitor", "read,write")
	if err != nil {
		s.Log.Warn("api.demo.upgrade.token_err", "err", err.Error())
		s.writeError(w, http.StatusInternalServerError, "token_failed", "could not mint token")
		return
	}
	// Scratch tokens follow the standard API token TTL; the ticker
	// bounds usability by sweeping the user row on its TTL. We don't
	// emit an explicit ExpiresUnix because the client should be
	// resilient to a 401 mid-flight and re-mint.
	s.writeJSON(w, http.StatusOK, demoSessionResp{
		Kind:   "scratch",
		Token:  token,
		Header: "Authorization",
	})
}

// randomSuffix returns 12 hex chars — enough entropy that N concurrent
// visitors won't collide (birthday-bound at ~1e7 for a 50% chance).
func randomSuffix() (string, error) {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}
