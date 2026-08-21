// Package httpx is the HTTP layer glue: middleware stack, security
// headers, request-id, auth wiring, rate limiting.
//
// The router is stdlib net/http.ServeMux from Go 1.22+ — method-aware
// patterns are in the standard library and one less dep is one more
// point on principle 3.
package httpx

import (
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
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

// SecFetchSite rejects cross-site state-changing requests where the
// caller is authenticated by a session cookie. It's the modern
// browser-shipped CSRF signal — every browser Google can see stamps
// `Sec-Fetch-Site: same-origin | same-site | cross-site | none` on
// every request. `SameSite=Lax` on the session cookie already blocks
// most cross-site forms; this middleware closes the edge cases
// (older engines with lax defaults, opaque origins, javascript:
// redirect chains, subdomain takeovers).
//
// Token-authenticated calls (`Authorization: Token …` / `Bearer …`)
// are exempt — a cross-site attacker cannot forge an Authorization
// header, so the CSRF class of attack doesn't apply.
//
// Compose AFTER Authenticate — needs `auth.FromContext` to see the
// principal kind.
func SecFetchSite(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Only state-changing verbs need the check. GET/HEAD/OPTIONS
		// with a cookie can leak information but not mutate.
		switch r.Method {
		case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		default:
			next.ServeHTTP(w, r)
			return
		}
		p := auth.FromContext(r.Context())
		// Token / bearer calls exempt: forge-proof.
		if p != nil && (p.Kind == "token" || p.Kind == demoScratchPrincipalKind) {
			next.ServeHTTP(w, r)
			return
		}
		site := r.Header.Get("Sec-Fetch-Site")
		// Older browsers that don't send the header at all get a
		// pass — turning them into 403 across the board would break
		// curl + integration scripts that don't set the header. The
		// SameSite=Lax cookie is the fallback line.
		if site == "" || site == "same-origin" || site == "same-site" || site == "none" {
			next.ServeHTTP(w, r)
			return
		}
		http.Error(w, "cross-site request refused", http.StatusForbidden)
	})
}

// RequireAuth is a route-level guard for handlers that must have a
// Principal. Compose after Authenticate.
func RequireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if auth.FromContext(r.Context()) == nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"auth required","code":"unauthorized"}`))
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
func NormalizeAPITrailingSlash(mux *http.ServeMux) http.Handler {
	const prefix = "/api/"
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		if !strings.HasPrefix(p, prefix) {
			mux.ServeHTTP(w, r)
			return
		}

		_, pat := mux.Handler(r)
		if strings.HasSuffix(p, "/") && len(p) > len(prefix) {
			if isCatchAll(pat) {
				pat = ""
			}
			r2 := r.Clone(r.Context())
			r2.URL.Path = strings.TrimSuffix(p, "/")
			r2.URL.RawPath = ""
			if _, pat2 := mux.Handler(r2); pat2 != pat &&
				!isCatchAll(pat2) && !isSubtreeFallback(pat2, r2.URL.Path) {
				mux.ServeHTTP(w, r2)
				return
			}
		} else if strings.HasSuffix(muxPatternPath(pat), "/") &&
			!isSubtreeFallback(pat, p) {
			r2 := r.Clone(r.Context())
			r2.URL.Path = p + "/"
			r2.URL.RawPath = ""
			mux.ServeHTTP(w, r2)
			return
		}
		if isCatchAll(pat) || isSubtreeFallback(pat, p) {
			http.NotFound(w, r)
			return
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
	return muxPatternPath(pat) == "/"
}

// ServeMux patterns ending in a slash also match every deeper path.
// Reject those accidental matches so collection handlers cannot swallow
// item routes or unknown API tails.
func isSubtreeFallback(pat, requestPath string) bool {
	path := muxPatternPath(pat)
	if !strings.HasSuffix(path, "/") {
		return false
	}
	return pathDepth(requestPath) > pathDepth(path)
}

func muxPatternPath(pat string) string {
	if i := strings.LastIndex(pat, " "); i >= 0 {
		return pat[i+1:]
	}
	return pat
}

func pathDepth(path string) int {
	path = strings.Trim(path, "/")
	if path == "" {
		return 0
	}
	return strings.Count(path, "/") + 1
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

const (
	defaultRateLimitEntries = 4096
	defaultRateLimitIdle    = 10 * time.Minute
)

// RateLimit is an in-process token bucket keyed by a verified client address.
type RateLimit struct {
	mu      sync.Mutex
	buckets map[string]*bucket
	rate    float64
	burst   float64
	max     int
	idle    time.Duration
	trusted []netip.Prefix
}

type bucket struct {
	tokens float64
	last   time.Time
}

// NewRateLimit returns a bucket-per-client limiter. Forwarded addresses are
// considered only when the direct peer matches one of trustedProxies.
func NewRateLimit(perSecond, burst float64, trustedProxies ...netip.Prefix) *RateLimit {
	return &RateLimit{
		buckets: map[string]*bucket{},
		rate:    perSecond,
		burst:   burst,
		max:     defaultRateLimitEntries,
		idle:    defaultRateLimitIdle,
		trusted: append([]netip.Prefix(nil), trustedProxies...),
	}
}

func (r *RateLimit) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if !r.allow(r.clientIP(req)) {
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
		r.makeRoom(now)
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

func (r *RateLimit) makeRoom(now time.Time) {
	if len(r.buckets) < r.max {
		return
	}
	for key, b := range r.buckets {
		if now.Sub(b.last) >= r.idle {
			delete(r.buckets, key)
		}
	}
	if len(r.buckets) < r.max {
		return
	}

	var oldestKey string
	var oldest time.Time
	for key, b := range r.buckets {
		if oldestKey == "" || b.last.Before(oldest) {
			oldestKey, oldest = key, b.last
		}
	}
	delete(r.buckets, oldestKey)
}

func (r *RateLimit) clientIP(req *http.Request) string {
	direct := clientIP(req)
	directAddr, err := netip.ParseAddr(direct)
	if err != nil || !containsIP(r.trusted, directAddr.Unmap()) {
		return direct
	}

	forwarded := strings.Split(req.Header.Get("X-Forwarded-For"), ",")
	for i := len(forwarded) - 1; i >= 0; i-- {
		candidate, err := netip.ParseAddr(strings.TrimSpace(forwarded[i]))
		if err != nil {
			return direct
		}
		candidate = candidate.Unmap()
		if !containsIP(r.trusted, candidate) {
			return candidate.String()
		}
	}
	return direct
}

func containsIP(prefixes []netip.Prefix, addr netip.Addr) bool {
	for _, prefix := range prefixes {
		if prefix.Contains(addr) {
			return true
		}
	}
	return false
}

// clientIP returns only the direct peer. Forwarded headers are handled by the
// limiter after it verifies that this address belongs to a trusted proxy.
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(strings.TrimSpace(r.RemoteAddr))
	if err == nil {
		return host
	}
	return strings.TrimSpace(r.RemoteAddr)
}
