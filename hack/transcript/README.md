# transcript

A recording reverse proxy. Point it at any HTTP upstream with `--target`,
and every request/response pair the proxy sees lands in `--out` as a
golden fixture (JSON header block + sidecar blobs for binary bodies).

It is a small `net/http/httputil.ReverseProxy` with a recorder, intended for
capturing synthetic wire examples while investigating client behavior.

## Build

```
cd hack/transcript
go build ./...
```

The tool lives in its own Go module (`github.com/johnnybravo-xyz/suchi/hack/transcript`).
It has zero dependencies beyond the stdlib and is not built into
`dist/suchi`.

## Run

```
./transcript \
  --listen  :8443 \
  --target  https://your-existing-dms.your-lan/ \
  --out     ../../testdata/transcripts
```

Then point the client (mobile app, curl, whatever) at
`http://127.0.0.1:8443/`. Drive it through the interactions you want
captured. Kill the recorder when done; fixtures are already on disk.

### Flags

| Flag         | Default                | Purpose                                                                      |
| ------------ | ---------------------- | ---------------------------------------------------------------------------- |
| `--listen`   | `:8443`                | Address to bind                                                              |
| `--target`   | _required_             | Upstream URL to forward to                                                   |
| `--out`      | `testdata/transcripts` | Fixture directory                                                            |
| `--blob-min` | `4096`                 | Bodies larger than N bytes (or non-textual) spill to `blobs/`                |
| `--insecure` | `false`                | **Debug only.** Skips header redaction. Never commit fixtures made this way. |

## Fixture layout

```
testdata/transcripts/
├── 0001-GET-api-remote_version.json
├── 0002-POST-api-token.json
├── 0003-GET-api-documents.json
├── 0004-POST-api-documents.json
└── blobs/
    ├── 6f5f... (raw PDF that was uploaded)
    └── b3a1... (raw PDF that was downloaded)
```

Each fixture file is a single JSON document:

```json
{
  "seq": 4,
  "recorded_at": "2026-08-05T10:30:15.123456789Z",
  "duration_ms": 218,
  "request": {
    "method": "POST",
    "path": "/api/documents/",
    "query": { "async": ["true"] },
    "headers": {
      "Authorization": ["REDACTED"],
      "Content-Type": ["multipart/form-data; boundary=..."]
    },
    "body": {
      "kind": "blob",
      "blob": "6f5f...",
      "size_bytes": 148231,
      "content_type": "multipart/form-data; boundary=..."
    }
  },
  "response": {
    "status": 201,
    "headers": { "Content-Type": ["application/json"] },
    "body": {
      "kind": "inline",
      "text": "{\"id\": 42, ...}",
      "size_bytes": 187,
      "content_type": "application/json"
    }
  }
}
```

## Redactions

By default the recorder redacts these headers to `["REDACTED"]`:

- `Authorization`, `Cookie`, `Set-Cookie`, `X-Csrftoken`
- Anything starting with `X-Api-`

Any credential leak into the fixture set is a bug — file it and
regenerate that fixture with a scratch account. `--insecure` disables
redaction for debugging only; fixtures recorded that way must be
deleted, not committed.

## Committing fixtures

- Small text bodies inline; binary bodies land under `blobs/`
  keyed by SHA-256 (dedup for free).
- Commit both the JSON files and `blobs/`. They're the contract.
- Never commit `blobs/` content with real personal documents — record
  against a scratch with synthetic docs. `hack/emlfixtures`
  ships PDFs suitable for the upload path.

The tool records only. It does not replay fixtures, terminate TLS, or filter
paths.
