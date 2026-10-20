# Working on Suchi

Suchi is a local document archive: capture, extract, file, retrieve, and review.
Optional integrations must preserve useful local operation.

## Sources and ownership

- Read `README.md` and the relevant `docs/` guide. Code, contract tests, and
  `CHANGELOG.md` describe the shipped implementation; `../specs/` also contains
  future proposals and records for other branches.
- Read `docs/architecture.mdx` for the product map and its index of architecture
  guides (runtime, document lifecycle, storage, security, filing, and the
  frontend pages) before changing a boundary.
- `core/` owns application behavior; `plugins/` contains compiled integrations;
  `plugin-api/` is the extension vocabulary; `distro/app/` assembles the server
  and `distro/cmd/suchi/` owns command parsing and process behavior.
- `ui/src/` owns the web app. `make ui` generates the ignored
  `core/ui/spa/dist/` tree for embedded builds; never track that directory.
  `make build`, `make check`, and `make smoke` regenerate it as needed.
- `hack/` owns fixtures, smoke tests, and benchmarks. Standalone benchmark
  modules need `GOWORK=off` when invoked directly.

## Change and commit discipline

- Check `git status --short` and `git worktree list` first. Sibling `suchi/`
  directories may be different branches. Preserve local changes; commit
  frequently, with concise messages and no contribution trailers.
- Use Conventional Commits: `<type>(<scope>): brief imperative message` (scope
  optional), then a blank line and concise `-` bullets covering behavior and
  verification.
- When commits are requested, split work into coherent, independently reviewable
  slices. Keep implementation, UI, and documentation commits separate when each
  can stand on its own.
- Stage explicit paths in a dirty worktree. Never use broad staging commands that
  could include unrelated user changes.
- Repository guidance and executable checks must agree. If `AGENTS.md`,
  documentation, hooks, Make targets, or CI enforce conflicting policies, report
  the contradiction and fix the policy rather than silently choosing one.

## Licensing

- Suchi is dual-licensed: AGPL-3.0 for everyone, commercial for users who
  cannot accept it. Every file must stay licensable under both. Never merge
  outside code unless its author is in `CONTRIBUTORS.md` having agreed to
  `CLA.md`. A DCO or `Signed-off-by` line is not a substitute: it certifies
  provenance, not the right to relicense.
- `plugin-api/` is Apache-2.0, not AGPL, so third-party plugins can carry any
  license. Keep it dependency-free and stdlib-only: importing anything from
  `core/` would pull AGPL code into an Apache module and break that promise.
  Its files carry Apache-2.0 SPDX headers; every other file carries AGPL.
- Source files carry an SPDX line and no copyright line, enforced by
  `make license-check` (wired into `make check`, the CI lint job, and the
  pre-commit hook). The copyright record lives in `NOTICE` and the `README.md`
  License section — keep it there; with no per-file notices those two are the
  only places the holder is named, and dual licensing rests on that record.
- Record every newly bundled or vendored third-party component in `NOTICE` with
  its upstream URL and license, and reproduce the full text where the license
  requires it be carried. A vendored asset served to users needs the notice in
  the served file as well, not only in `NOTICE` — see the banner at the top of
  `core/ui/assets/vendor/oat.min.css`.
- Reject GPL and AGPL dependencies. The tree is permissive-only (MIT, BSD,
  Apache-2.0) and that is what keeps the commercial license possible. Bundle a
  copyleft tool only as a separate process, never linked, and record the
  election and the process boundary in `NOTICE` as msgconvert does.
- The five built-in filing trees in `core/jd/presets/*.toml` are CC0-1.0 and
  already published.
- Johnny.Decimal is a trademark of Coruscade Pty Ltd and its documentation is
  CC BY-NC-SA 4.0, which is incompatible with both of our licenses. Refer to the
  system by name and link to their pages; never copy their prose into ours, and
  never name a product, tier, or SKU after it. Keep the independence disclaimer
  on any page that uses the mark.
- Keep the License sections of `README.md` and `CONTRIBUTING.md` in agreement
  when the model changes, and update `NOTICE` in the same commit as the
  dependency change that motivates it.

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

- Register HTTP token access in `distro/app/serve_token_policy.go`;
  unlisted routes stay session-only. Scopes do not replace role or document ACLs.
- Put document visibility inside list/search SQL, including counts and pages.
- Use the single write pool and enqueue work with its state mutation. Keep
  network calls and subprocesses outside write transactions.
- Preserve immutable original blobs, explicit egress consent, and human edits.
- Keep schema changes in the current unreleased migration; never edit a
  migration already included in a published release.
- No over-engineering. Start from first principles and choose the simplest
  complete design. Prefer flat, explicit code over clever patterns, deep
  hierarchies, or unnecessary indirection. Add abstractions only for a current
  concrete need.
- Before adding a layer, simplify the existing path: reuse current contracts,
  remove obsolete branches and data, and eliminate avoidable work. Add an
  abstraction only when the simpler path cannot meet a current requirement.
- Delete unused code before adding abstractions. Keep ACL, recovery, retry,
  and wire-contract tests; prefer observable assertions over source-text checks.
- Update the relevant public guide and changelog for user-visible changes.
- For architecture changes, update the affected architecture guides in the
  same commit: responsibilities, entry points, data/request/job flow, storage,
  trust/egress boundaries, extension contracts, and verification commands.
  Follow their links to the affected pipeline/plugin/workflow guides rather
  than duplicating those designs. Before handoff, compare docs to the actual
  diff. The hook is only a reminder, not semantic validation.
