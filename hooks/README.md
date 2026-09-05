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

No pre-commit *framework* dep — these are plain shell scripts, portable
back to the git-in-1998 hook interface.
