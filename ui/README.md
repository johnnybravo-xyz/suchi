# Suchi UI

This directory contains the Svelte SPA served at `/app/`. Production builds
generate an ignored `core/ui/spa/dist/` tree and embed it in the Go binary.

## Development

Generate a fresh bundle and run the backend from the repository root, then start
Vite:

```sh
make run
make ui-dev
```

`make run` regenerates and embeds the production bundle. Vite serves the live
development app on `http://127.0.0.1:5173` and proxies backend routes to
`http://127.0.0.1:8000`.

From the repository root:

- `make ui-check` runs Svelte diagnostics and tests, builds the SPA, verifies
  its source/bundle manifest, and refuses tracked generated assets.
- `make ui-e2e` runs the desktop/mobile Chromium setup smoke tests. Install a
  browser with `bunx playwright install chromium`, or point
  `PLAYWRIGHT_CHROMIUM_EXECUTABLE_PATH` at a local Chromium binary.
- `make ui` installs dependencies and regenerates the ignored
  `core/ui/spa/dist/` tree.
- `make ui-verify` checks that the generated bundle is complete and current.
- `PLAYWRIGHT_PRODUCTION=1 bun run e2e` rebuilds and tests the actual production
  assets through Vite preview. It needs port 5173 free; API mocks remain mocks.

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
