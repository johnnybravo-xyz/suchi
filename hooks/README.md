# git hooks

Per-clone install:

```
make install-hooks
```

That copies the scripts into Git's hooks directory, including when run from a
linked worktree. Linked worktrees share those hooks. Run it once after cloning,
and again after changing a hook.

## Hooks that live here

- **pre-commit** — runs `make fmt-check` over project Go files, including new
  files, excluding Git metadata, frontend dependencies, and vendored code.
  It also reminds you to review architecture docs when staged structural entry
  points change without an architecture-guide update. The reminder is advisory:
  file paths cannot distinguish a bug fix from a new responsibility or flow.
  Tests alone do not trigger it. No extra runtime or hook framework is needed.

When architecture changes, update the backend/frontend guide and affected
linked design guides in the same change. If no architecture changed, record
that conclusion briefly in the commit or PR; do not make a meaningless doc
edit to silence the reminder. Reinstall the hook after changing it.

No pre-commit *framework* dep — these are plain shell scripts, portable
back to the git-in-1998 hook interface.
