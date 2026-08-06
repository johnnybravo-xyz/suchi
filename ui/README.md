# suchi-ui

A modern frontend for suchi. Svelte 5 + Vite, **zero runtime dependencies**:
no component library, no router package, no icon package, no webfonts.
The whole app is **~31 KB of gzipped JS + ~3 KB of CSS** — smaller than most
sites' cookie banners, in keeping with the one-binary ethos.

## Why this shape

- **Plain Svelte, not SvelteKit.** The Go binary is the server; the UI is a
  static SPA it embeds and serves. Kit's SSR/routing layer would be dead weight.
- **Hash routing** (`#/documents`, `#/doc/42`). Works when embedded at any
  path behind any reverse proxy with zero server-side route configuration,
  and deep links survive without a catch-all handler.
- **Same API as the mobile apps.** Everything goes through the documented
  HTTP surface (`Authorization: Token …` or the session cookie). No private
  endpoints were added for this UI — if the UI can do it, an agent can too.
- **Design system = the brand.** Paper/ink/manila tokens, accent `#0575B6`
  (dark mode: gunmetal + lifted `#4FA8DC`), and the signature element is the
  logo made functional: every document row is a dotted index row — leading
  status dot, JD chip, title. System font stack by default; if you serve
  Schibsted Grotesk / Spline Sans Mono, the CSS picks them up automatically.

## What's implemented

| Screen | Wired to |
|---|---|
| Login | `POST /api/login` (JSON → token) + cookie sessions via `GET /api/whoami` |
| Documents | `GET /api/documents/` — facet filters (`tags__id__in`, `correspondents__id__in`, `document_type__id`, `sensitivity`, `jd_category_id`), ordering, DRF pagination |
| Inbox | Same list scoped to the inbox JD category, with a quick "File under…" action (`PATCH`) per row |
| Document | `GET /api/documents/{id}` + `/preview/{id}` iframe, sensitivity **blur with reveal**, rename/refile/sensitivity `PATCH`, versions, share link (`POST /api/share_links/`), download, trash |
| Search | `GET /api/search/` with `<mark>` snippets (sanitized), pagination; `GET /api/autocomplete/` powers the palette |
| Approvals | `GET /api/tasks/?include=workflow` + approve/reject via `POST /api/approvals/tasks/{id}/resolve`; dead jobs surfaced from the outbox |
| Automations | List/create/edit/toggle/delete via `/api/automations/` (JSON spec editor with a template) |
| Upload | Drag-and-drop multi-file `POST /api/documents/` with per-file dedup/restore feedback |
| Settings | whoami, API-token mint/revoke (`/api/tokens/`) |
| ⌘K palette | Page jump + document autocomplete |

Dark/light theme (system default, sidebar toggle), keyboard focus states,
`prefers-reduced-motion` respected, empty states with a next action.

## Development

```bash
just serve          # the Go binary on :8000, in the suchi repo
npm install         # three build-time deps (svelte, vite, plugin) — see note below
npm run dev         # vite on :5173, proxying /api /preview /download /login to :8000
```

## Integrating into the suchi repo

### 1. Where the code lives

This project goes at the repo root as `ui/` — a sibling of `core/`, not
inside `core/ui/` (that's the Go package):

```bash
mv suchi-ui ui
printf 'ui/node_modules/\nui/dist/\n' >> .gitignore
```

### 2. Commit the built output, not the toolchain

Build artifacts go to `core/ui/spa/dist/` and get **committed** (~95 KB,
three files). This keeps `go build ./...` working with no Node installed —
contributors and CI never touch npm unless they change the UI. Mark them
generated so diffs stay readable:

```bash
echo "core/ui/spa/dist/** linguist-generated=true" >> .gitattributes
```

### 3. The embed + handler

New file `core/ui/spa.go`:

```go
package ui

import (
	"embed"
	"io/fs"
	"net/http"
	"strings"
)

//go:embed all:spa/dist
var spaFS embed.FS

// RegisterSPA mounts the Svelte app at /app. It is served without
// RequireUI — the SPA renders its own login and every data call is
// enforced by the API's auth chain, same as the mobile clients.
func (s *Server) RegisterSPA(mux *http.ServeMux) {
	sub, _ := fs.Sub(spaFS, "spa/dist")
	files := http.StripPrefix("/app/", http.FileServer(http.FS(sub)))
	mux.HandleFunc("GET /app/", func(w http.ResponseWriter, r *http.Request) {
		p := strings.TrimPrefix(r.URL.Path, "/app/")
		if p != "" {
			if _, err := fs.Stat(sub, p); err == nil {
				files.ServeHTTP(w, r)
				return
			}
		}
		// everything else is the shell; the hash router takes over
		http.ServeFileFS(w, r, sub, "index.html")
	})
	mux.HandleFunc("GET /app", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/app/", http.StatusPermanentRedirect)
	})
}
```

### 4. One line in `distro/cmd/suchi/main.go`

Inside the existing `!cfg.UIDisabled` block, right after
`uiSrv.Register(mux)`:

```go
uiSrv.RegisterSPA(mux)
```

`SUCHI_UI_DISABLED=1` keeps its exact meaning — headless skips both UIs.

### 5. Justfile recipes

```make
# --- spa ---

# Rebuild the Svelte UI and refresh the embedded copy.
ui-build:
    cd ui && npm ci && npm run build
    rm -rf core/ui/spa/dist && mkdir -p core/ui/spa
    cp -r ui/dist core/ui/spa/dist
    @echo "embedded $(du -sh core/ui/spa/dist | cut -f1) — commit core/ui/spa/dist"

# Vite dev server proxying to a running `just serve`.
ui-dev:
    cd ui && npm run dev
```

Then: `just ui-build && just build && just serve` and open
`http://127.0.0.1:8000/app/`.

### 6. Why /app first, not /

Low-risk migration: the server-rendered UI keeps serving `/` untouched and
the SPA runs beside it against the same session cookie. The later flip is
small — point `GET /` at the SPA handler in `Register()` and retire the
template routes one by one. `/preview/{id}`, `/download/{id}`, `/login`,
and `/s/` stay exactly where they are; the SPA depends on them. `RequireUI`
does not apply to the SPA shell (static and public by design; auth is
enforced where the data is, which is how the mobile apps already work).

### 7. CI

Nothing changes on day one — committed `dist/` means the Go jobs stay
self-sufficient. When UI changes become regular, add a job gated on
`paths: [ui/**]` that runs `npm ci && npm run build` and diffs the result
against `core/ui/spa/dist` — that catches the one real failure mode of
committing the dist (editing `ui/` and forgetting `just ui-build`).

**CSP note:** the app ships no inline scripts. Svelte injects styles at
runtime, so either allow `style-src 'unsafe-inline'` for the UI routes or
build with a nonce; everything else works under a strict policy.

## The JD surface (server contract)

The sidebar tree, list-row chips, and refile pickers consume:

```
GET /api/jd/categories/            standard DRF envelope
  results: [{ id, code, name, description, area_code, area_name, system }]
  ?q=<prefix>     code OR name, case-insensitive
  ?area=<code>    scope to one area (start code, e.g. 20)
```

plus enriched document projections on list and detail:
`jd_category_code`, `jd_category_name`, `jd_area_name` (no `jd_area_code` —
the category code already encodes it). The UI degrades gracefully where
the enrichment is absent (raw id chip, tree hidden).

## Supply-chain note

Exactly three dev-time npm packages (`svelte`, `@sveltejs/vite-plugin-svelte`,
`vite`), pinned in `package-lock.json`. Nothing from npm ships to the browser
except Svelte's compiled runtime. Review the lockfile before vendoring; there
are no postinstall scripts in this tree. Icons are hand-drawn inline SVG paths;
fonts are the system stack.
