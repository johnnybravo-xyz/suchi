# docs-claims.md — pre-release verification checklist

The README/docs make specific factual claims about suchi's behavior.
Drift between those claims and the code has caused two shipped-in-docs
regressions (backups, CSRF). Run this checklist before every tagged
release.

Each row is a claim + the grep or file to verify it against. When the
grep confirms, tick the box. When it fails, fix the code or the doc
before cutting the release. Never silently soften a doc claim to match
current behavior — that's how a doc rot spiral starts.

## How to use this file

1. `git switch -c release/vX.Y.Z`
2. Walk the sections below top-to-bottom. For each row, run the check
   verbatim and confirm the result.
3. For any failure, open a fix branch and land the correction before
   cutting the tag. Do not check the box until the check passes.
4. Commit the ticked file as part of the release PR so reviewers can
   see the checklist was actually run.

If a row's check has become stale (renamed file, restructured code),
update the check inline. The file is the checklist AND the source of
truth for how each claim is verified.

---

## Security posture (SECURITY.md)

- [ ] **Argon2id passwords.**
      `grep -n 'argon2\.IDKey\|argon2id' plugins/local-auth/*.go`
      → must be present.

- [ ] **API tokens hashed at rest.**
      `grep -n 'sha256\.\|crypto/subtle' core/auth/*.go core/api/*.go`
      → hash + constant-time compare in the token verify path.

- [ ] **Upload body cap enforced via MaxBytesReader.**
      `grep -n 'MaxBytesReader\|MaxBytesError' core/api/*.go core/httpx/*.go`
      → BodyLimit middleware wraps r.Body; upload handler maps the
      typed error to 413.

- [ ] **Parameterized SQL everywhere.**
      `grep -rn "fmt.Sprintf.*SELECT\|fmt.Sprintf.*INSERT\|fmt.Sprintf.*UPDATE" core/ | grep -v _test.go | grep -v ORDER`
      → only ORDER BY interpolation with the allow-list guard is
      allowed. Anything else is a finding.

- [ ] **CSP + frame-ancestors on every UI response.**
      `grep -n "Content-Security-Policy" core/httpx/middleware.go core/ui/*.go`
      → SecurityHeaders sets a default-src 'self' policy with
      `frame-ancestors 'none'`. Preview handler overrides to
      `frame-ancestors 'self'` + `sandbox`.

- [ ] **CSRF: SameSite=Lax + Sec-Fetch-Site middleware.**
      `grep -n "SameSite\|SecFetchSite\|Sec-Fetch-Site" core/httpx/middleware.go plugins/local-auth/*.go`
      → session cookie sets SameSite=Lax; SecFetchSite middleware is
      in the chain in main.go.

- [ ] **Rate limits on auth endpoints.**
      `grep -n "rl.Middleware\|NewRateLimit" distro/cmd/suchi/main.go`
      → every path SECURITY.md lists (login/setup/bootstrap/token/
      share fetch/share download) is wrapped.

- [ ] **Argon2id parameters written down.**
      `grep -n 'argon2.IDKey' plugins/local-auth/*.go`
      → the (time, memory, threads) tuple in code matches the numbers
      quoted in SECURITY.md.

## Reference architecture (docs/reference-architecture.mdx §11)

- [ ] **SQLite two-pool discipline: Write MaxOpenConns=1.**
      `grep -n "SetMaxOpenConns" core/db/db.go`
      → Write pool set to 1, Read pool to 0 (unbounded).

- [ ] **CAS put is content-addressed and immutable.**
      `grep -n "sha256\|CAS\.Put" core/blob/*.go`
      → hash is content-derived; write is idempotent on same content.

- [ ] **VACUUM INTO backup loop.**
      `grep -n "VACUUM INTO\|backup.Loop\|BackupInterval" core/backup/*.go distro/cmd/suchi/main.go`
      → Loop is started from main; Snapshot uses VACUUM INTO from the
      read pool.

- [ ] **Preview 202 gate on high-sensitivity docs.**
      `grep -n "StatusAccepted\|isHighSensitivity\|reveal=1" core/ui/ui.go`
      → gate returns 202 + Cache-Control: no-store; reveal=1 bypasses.

- [ ] **Dead-lettering is audit-logged.**
      `grep -n "audit.Log.*job.dead\|action.*job\\.dead" core/jobs/*.go`
      → markDead emits an audit event.

- [ ] **Job outbox reclaim on boot.**
      `grep -n "ReclaimOrphaned\|boot_reclaimed" core/jobs/*.go distro/cmd/suchi/main.go`
      → ReclaimOrphaned is called before Run in main; excludes
      agent:* kinds.

## Config surface (docs/config.mdx)

- [ ] **Every documented env var exists in core/config/config.go.**
      For each row in the config table, `grep -n "\"<ENV_NAME>\"" core/config/config.go`
      returns a hit.

- [ ] **Every non-secret env var suchi actually reads is documented.**
      `grep -rn 'os.Getenv\|env(' core/config/*.go` — every name
      returned must appear in config.mdx OR be a genuine internal
      (test-only, migration guard).

- [ ] **Defaults in docs match code.**
      For each `env("NAME", "<default>")` in config.go, check the
      config.mdx row's "Default" column matches.

## CLI surface (docs/cli.mdx)

- [ ] **Every `suchi <verb>` documented is dispatched in main.go.**
      `grep -n 'runServe\|runDoctor\|runGC\|runMigrate\|runImporter' distro/cmd/suchi/main.go`

- [ ] **Every subcommand in main's dispatcher is documented.**
      Compare the switch statement in main.go against the `## Verbs`
      list in docs/cli.mdx.

- [ ] **`suchi doctor` sections match the header list in cli.mdx.**
      Section headers printed by runDoctor (== egress ==, == pipeline
      binaries ==, == schema ==, == filesystem ==, == operational
      health ==) all appear in the doc.

## API surface (docs/api.mdx)

- [ ] **Every documented endpoint is registered in api.go / ui.go.**
      For each row in the api.mdx endpoint table, grep for the exact
      `mux.HandleFunc("METHOD /path"` line.

- [ ] **Every registered `/api/…` endpoint is either documented or
      explicitly marked internal.**
      `grep -n 'mux.HandleFunc\|mux.Handle' core/api/api.go`

- [ ] **JD categories endpoint returns the DRF-shaped envelope.**
      `grep -n 'ListJDCategories\|"count":\|"results":' core/api/jd.go`

## Backup + restore (docs/backup-restore.mdx)

- [ ] **The "built-in snapshot loop" section reflects the actual
      audit event action, path pattern, and pruning behavior.**
      `grep -n 'backup.written\|Snapshot(ctx\|prune' core/backup/*.go`

- [ ] **`suchi doctor` really reports last-backup age.**
      `grep -n 'LastSnapshotAge\|last backup' distro/cmd/suchi/doctor.go`

## Importer / ingest (docs/importer.mdx, docs/preconsume.mdx)

- [ ] **Every importer flag documented is parsed in the importer
      code.**
      `grep -n 'flag\\.\\(String\\|Int\\|Bool\\)' core/importer/*.go`

- [ ] **preconsume hook contract in docs matches the code path.**
      `grep -n 'PreconsumeHook\|preconsume' core/pipeline/*.go core/ingest/*.go`

## MCP (docs/mcp.mdx)

- [ ] **Every advertised MCP tool exists in the code.**
      `grep -n 'RegisterTool\|Handler:.*func' core/mcp/*.go`

- [ ] **Every MCP tool in code appears in the doc.**
      Compare the doc's tool table against core/mcp/*.go.

---

## Ratchet notes

If any check *has* to be temporarily silenced (a claim that's true on
main but not yet released), note it here with a target date. Don't
just delete the row.

- (none)

## Recent fixes flagged by this checklist

Log each real drift caught by the checklist here so we notice which
sections rot fastest.

- **2026-08-06 — SECURITY.md rate-limit list drift.** Doc claimed
  share-link password-check was throttled; code didn't wrap the
  handlers. Added the wraps in main.go + expanded the doc row to
  enumerate every path.
