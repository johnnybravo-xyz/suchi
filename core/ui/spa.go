// SPA handler: mount the Svelte UI (built into core/ui/spa/dist/) at
// /app/*. Kept in the same package as the server-rendered UI on
// purpose — both consume the same Server + auth chain, so shared
// state stays in one place. The two coexist during migration; when
// the SPA takes over /, retire the template routes one by one.
//
// The SPA renders its own login and every data call is enforced by
// the API auth chain (session cookie or Token header), same as the
// mobile apps. That's why RequireUI is NOT applied here — the shell
// is public by design; auth is where the data is.

package ui

import (
	"bytes"
	"embed"
	"io/fs"
	"net/http"
	"strings"
)

//go:embed all:spa/dist
var spaFS embed.FS

// spaTitleTag is the exact <title> Vite writes into the built shell.
// Rewritten server-side to add "· Demo" when SUCHI_DEMO_MODE=1 so the
// browser tab reflects the environment. Anchored on the tag so the
// rewrite is a no-op if the source title ever changes.
var spaTitleTag = []byte(`<title>suchi</title>`)
var spaTitleTagDemo = []byte(`<title>suchi · Demo</title>`)

// RegisterSPA mounts the Svelte app at /app. Deep links via the hash
// router work without a server-side catch-all — the URL segments
// live in the fragment identifier, which browsers never send to the
// server.
//
// Requests that match a real asset under spa/dist are served
// verbatim (JS bundle, CSS, index.html). Everything else falls
// through to index.html so the router can decode the hash on the
// client.
func (s *Server) RegisterSPA(mux *http.ServeMux) {
	sub, err := fs.Sub(spaFS, "spa/dist")
	if err != nil {
		// Build-time invariant: spa/dist must exist. If not,
		// register a debug endpoint that says so — better than a
		// silent 404.
		mux.HandleFunc("GET /app/", func(w http.ResponseWriter, r *http.Request) {
			http.Error(w,
				"SPA bundle missing (run `make ui` and rebuild suchi)",
				http.StatusServiceUnavailable)
		})
		return
	}
	files := http.StripPrefix("/app/", http.FileServer(http.FS(sub)))

	// Pre-compute the shell body once. In demo mode we swap the
	// <title>. Falling back to the raw file if either the read or the
	// rewrite fails keeps the /app/ route serving even if the shape
	// drifts.
	shell, err := fs.ReadFile(sub, "index.html")
	if err == nil && s.DemoMode {
		shell = bytes.Replace(shell, spaTitleTag, spaTitleTagDemo, 1)
	}

	mux.HandleFunc("GET /app/", func(w http.ResponseWriter, r *http.Request) {
		p := strings.TrimPrefix(r.URL.Path, "/app/")
		if p != "" {
			// Only serve as an asset if the file actually exists —
			// otherwise fall through to the shell so client-side
			// deep links resolve.
			if _, err := fs.Stat(sub, p); err == nil {
				files.ServeHTTP(w, r)
				return
			}
		}
		// Fresh-instance guard: with the setup token still unclaimed
		// there is no user to sign in as, so the SPA's login form is a
		// dead end. Match the / and /login handlers and route the
		// operator to /bootstrap first. Runs AFTER the asset short-
		// circuit so /bootstrap's own /app/assets/* still resolve.
		if s.SetupPendingFn != nil && s.SetupPendingFn() {
			http.Redirect(w, r, "/bootstrap", http.StatusFound)
			return
		}
		if shell != nil {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = w.Write(shell)
			return
		}
		http.ServeFileFS(w, r, sub, "index.html")
	})
	// Redirect `/app` (no trailing slash) so relative asset URLs in
	// index.html resolve correctly.
	mux.HandleFunc("GET /app", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/app/", http.StatusPermanentRedirect)
	})
}
