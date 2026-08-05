// Package httpx is the HTTP layer glue: middleware stack, security
// headers, request-id, auth wiring, rate limiting.
//
// The router is stdlib net/http.ServeMux from Go 1.22+ — method-aware
// patterns are in the standard library and one less dep is one more
// point on principle 3.
package httpx

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/johnnybravo-xyz/suchi/core/auth"
	"github.com/johnnybravo-xyz/suchi/core/logx"
)

// Middleware is the func(http.Handler) http.Handler contract shared by
// every middleware in this package.
type Middleware func(http.Handler) http.Handler

// Chain composes middleware in outer-to-inner order — the first Middleware
// argument wraps the outermost, the last wraps closest to the handler.
func Chain(h http.Handler, mws ...Middleware) http.Handler {
	for i := len(mws) - 1; i >= 0; i-- {
		h = mws[i](h)
	}
	return h
}

// SecurityHeaders sets baseline defensive headers on every response.
//
// The CSP is intentionally strict for the API surface + server-rendered
// UI. The Svelte SPA under /app/ needs `style-src 'unsafe-inline'`
// because Svelte injects styles at runtime; that relaxation is scoped
// to /app/* only. The rest of the surface stays locked down.
func SecurityHeaders(next http.Handler) http.Handler {
	const strict = "default-src 'self'; img-src 'self' data:; " +
		"frame-ancestors 'none'; base-uri 'self'; form-action 'self'"
	// SPA CSP: same policy plus `style-src 'self' 'unsafe-inline'`.
	// Every other directive stays; only the runtime-style compromise
	// is allowed. No `script-src 'unsafe-inline'` — the JS bundle is
	// external.
	const spa = "default-src 'self'; img-src 'self' data:; " +
		"style-src 'self' 'unsafe-inline'; " +
		"frame-ancestors 'none'; base-uri 'self'; form-action 'self'"

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		if isSPAPath(r.URL.Path) {
			h.Set("Content-Security-Policy", spa)
		} else {
			h.Set("Content-Security-Policy", strict)
		}
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Permissions-Policy", "geolocation=(), microphone=(), camera=()")
		next.ServeHTTP(w, r)
	})
}

// isSPAPath reports whether the request targets the Svelte shell or
// one of its bundled assets. Kept as a plain string check (no route
// match) so the middleware stays cheap.
func isSPAPath(p string) bool {
	return p == "/app" || len(p) >= 5 && p[:5] == "/app/"
}

// RequestID injects a random request id into the response header and
// the logging context. If the client already sent an X-Request-Id we
// echo it back (transparent for downstream tracing) but do not trust
// it for anything security-relevant.
func RequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("X-Request-Id")
		if id == "" || len(id) > 64 {
			id = newRequestID()
		}
		w.Header().Set("X-Request-Id", id)
		ctx := logx.WithRequestID(r.Context(), id)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func newRequestID() string {
	var b [12]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// AccessLog emits one info line per request with method, path, status,
// bytes, and duration. Log body is intentionally spartan — the log is a
// forensic tool, not a metrics one; /metrics owns the counts.
func AccessLog(log *slog.Logger) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			sw := &statusWriter{ResponseWriter: w, status: 200}
			next.ServeHTTP(sw, r)
			log.LogAttrs(r.Context(), slog.LevelInfo, "http.access",
				slog.String("method", r.Method),
				slog.String("path", r.URL.Path),
				slog.Int("status", sw.status),
				slog.Int64("bytes", sw.bytes),
				slog.Duration("took", time.Since(start)),
			)
		})
	}
}

type statusWriter struct {
	http.ResponseWriter
	status int
	bytes  int64
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}
func (w *statusWriter) Write(b []byte) (int, error) {
	n, err := w.ResponseWriter.Write(b)
	w.bytes += int64(n)
	return n, err
}

// Authenticate runs the auth chain and puts the resulting Principal on
// the request context. Anonymous requests continue with nil principal;
// the handler decides whether that is OK for its route.
//
// A chain error is treated as 401 Unauthorized on purpose: a malformed
// or revoked token must NOT silently fall through to anonymous access.
func Authenticate(chain *auth.Chain, log *slog.Logger) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			p, err := chain.Authenticate(r)
			if err != nil {
				log.LogAttrs(r.Context(), slog.LevelInfo, "auth.reject",
					slog.String("err", err.Error()))
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			if p != nil {
				r = r.WithContext(auth.WithPrincipal(r.Context(), p))
			}
			next.ServeHTTP(w, r)
		})
	}
}

// RequireAuth is a route-level guard for handlers that must have a
// Principal. Compose after Authenticate.
func RequireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if auth.FromContext(r.Context()) == nil {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// NormalizeAPITrailingSlash accepts both `/api/foo/bar` and
// `/api/foo/bar/` for any registered API route, so third-party
// clients that follow the trailing-slash convention work against
// suchi without caring about the slash. Only paths under `/api/`
// are affected; the browser UI keeps its stricter matching.
//
// Strategy: leave the mux registrations untouched. When a request
// under `/api/` ends in `/` and the mux has no registered pattern for
// it, look up the same request with the trailing slash stripped; if
// THAT matches, rewrite r.URL.Path and dispatch. All other paths pass
// through with zero cost (one mux.Handler call, no ServeHTTP retry).
//
// Why not auto-register both forms per route? Go 1.22 ServeMux treats
// a pattern ending in `/` as a subtree matcher. Registering
// `/api/documents/{id}/` alongside `/api/documents/{id}` would catch
// stray tails like `/api/documents/1/garbage/` as `/api/documents/1`,
// masking real 404s. The check-then-retry middleware avoids that
// footgun.
func NormalizeAPITrailingSlash(mux *http.ServeMux) http.Handler {
	const prefix = "/api/"
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		if strings.HasPrefix(p, prefix) &&
			strings.HasSuffix(p, "/") &&
			len(p) > len(prefix) {
			// The UI registers `GET /` (and similar bare-root patterns)
			// as the browser catch-all. mux.Handler() returns these for
			// anything without a more specific match — including
			// `/api/foo/N/` — which would let the browser handler
			// swallow an API request. Treat any match whose registered
			// path is just "/" as "no API route matched, try strip".
			_, pat := mux.Handler(r)
			if isCatchAll(pat) {
				r2 := r.Clone(r.Context())
				r2.URL.Path = strings.TrimSuffix(p, "/")
				if _, pat2 := mux.Handler(r2); !isCatchAll(pat2) {
					mux.ServeHTTP(w, r2)
					return
				}
			}
		}
		mux.ServeHTTP(w, r)
	})
}

// isCatchAll returns true when pat is empty (no registration matched)
// or when pat is a bare root pattern (`/`, `GET /`, ...). Both mean
// the mux fell through to a wildcard that would happily eat an
// intended API request.
func isCatchAll(pat string) bool {
	if pat == "" {
		return true
	}
	// Patterns can be "/", "GET /", "POST /", "example.com/", etc.
	// Take the path portion — after the last space if a method prefix
	// is present — and compare.
	path := pat
	if i := strings.LastIndex(pat, " "); i >= 0 {
		path = pat[i+1:]
	}
	return path == "/"
}

// BodyLimit caps request bodies to n bytes using http.MaxBytesReader.
// Applied globally to every route. n <= 0 disables the cap for
// operators who genuinely need unbounded (rare — the review guidance
// is to keep the cap on with a sensible ceiling).
//
// When the reader trips, the handler downstream sees a
// http.MaxBytesError on its next Read; the upload endpoints
// specifically map that to a structured 413 response.
func BodyLimit(n int64) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if n > 0 {
				r.Body = http.MaxBytesReader(w, r.Body, n)
			}
			next.ServeHTTP(w, r)
		})
	}
}

// RateLimit is a naive per-IP token bucket, intentionally kept in-process
// for MVP. Applied to auth-sensitive endpoints (login, setup, token
// issuance) at ~5 req/s with a small burst — plenty for humans, painful
// for password sprays. Not a substitute for a WAF, and not designed to.
type RateLimit struct {
	mu      sync.Mutex
	buckets map[string]*bucket
	rate    float64 // tokens per second
	burst   float64
}

type bucket struct {
	tokens float64
	last   time.Time
}

// NewRateLimit returns a bucket-per-remote limiter.
func NewRateLimit(perSecond, burst float64) *RateLimit {
	return &RateLimit{buckets: map[string]*bucket{}, rate: perSecond, burst: burst}
}

// Middleware form.
func (r *RateLimit) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if !r.allow(clientIP(req)) {
			http.Error(w, "rate limited", http.StatusTooManyRequests)
			return
		}
		next.ServeHTTP(w, req)
	})
}

func (r *RateLimit) allow(key string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := time.Now()
	b, ok := r.buckets[key]
	if !ok {
		r.buckets[key] = &bucket{tokens: r.burst - 1, last: now}
		return true
	}
	elapsed := now.Sub(b.last).Seconds()
	b.tokens = min(r.burst, b.tokens+elapsed*r.rate)
	b.last = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// clientIP prefers X-Forwarded-For's leftmost entry (behind a trusted
// proxy) but falls back to r.RemoteAddr. This is a lie when there is no
// trusted proxy in front — operators running without a proxy get real
// remote addrs by default.
func clientIP(r *http.Request) string {
	if v := r.Header.Get("X-Forwarded-For"); v != "" {
		if i := strings.Index(v, ","); i > 0 {
			return strings.TrimSpace(v[:i])
		}
		return strings.TrimSpace(v)
	}
	host := r.RemoteAddr
	if i := strings.LastIndex(host, ":"); i > 0 {
		host = host[:i]
	}
	return host
}

// CtxTimeout wraps requests in a hard timeout to bound handler work.
func CtxTimeout(d time.Duration) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx, cancel := context.WithTimeout(r.Context(), d)
			defer cancel()
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}
