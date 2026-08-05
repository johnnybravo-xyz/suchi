# Contributing to suchi

This doc is the operating manual — issue filing, PR flow, coding
conventions, testing, docs, and the DCO gate. Skim before your
first PR; the rules exist for reasons that show up in the design
doc, not because process is fun.

**Status**: pre-alpha, private repo. Until then this doc is
aspirational for the maintainer; useful to have in place so day
one of "public" isn't day one of "figure out how contributions work".

## Table of contents

1. [Code of conduct](#code-of-conduct)
2. [Where discussion happens](#where-discussion-happens)
3. [Filing issues](#filing-issues)
4. [Sending PRs](#sending-prs)
5. [DCO sign-off](#dco-sign-off)
6. [Commit convention](#commit-convention)
7. [Coding conventions](#coding-conventions)
8. [Testing](#testing)
9. [Docs updates](#docs-updates)
10. [Plugin authoring](#plugin-authoring)
11. [Security disclosure](#security-disclosure)
12. [License + IP](#license--ip)

## Code of conduct

Treat maintainers, reviewers, and other contributors with respect.
Disagreement is fine and encouraged; personal attacks, harassment,
and dismissive behavior are not. If a thread stops being productive,
a maintainer will lock it and move the discussion elsewhere.

We don't have a separate CoC document because suchi is small enough
that the maintainer moderates directly. If that changes, this
section grows into one.

## Where discussion happens

- **GitHub Issues** — bugs, feature requests, design questions with
  a concrete artifact to point at.
- **GitHub Discussions** — architecture RFCs, cross-cutting design
  proposals, "how do others use X". Not enabled yet; wired at the
  repo-flip.
- **PR comments** — reviews of specific code changes.

We don't have a Discord or Slack — async, indexable, zero
moderation overhead is a deliberate choice per the plan doc.

## Filing issues

Before opening a new issue:

1. **Search existing** — bug + feature. Duplicates are cheap to
   avoid.
2. **Reproduce on latest `main`** — a fixed bug is not a bug.
3. **Include the version** — `suchi version` output.

### Bug reports

Include:

- What you did (curl command, upload, config change).
- What you expected.
- What happened instead — full error message, `X-Request-Id` header
  if there's an HTTP request involved.
- Relevant log lines (stderr JSON block; grep by request_id).
- `suchi doctor` output if the issue smells environmental.

If it's a data-loss or corruption bug, DO NOT close the issue as
"resolved" without a full postmortem in the audit log.

### Feature requests

Frame the use case, not the solution. "I need to filter search by
document age" beats "add a `date_range` param to the search
endpoint" — the former lets the design conversation happen; the
latter jumps to implementation before the maintainers understand
the shape.

Feature requests that fit an existing package or endpoint get moved
forward faster than ones requiring new architecture. If the request
implies a new plugin kind, a new subsystem, or a change to the
plugin ABI, expect a design discussion first.

### "Won't fix" is a valid answer

Some requests are outside suchi's stated identity (see
[comparison](docs/comparison.mdx) — "where suchi wins" section).
Cloud storage as a first-class primary store, tenant sharding at
the DB level, and a rich WYSIWYG editor are examples of things
suchi is not trying to be. Won't-fix is not personal; it keeps the
project shipping.

## Sending PRs

### Setup

```sh
git clone git@github.com:suchi-dms/suchi.git
cd suchi
make install-hooks     # pre-commit hook: runs gofmt on staged files
```

### Branch names

`type/short-description` — `feat/tag-search-facet`,
`fix/preview-race`, `docs/refile-guide`. No `feature/` prefix
(Git doesn't care and the shorter form is greppable in `git
branch --list`).

### PR size

- **Small PRs get merged fast.** A PR that touches 3 files and adds
  200 lines is easier to review than one touching 30 files with
  2000 lines, regardless of the underlying work quantity.
- If a change genuinely spans multiple concerns, split into a
  **stacked PR series**. Each PR must be independently mergeable
  and CI-green.
- Refactors ride separately from behavior changes. "This PR cleans
  up the layer AND adds the feature" is a review anti-pattern.

### PR checklist

Before requesting review:

- [ ] `make check` passes locally (fmt + lint + tests). No excuses.
- [ ] CHANGELOG.md `[Unreleased]` section has an entry (unless the
      PR is `docs`-only or `refactor`-only with zero user-visible
      change).
- [ ] Every code change has a corresponding docs update if it
      touches: CLI flags, env vars, endpoints, config file schema,
      user-visible behavior. Docs and code ship in the same PR —
      not a follow-up.
- [ ] Commit messages follow the [Conventional Commits](docs/release-process.mdx#commit-convention)
      convention.
- [ ] Every commit is signed off (`git commit -s`) — see
      [DCO sign-off](#dco-sign-off).
- [ ] Tests exist for new behavior. Rules:
  - New handler → happy-path integration test + one refusal case
  - New CLI subcommand → smoke test running the binary against a
    temp DATA_DIR
  - New pipeline step → unit test with a golden fixture
- [ ] The PR description references the issue (`Fixes #123`) or
      states "no issue" and explains why (usually "trivial fix").

### Review

- The maintainer reviews within a week. Longer requests get flagged
  as "in review — expected finish date" so you're not left
  guessing.
- Review comments fall into three buckets:
  - **Blocking** — must-fix before merge. Explicitly marked.
  - **Question** — a clarification needed. Answering may or may
    not require a change.
  - **Nit** — style, phrasing, might-consider. Take or leave.
- Force-pushes during review are fine as long as the PR is a
  single-author change. For multi-author PRs, prefer additive
  commits so review threads stay attached to lines.
- Squash-merge on close. The PR body becomes the commit body —
  write it accordingly.

### After merge

- Delete the branch. GitHub's UI does it in one click.
- If the change is user-visible, watch `/api/tasks/` (or the
  smoke workflow) on `main` for the next 24h — a regression that
  slips through review usually surfaces there.

## DCO sign-off

Every commit that lands in `main` must carry a
[Developer Certificate of Origin](https://developercertificate.org/)
sign-off:

```
Signed-off-by: Ritesh Shrivastav <ritesh@example.com>
```

Automated via `git commit -s` — set your `user.email` in git config
and you're done. CI enforces the trailer at PR time; a PR without
DCO trailers on every commit gets a bot comment asking for a rebase.

We chose DCO over a CLA on purpose:

- No paperwork. No lawyer round-trip. Every contributor asserts
  authorship + license eligibility with one line per commit.
- The AGPL-3.0 license already handles the licensing side.
- Rebase and cherry-pick preserve sign-offs, so the trailer travels
  with the code.

If you contribute on behalf of your employer, ensure your employer
knows and permits it — DCO §3 covers this.

## Commit convention

See [`docs/release-process.mdx`](docs/release-process.mdx#commit-convention)
for the full spec. Short version:

```
<type>(<scope>)!?: <short summary>

<optional body>

<optional footer, including BREAKING CHANGE: … and Signed-off-by: …>
```

Types: `feat`, `fix`, `perf`, `refactor`, `docs`, `test`, `build`,
`chore`. Scopes are package names (`authz`, `automations`, `ui`,
`api`, etc.). `!` after the scope + `BREAKING CHANGE:` footer for
breaking changes.

Examples pulled from the log:

- `feat(refile): "come back and change your mind" — bulk re-run rules + re-render across the corpus`
- `feat(authz): Phase 6 batch 2 — enforce document ACLs`
- `docs: full-codebase sweep covering Phase 4–6 delivery`

## Coding conventions

### Language

- Go 1.25+. `plugin-api/` targets 1.24 to stay maximally consumable.
- `CGO_ENABLED=0`. Every dep must build without a C toolchain.
- Prefer stdlib. Every third-party package must be justified in the
  PR description ("stdlib doesn't cover X, and Y is 200 lines vs
  the alternative library").

### Formatting

- `gofmt` is the source of truth. `make fmt` runs it repo-wide.
- The pre-commit hook blocks unformatted commits. `make
install-hooks` wires it — do this before your first commit.
- `staticcheck` clean. `make lint` runs it; auto-installs if
  missing.

### Package layout

- `core/<name>/` — internal library. Depends only on `plugin-api`
  - stdlib + explicitly-vetted third-party deps.
- `plugins/<name>/` — reference plugins. Own `go.mod`. Blank-imported
  from `distro/cmd/suchi`.
- `distro/cmd/suchi/` — the binary. Pins versions.
- `hack/` — local dev harnesses. Own `go.mod`s. Not shipped.

### Naming

- Package name = directory name. Lowercase, one word if possible.
- Exported types/functions have godoc comments explaining **why**,
  not just repeating the signature.
- URLs, Go package names, file names, and handler function names
  should match the domain concept. See
  [`feedback_easy_to_get_around`](https://github.com/suchi-dms/suchi/blob/main/docs/permissions.mdx)
  precedent — clarity beats compat when they collide.

### SQL

- Every SQL string that reaches `Exec` / `Query` uses `?` bind
  parameters. No string concatenation. Ever.
- Migrations are additive and numbered `NNNN_short_name.sql` under
  `core/db/migrations/`. Once applied, migrations are immutable —
  a mistake gets a new migration, not an edit. Pre-alpha exception:
  we amend in-place when there are zero deployed DBs; this stops
  the day we cut the first tag.
- `STRICT` on every new table. `CHECK` constraints for closed
  vocabularies. Foreign keys with `ON DELETE CASCADE` or `SET NULL`
  spelled out explicitly.

### Errors

- Return `error`, not sentinel values or panics.
- Wrap with `fmt.Errorf("context: %w", err)` when the caller might
  want to `errors.Is` the underlying.
- Log at `Warn` for recoverable, `Error` for "the operator needs
  to look at this". `Info` is happy path.

### Comments

- Explain **why**, not **what**. The code says what.
- Document invariants, non-obvious ordering, and the reasons a
  boring-looking piece of code is actually load-bearing.
- Avoid TODO comments; open an issue instead.

### Tests

- `_test.go` files sit next to the code they test.
- Table-driven for closed-vocabulary cases. Named subtests
  (`t.Run("case name", ...)`).
- No sleep loops. If you need to wait for a background thing, use
  a channel or a poll with `context.WithTimeout`.
- Integration tests that hit the DB use `t.TempDir()` + a fresh
  migration run.

## Testing

Three tiers:

- **Unit** — `make test` runs `go test -count=1 ./...` per module.
  Every PR must be green.
- **Smoke** — `make smoke` builds a binary, boots it on `:8765`,
  hits `/healthz` + `/readyz`. `just smoke-ingest` + `just
smoke-anydoc` exercise the ingest pipelines.
- **Full-image E2E** — `.github/workflows/smoke.yml` in CI.
  Builds the Docker full image, boots against a throwaway volume,
  ingests a Ghostscript-generated PDF, asserts pipeline output via
  HTTP. This catches wrapper-vs-real-binary mismatches nothing
  else does.

If your PR touches:

- **A handler** — add an integration test that exercises the
  handler with a live DB. Auth, empty-body, happy path, one error
  case.
- **A pipeline step** — add a golden-fixture test under the step's
  own `_test.go`. Real bytes in, expected output bytes/JSON out.
- **A CLI subcommand** — smoke test that runs `go run
./distro/cmd/suchi <subcommand>` against a temp `DATA_DIR`.
- **A migration** — the migration itself is the test. Verify by
  running against a fresh DB and against a DB at the prior schema
  version.

## Docs updates

**Docs and code ship together, not as a follow-up.** This is
non-negotiable per the memory
[`feedback_docs_alongside_code`](docs/index.mdx).

Checklist for a user-visible change:

- **New / renamed CLI flag** → `docs/cli.mdx` + `distro/cmd/suchi`
  usage string
- **New / renamed env var** → `docs/config.mdx` + `.example.toml` /
  `.env.example`
- **New / renamed endpoint** → `docs/api.mdx` + endpoint-index
  table at the top + OpenAPI spec (`core/api/schema.json`)
- **New / changed feature** → the relevant `docs/*.mdx` guide + the
  index-page card if it's headline
- **New importer flag** → `docs/importer.mdx`
- **Breaking change** → CHANGELOG.md `[Unreleased]` block with the
  ⚠ Migration callout

Docs use Mintlify MDX. `cd docs && bunx mint dev` for a live
preview at `http://127.0.0.1:3000`. `bunx mint broken-links` before
you push.

## Plugin authoring

See [`docs/plugins.mdx`](docs/plugins.mdx) for the current guide.
Three interfaces in `plugin-api/`:

- `Authenticator` — read the request, return a Principal.
- `Subscriber` — receive durable-outbox events (post-ingest, render,
  workflow-advance, etc.).
- `AuditSink` — receive audit events for external emission.

For a Subscriber:

- Handle MUST be idempotent — the outbox retries with backoff.
- Kinds() is called once at registration; return a stable slice.
- Panic-free is the invariant. Panics get logged; the job goes to
  the dead-letter table.

Third-party plugins ship in their own module with their own
`go.mod`. Vendor into `distro/cmd/suchi/main.go` via blank-import
when you want the plugin in the shipped binary; leave it out for
opt-in distros.

## Security disclosure

**Do not open a public issue for security bugs.** See
[`SECURITY.md`](SECURITY.md) for the disclosure address, response
SLA, and coordination protocol.

## License + IP

- **Code**: [AGPL-3.0](LICENSE). By contributing you agree your
  contribution is licensed under AGPL-3.0.
- **Docs**: same repo, same license unless a specific file states
  otherwise.
- **DCO sign-off** on every commit is your assertion that you have
  the right to contribute under this license (see
  [DCO §1](https://developercertificate.org/)).

If you're contributing content that includes third-party code or
assets, note the origin in the PR description and link the
upstream license. Vendored third-party code goes under a clearly
labeled path with the upstream LICENSE file preserved.

## First-issue guidance

Once the repo goes public, look for the `good-first-issue` label.
Typical first-issue shape:

- A single package touch
- Tests already exist and pass
- The issue description spells out the acceptance criteria

If nothing labeled `good-first-issue` looks interesting, ask in a
Discussion — often there's an unlabeled issue that fits.

Thanks for reading this far. See you on `main`.
