# Suchi UI

This directory contains the Svelte SPA served at `/app/`. Production assets
are committed under `core/ui/spa/dist/` and embedded in the Go binary.

## Development

Run the backend from the repository root, then start Vite:

```sh
make run
cd ui
bun install --frozen-lockfile
bun run dev
```

Vite serves the app on `http://127.0.0.1:5173` and proxies backend routes to
`http://127.0.0.1:8000`.

From the repository root:

- `make ui-check` runs Svelte diagnostics, builds the SPA, and verifies that
  the committed embedded assets are current.
- `make ui-e2e` runs the desktop/mobile Chromium setup smoke tests. Install a
  browser with `bunx playwright install chromium`, or point
  `PLAYWRIGHT_CHROMIUM_EXECUTABLE_PATH` at a local Chromium binary.
- `make ui` installs dependencies, builds the SPA, and refreshes
  `core/ui/spa/dist/`.

Use `bun run check` for diagnostics without a production build. `bun.lock` is
the only frontend lockfile and is checked by the root Make targets.

## Layout

- `src/App.svelte` owns the application shell.
- `src/routes/` contains route-level views.
- `src/lib/` contains shared components, API helpers, router, and session state.
- `src/app.css` contains the design system and component styles.
- `vite.config.js` defines the development proxy and deterministic asset names.

See [`docs/spa-architecture.mdx`](../docs/spa-architecture.mdx) for routing,
state, API, accessibility, and security conventions.
