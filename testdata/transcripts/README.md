# transcripts

Golden fixtures recorded by `hack/transcript`. The directory is currently a
fixture destination; no automatic replay suite is shipped.

See `hack/transcript/README.md` for capture instructions. Contract tests
that consume this directory MUST skip when it's empty (`t.Skip` in
each file's package init or a top-level `TestMain`).

## Ground rules

- Never commit fixtures made with `--insecure` (auth headers survive)
- Never commit fixtures containing real personal documents — record
  against a scratch seeded with synthetic content
- Treat committed fixtures as reviewed protocol examples.
