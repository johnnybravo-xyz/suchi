#!/usr/bin/env bash
#
# hack/smoke-anydoc-docx.sh — end-to-end verify that a docx ingest hits
# anydoc and lands non-empty content. Builds the standard image, boots a
# throwaway container against /tmp/suchi-smoke-anydoc, bootstraps the
# admin, uploads a hand-crafted minimal docx, waits for post-ingest,
# and asserts documents.content is non-empty and contains the marker
# string.
#
# Not for CI (needs docker, ~3-5 minutes cold). Run manually after
# changing anydoc-side code or bumping ANYDOC_TAG.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
IMAGE="${IMAGE:-suchi:standard}"
DATA_DIR="${DATA_DIR:-/tmp/suchi-smoke-anydoc}"
PORT="${PORT:-8765}"
BASE="http://127.0.0.1:$PORT"
EMAIL="you@example.com"
PASSWORD="local-smoke-passwd"
MARKER="RUSTIC_FLAMINGO_QUANTUM_TROMBONE"  # unlikely-to-hit-elsewhere marker

trap 'docker rm -f suchi-smoke-anydoc >/dev/null 2>&1 || true' EXIT

echo "== build =="
docker build --target standard -t "$IMAGE" "$ROOT"

echo
echo "== fresh DATA_DIR =="
rm -rf "$DATA_DIR"
mkdir -p "$DATA_DIR"

echo
echo "== boot standard =="
docker rm -f suchi-smoke-anydoc >/dev/null 2>&1 || true
# Run as the host UID so writes to the bind-mounted DATA_DIR succeed.
# The image bakes UID 65532 for named volumes; host bind-mounts don't
# inherit that ownership, so a --user override is the friction-free
# path (the Go binary is CGO_ENABLED=0 and doesn't care about UIDs).
docker run -d --name suchi-smoke-anydoc \
  --user "$(id -u):$(id -g)" \
  -p "$PORT:8000" \
  -e PUBLIC_URL="$BASE" \
  -e HOME=/tmp \
  -v "$DATA_DIR:/data" \
  "$IMAGE" >/dev/null

for _ in $(seq 1 60); do
  if curl -sf "$BASE/healthz" >/dev/null; then break; fi
  sleep 0.5
done
if ! curl -sf "$BASE/healthz" >/dev/null; then
  echo "suchi never came up:"; docker logs suchi-smoke-anydoc | tail -50; exit 1
fi
echo "suchi up on :$PORT"

echo
echo "== confirm anydoc is on PATH inside the image =="
# anydoc's example CLI has no --help handler — it treats any argv as
# the positional <file> arg. Sanity-check by running with no args:
# the binary should print USAGE and exit non-zero. Any other outcome
# (segfault, missing binary, ELF format error) means the anydoc-build
# stage produced something broken.
USAGE_OUT=$(docker exec suchi-smoke-anydoc anydoc 2>&1 || true)
if ! echo "$USAGE_OUT" | grep -qi "usage"; then
  echo "anydoc no-arg check didn't print USAGE — binary is broken"
  echo "  got: $USAGE_OUT"
  docker exec suchi-smoke-anydoc which anydoc || echo "  not on PATH"
  exit 1
fi
echo "  binary OK — usage line: $USAGE_OUT"

echo
echo "== bootstrap admin =="
TOKEN=$(docker logs suchi-smoke-anydoc 2>&1 \
        | grep 'localauth.setup.token_minted' \
        | grep -oP '"token":"\K[^"]+' | head -1)
if [ -z "${TOKEN:-}" ]; then
  echo "setup token not found in log"; docker logs suchi-smoke-anydoc | tail -30; exit 1
fi
curl -sf -X POST "$BASE/setup" \
     -H 'Content-Type: application/json' \
     -d "{\"token\":\"$TOKEN\",\"email\":\"$EMAIL\",\"password\":\"$PASSWORD\"}" \
     >/dev/null

API_TOKEN=$(curl -s -X POST "$BASE/api/login" \
    -H 'Accept: application/json' -H 'Content-Type: application/json' \
    -d "{\"username\":\"$EMAIL\",\"password\":\"$PASSWORD\"}" \
    | grep -oP '"token":"\K[^"]+')
if [ -z "$API_TOKEN" ]; then
  echo "no API token"; exit 1
fi

echo
echo "== craft minimal .docx with marker $MARKER =="
DOCX="$DATA_DIR/smoke.docx"
STAGE="$(mktemp -d)"
trap 'rm -rf "$STAGE"; docker rm -f suchi-smoke-anydoc >/dev/null 2>&1 || true' EXIT

mkdir -p "$STAGE/_rels" "$STAGE/word/_rels"

cat > "$STAGE/[Content_Types].xml" <<'EOF'
<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types">
  <Default Extension="rels" ContentType="application/vnd.openxmlformats-package.relationships+xml"/>
  <Default Extension="xml" ContentType="application/xml"/>
  <Override PartName="/word/document.xml"
            ContentType="application/vnd.openxmlformats-officedocument.wordprocessingml.document.main+xml"/>
</Types>
EOF

cat > "$STAGE/_rels/.rels" <<'EOF'
<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">
  <Relationship Id="rId1"
                Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/officeDocument"
                Target="word/document.xml"/>
</Relationships>
EOF

cat > "$STAGE/word/_rels/document.xml.rels" <<'EOF'
<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships"/>
EOF

cat > "$STAGE/word/document.xml" <<EOF
<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<w:document xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main">
  <w:body>
    <w:p><w:r><w:t>Smoke test heading</w:t></w:r></w:p>
    <w:p><w:r><w:t>This document contains the marker $MARKER and a paragraph of prose so anydoc has non-trivial text to convert.</w:t></w:r></w:p>
  </w:body>
</w:document>
EOF

(cd "$STAGE" && zip -qr "$DOCX" .)
echo "wrote $DOCX ($(stat -c%s "$DOCX") bytes)"

echo
echo "== upload =="
DOC_JSON=$(curl -sf -X POST "$BASE/api/documents/" \
  -H "Authorization: Token $API_TOKEN" \
  -F "document=@$DOCX" \
  -F "title=smoke.docx")
echo "$DOC_JSON"
DOC_ID=$(echo "$DOC_JSON" | grep -oP '"id":\s*\K[0-9]+' | head -1)
if [ -z "${DOC_ID:-}" ]; then
  echo "no doc id in response"; exit 1
fi

echo
echo "== wait for post-ingest to drain =="
for _ in $(seq 1 60); do
  # Check the jobs table via the API — nothing pending on this doc?
  PENDING=$(curl -sf "$BASE/api/tasks/?doc_id=$DOC_ID&state=pending" \
    -H "Authorization: Token $API_TOKEN" | grep -c '"kind"' || true)
  RUNNING=$(curl -sf "$BASE/api/tasks/?doc_id=$DOC_ID&state=running" \
    -H "Authorization: Token $API_TOKEN" | grep -c '"kind"' || true)
  if [ "$PENDING" = "0" ] && [ "$RUNNING" = "0" ]; then break; fi
  sleep 0.5
done

echo
echo "== fetch content, look for marker =="
# Explicit -w so we can see HTTP status if the endpoint 500s; capture
# both body and status separately. curl -sf swallows non-2xx bodies
# entirely, which produced an empty-JSON traceback on the first run.
DOC_RESP=$(curl -s -w '\n---STATUS:%{http_code}' \
  "$BASE/api/documents/$DOC_ID/" \
  -H "Authorization: Token $API_TOKEN")
STATUS=$(echo "$DOC_RESP" | tail -1 | sed 's/^---STATUS://')
BODY=$(echo "$DOC_RESP" | sed '$d')
if [ "$STATUS" != "200" ]; then
  echo "  ✗ GET /api/documents/$DOC_ID/ returned $STATUS"
  echo "  body: $BODY"
  exit 1
fi
CONTENT=$(echo "$BODY" | python3 -c "import json,sys; print(json.load(sys.stdin).get('content','') or '')")

if echo "$CONTENT" | grep -q "$MARKER"; then
  echo "✅ PASS — content contains marker"
  echo "   (first 200 chars: ${CONTENT:0:200})"
else
  echo "❌ FAIL — marker not in content"
  echo "   content length: ${#CONTENT}"
  echo "   content head: ${CONTENT:0:400}"
  echo "   suchi logs (last 30 lines):"
  docker logs suchi-smoke-anydoc 2>&1 | tail -30
  exit 1
fi
