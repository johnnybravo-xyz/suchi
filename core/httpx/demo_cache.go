// Cache-Control policy for demo mode.
//
// The demo instance sits behind Cloudflare with grey-cloud DNS. Bytes-heavy
// read endpoints (blob previews, thumbnails, static SPA shell) are safe
// to cache aggressively — the corpus is baked into the container image
// and the container never mutates seeded rows, so a cached response is
// still correct until the next container roll.
//
// This middleware ONLY fires when SUCHI_DEMO_MODE=1 (compose it
// conditionally in main.go). Everything else keeps the default (no
// caching) so a normal deployment is unchanged.

package httpx

import (
	"net/http"
	"strings"
)

// demoCachePolicies maps a URL path prefix to the Cache-Control header
// value applied to GET/HEAD responses. Order matters — the first prefix
// match wins. Keep specific paths above broader ones.
//
// Cache-Control notes:
//   - public              — cache at shared caches (Cloudflare).
//   - s-maxage=<seconds>  — shared-cache TTL. What Cloudflare honours.
//   - max-age=<seconds>   — browser TTL. Shorter so a user hitting F5
//     on the tab picks up a container roll fast.
//   - immutable           — for content-addressed asset hashes (SPA
//     bundle names) that literally cannot change.
var demoCachePolicies = []struct {
	prefix string
	value  string
}{
	// SPA bundle — filenames are content-hashed by Vite, safe to cache
	// forever. Cloudflare + browser both.
	{"/assets/", "public, max-age=31536000, s-maxage=31536000, immutable"},
	// Blob endpoints. The blob is content-addressed by SHA256 in the
	// CAS, so its bytes literally cannot change. The route parameter
	// is a doc id, and the container's seed guarantees id→hash
	// stability for the duration of the container's life. 1h shared,
	// 60s local so a container roll flushes visitor tabs quickly.
	{"/api/documents/", cachePreviewOrThumb},
	// SPA shell. Short TTL so a deploy is picked up within a minute.
	{"/app/", "public, max-age=60, s-maxage=600"},
	{"/", "public, max-age=60, s-maxage=600"},
}

// cachePreviewOrThumb is a sentinel; DemoCacheControl resolves the actual
// value against the trailing subpath.
const cachePreviewOrThumb = "__PREVIEW_OR_THUMB__"

// DemoCacheControl stamps Cache-Control on responses to a small, safe
// read plane. Skips anything not GET/HEAD, and never overwrites a
// Cache-Control the handler already set.
func DemoCacheControl(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet, http.MethodHead:
		default:
			next.ServeHTTP(w, r)
			return
		}
		policy := lookupDemoCache(r.URL.Path)
		if policy == "" {
			next.ServeHTTP(w, r)
			return
		}
		// Wrap the writer so we stamp Cache-Control only on 2xx —
		// caching a 500 or a 404 would poison the CDN.
		rec := &cacheStamper{ResponseWriter: w, policy: policy}
		next.ServeHTTP(rec, r)
	})
}

// lookupDemoCache resolves the Cache-Control value for path. Returns ""
// when the path is outside the cacheable set. The `/api/documents/`
// entry is only honoured for the /preview, /download, /thumb subpaths
// because the JSON metadata endpoints (list, get, similar) can change
// when a scratch visitor uploads.
func lookupDemoCache(path string) string {
	for _, p := range demoCachePolicies {
		if !strings.HasPrefix(path, p.prefix) {
			continue
		}
		if p.value != cachePreviewOrThumb {
			return p.value
		}
		// /api/documents/* — only cache the blob-ish subpaths.
		if strings.HasSuffix(path, "/preview") ||
			strings.HasSuffix(path, "/download") ||
			strings.HasSuffix(path, "/thumb") {
			return "public, max-age=60, s-maxage=3600"
		}
		return ""
	}
	return ""
}

// cacheStamper is a minimal ResponseWriter shim that stamps Cache-Control
// once the status code is committed. We wrap rather than inline to keep
// the middleware oblivious to whether the inner handler writes a body
// before or after status.
type cacheStamper struct {
	http.ResponseWriter
	policy  string
	stamped bool
}

func (c *cacheStamper) WriteHeader(code int) {
	if !c.stamped {
		c.stamped = true
		if code >= 200 && code < 300 {
			if c.ResponseWriter.Header().Get("Cache-Control") == "" {
				c.ResponseWriter.Header().Set("Cache-Control", c.policy)
				// Vary on Authorization so a user's cached preview
				// never leaks to an anonymous visitor's cache slot
				// (belt-and-suspenders — anon visitors get a bare
				// request and shared-cache Vary already tracks it).
				c.ResponseWriter.Header().Add("Vary", "Authorization")
				c.ResponseWriter.Header().Add("Vary", "X-Suchi-Demo-Token")
			}
		}
	}
	c.ResponseWriter.WriteHeader(code)
}

func (c *cacheStamper) Write(b []byte) (int, error) {
	if !c.stamped {
		c.WriteHeader(http.StatusOK)
	}
	return c.ResponseWriter.Write(b)
}
