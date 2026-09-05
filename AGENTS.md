# Working on Suchi

Suchi is a local document archive: capture, extract, file, retrieve, and review.
Optional integrations must preserve useful local operation.

## Sources and ownership

- Read `README.md` and the relevant `docs/` guide. Code, contract tests, and
  `CHANGELOG.md` describe the shipped implementation; `../specs/` also contains
  future proposals and records for other branches.
- Read `docs/architecture.mdx` for the product/backend map and
  `docs/spa-architecture.mdx` for browser ownership before changing a boundary.
- `core/` owns application behavior; `plugins/` contains compiled integrations;
  `plugin-api/` is the extension vocabulary; `distro/cmd/suchi/` wires it.
- `ui/src/` owns the web app. Commit its generated `core/ui/spa/dist/` bundle
  with source changes so Go builds do not need Bun.
- `hack/` owns fixtures, smoke tests, and benchmarks. Standalone benchmark
  modules need `GOWORK=off` when invoked directly.
- Check `git status --short` and `git worktree list` first. Sibling `suchi/`
  directories may be different branches. Preserve local changes; commit only
  when asked, with concise messages and no contribution trailers.

## Verification loop

- Start with affected packages: `go test ./core/<package>`. Use `-race` for
  concurrency changes.
- Frontend: `cd ui && bun run check && bun test src`; exercise changed browser
  behavior with `bun run e2e --grep '<behavior>'`.
- Run `make ui` after frontend edits, then `make check` before handoff. This
  checks all Go modules, UI diagnostics/tests/build, and embedded bundle parity.
- Tests reuse Go's cache. `make test TEST_FLAGS='-count=1 -timeout 60s'` forces
  re-execution. Use a writable `GOCACHE` in a restricted environment.
- Runtime changes need `make smoke`; ingestion changes need the relevant format
  smoke test. Use focused benchmarks for small performance changes and
  `make bench-check` for binary/startup/memory guardrails.
- Report checks that could not run. Mocked browser API tests do not establish
  server compatibility, and unit tests do not establish real OCR/device support.

## Invariants

- Register HTTP token access in `distro/cmd/suchi/serve_token_policy.go`;
  unlisted routes stay session-only. Scopes do not replace role or document ACLs.
- Put document visibility inside list/search SQL, including counts and pages.
- Use the single write pool and enqueue work with its state mutation. Keep
  network calls and subprocesses outside write transactions.
- Preserve immutable original blobs, explicit egress consent, and human edits.
- Keep schema changes in the current unreleased migration; never edit a
  migration already included in a published release.
- Delete unused code before adding abstractions. Keep ACL, recovery, retry,
  and wire-contract tests; prefer observable assertions over source-text checks.
- Update the relevant public guide and changelog for user-visible changes.
- For architecture changes, update the affected architecture guides in the
  same commit: responsibilities, entry points, data/request/job flow, storage,
  trust/egress boundaries, extension contracts, and verification commands.
  Follow their links to the affected pipeline/plugin/workflow guides rather
  than duplicating those designs. Before handoff, compare docs to the actual
  diff; if architecture is unchanged, say so in the commit or PR. The hook is
  only a reminder, not semantic validation.
