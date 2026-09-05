# Contributing to suchi

Suchi is a v0.1 beta candidate. Interfaces may change while the product is
still pre-1.0, so prefer clear designs over compatibility layers.

## Get started

The workspace uses Go 1.27.0 or newer. UI changes also require Bun 1.4.1.

```sh
git clone git@github.com:johnnybravo-xyz/suchi.git
cd suchi
make install-hooks
make check
```

Run `make run` to build and serve a development instance at
`http://127.0.0.1:8000`.

## Before opening an issue

- Search existing issues and reproduce the problem on current `main`.
- Include `suchi version`, reproduction steps, the expected result, and the
  actual error or relevant log lines.
- Describe the use case behind feature requests. This leaves room to find the
  smallest design that solves it.
- Report security issues privately as described in [SECURITY.md](SECURITY.md).

## Pull requests

Keep changes focused and consistent with the existing package boundaries.
Before requesting review:

- Run `make check`.
- Add focused tests for changed behavior.
- Run `make smoke` for runtime, configuration, or deployment changes.
- Update user documentation and `CHANGELOG.md` with user-visible changes.
- Rebuild and commit the embedded SPA with `make ui` for UI changes.

Do not mix unrelated refactors into a feature or fix. A new package should own
a distinct responsibility, not merely shorten another file.

## Code conventions

- Use `gofmt`; `make check` also runs tests, vet, Staticcheck, and UI checks.
- Prefer the standard library and existing project helpers. Explain any new
  dependency in the pull request.
- Routine dependency bumps stop at the previous stable release after a
  changelog review; existing newer pins are not downgraded, and security fixes
  and base-image patches take precedence.
- Return and wrap errors instead of panicking. Comments should explain
  invariants or non-obvious decisions.
- Use SQL bind parameters. New tables use `STRICT`, explicit constraints, and
  explicit foreign-key deletion behavior.
- Keep tests beside the code. Prefer deterministic tests, `t.TempDir()`, and
  named table-driven cases where they fit.
- Keep network calls outside database transactions and make durable job
  handlers idempotent.

The main ownership boundaries are:

- `core/`: application libraries and services
- `plugins/`: compile-linked reference plugins
- `plugin-api/`: public plugin interfaces
- `distro/cmd/suchi/`: binary assembly and CLI
- `ui/`: Svelte application embedded into the binary
- `hack/`: development and benchmark tools

## Useful checks

```sh
make test         # all Go modules
make ui-check     # Svelte checks, tests, build, and embedded bundle diff
make smoke        # build and probe a running server
make bench-check  # binary, startup, memory, and goroutine guardrails
make security-check # release-time Go and frontend advisory scan
```

`make test` reuses Go's cache for unchanged packages. To force a fresh run,
use `make test TEST_FLAGS='-count=1 -timeout 60s'` (also used by CI). For a
focused backend edit, start with `go test ./core/<package>`; use `-race` when
changing concurrency. The module checks include the standalone benchmark tools.

`make smoke` waits for a fresh server to become ready and removes its temporary
data on exit. Set `PORT` to use a different local port.

Tests should cover the contract being changed: authorization and refusal cases
for handlers, validated input and output for pipeline steps, and live reload or
restart behavior for configuration changes.

## Commits and documentation

Use a short [Conventional Commit](https://www.conventionalcommits.org/en/v1.0.0/)
subject, such as `fix(mail): reload account settings`. Add a brief body only
when the reason is not obvious. No contribution trailer is required.

Documentation ships with the behavior it describes:

- Read [Backend architecture](docs/architecture.mdx) and
  [Frontend architecture](docs/spa-architecture.mdx) before changing boundaries.
- Architecture changes update those guides and affected linked design guides
  in the same change: ownership, entry points, data flow, storage, trust/egress,
  and verification. If none changed, note that in the commit or PR. The local
  hook reminds on structural paths but cannot validate architectural meaning.
- CLI or config changes update `docs/cli.mdx` or `docs/config.mdx`.
- API changes update `docs/api.mdx`.
- Feature changes update the relevant guide.
- User-visible changes update `CHANGELOG.md`.

See [the release process](docs/release-process.mdx) for tagging and publishing.

## License

Contributions are accepted under [AGPL-3.0](LICENSE). Identify third-party
code or assets and preserve their upstream license notices.
