# git hooks

Per-clone install:

```
make install-hooks
```

That copies every executable in `hooks/` into `.git/hooks/`. Run it once
after `git clone`, and again after adding a new hook to this dir.

## Hooks that live here

- **pre-commit** — runs `gofmt -l` against every tracked `*.go` file and
  refuses the commit if anything would be reformatted. Matches CI's
  `gofmt` gate exactly, saves a bounce.

No pre-commit *framework* dep — these are plain shell scripts, portable
back to the git-in-1998 hook interface.
