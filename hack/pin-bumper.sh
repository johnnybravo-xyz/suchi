#!/usr/bin/env bash
#
# hack/pin-bumper.sh — audit upstream-repo tags pinned in the Dockerfile
# and suggest a bump when one is older than the previous stable release.
#
# Bumps to N-1 (one behind latest), never to latest. Rationale:
# the latest release might carry unnoticed regressions; a
# just-released-yesterday tag has had zero soak time. N-1 gives us
# roughly one release cycle of validation. If the current pin is
# already N-1 or newer, nothing happens.
#
# Rule of engagement: this script reads-and-suggests; the human runs
# `git diff Dockerfile` and decides whether to commit. Never auto-
# commits — every bump is a code-review moment (upstream changelog,
# breaking-change scan, license drift check).
#
# Requires: gh (authenticated), sed.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
DOCKERFILE="$ROOT/Dockerfile"

if [[ ! -f "$DOCKERFILE" ]]; then
  echo "no Dockerfile at $DOCKERFILE" >&2
  exit 1
fi
if ! command -v gh >/dev/null; then
  echo "gh CLI not on PATH — install github.com/cli/cli" >&2
  exit 1
fi

# Registry of managed pins: one line per pin, format:
#   <repo>|<tag-arg>|<commit-arg>|<display-name>
#
# tag-arg and commit-arg are paired Dockerfile ARGs. The script verifies
# that the readable release tag still resolves to the recorded commit.
# Add new pins here as the Dockerfile grows.
PINS=(
  "firecrawl/anydoc|ANYDOC_TAG|ANYDOC_COMMIT|anydoc"
)

for entry in "${PINS[@]}"; do
  IFS='|' read -r repo tag_arg commit_arg display <<<"$entry"
  current=$(grep -E "^ARG ${tag_arg}=" "$DOCKERFILE" | head -1 | sed -E "s/^ARG ${tag_arg}=//") || true
  current_commit=$(grep -E "^ARG ${commit_arg}=" "$DOCKERFILE" | head -1 | sed -E "s/^ARG ${commit_arg}=//") || true
  if [[ -z "$current" ]]; then
    echo "== $display ($repo) — no ARG $tag_arg= in Dockerfile; skipping"
    continue
  fi
  echo "== $display ($repo) — currently pinned to $current ($current_commit)"
  resolved_current=$(gh api "repos/$repo/commits/$current" --jq '.sha')
  if [[ "$resolved_current" != "$current_commit" ]]; then
    echo "  ERROR: $current resolves to $resolved_current; update ARG $commit_arg" >&2
    continue
  fi

  # GitHub returns published releases newest-first.
  tags=$(gh api "repos/$repo/releases" --paginate \
    --jq '.[] | select(.draft == false and .prerelease == false) | .tag_name' \
    2>/dev/null | head -20)
  if [[ -z "$tags" ]]; then
    echo "  no releases returned by GitHub; skipping"
    continue
  fi

  latest=$(echo "$tags" | sed -n 1p)
  n_minus_1=$(echo "$tags" | sed -n 2p)
  if [[ -z "$n_minus_1" ]]; then
    n_minus_1="$latest"  # only one release exists
  fi

  # Count how many releases the current pin is behind.
  # 0 = pinned to latest; 1 = one behind; ...
  behind=0
  while IFS= read -r t; do
    [[ "$t" == "$current" ]] && break
    behind=$((behind + 1))
  done <<<"$tags"

  echo "  latest = $latest"
  echo "  n-1    = $n_minus_1"
  echo "  behind = $behind release(s)"

  if [[ "$behind" -le 1 ]]; then
    echo "  → at n-1 or newer; nothing to do"
    continue
  fi

  echo "  → BUMP suggested: $current  →  $n_minus_1"
  target_commit=$(gh api "repos/$repo/commits/$n_minus_1" --jq '.sha')
  echo "  review upstream diff:"
  echo "    https://github.com/$repo/compare/${current}...${n_minus_1}"
  echo "  apply with:"
  echo "    sed -i 's|^ARG ${tag_arg}=.*|ARG ${tag_arg}=${n_minus_1}|; s|^ARG ${commit_arg}=.*|ARG ${commit_arg}=${target_commit}|' $DOCKERFILE"
  echo
done

echo
echo "Nothing was written. This is an audit tool; act on the suggestions above by hand."
