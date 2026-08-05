#!/usr/bin/env bash
#
# hack/pin-bumper.sh — audit upstream-repo tags pinned in the Dockerfile
# and (interactively) bump any that are more than 2 releases behind
# latest.
#
# Bumps to N-1 (one behind latest), never to latest. Rationale:
# the latest release might carry unnoticed regressions; a
# just-released-yesterday tag has had zero soak time. N-1 gives us
# ~one release cycle of community validation. If the current pin is
# already N-1 or newer, nothing happens.
#
# Rule of engagement: this script reads-and-suggests; the human runs
# `git diff Dockerfile` and decides whether to commit. Never auto-
# commits — every bump is a code-review moment (upstream changelog,
# breaking-change scan, license drift check).
#
# Requires: gh (authenticated), sed, git.

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
#   <repo>|<argname>|<display-name>
#
# argname is the Dockerfile ARG this pin lives in — the script
# grep-matches "ARG <argname>=" to find the current value.
# Add new pins here as the Dockerfile grows.
PINS=(
  "firecrawl/anydoc|ANYDOC_TAG|anydoc"
)

# semver_int converts "v1.2.3" or "1.2.3" to a monotonic sortable
# integer like 001002003 so bash string comparison Just Works. Handles
# leading v and 3-part semver; anything else falls back to lexical.
semver_int() {
  local v="${1#v}"
  local a b c
  IFS='.' read -r a b c <<<"$v"
  if [[ -z "${a:-}" || -z "${b:-}" || -z "${c:-}" ]]; then
    printf '%s' "$v"
    return
  fi
  printf '%03d%03d%03d' "$a" "$b" "$c" 2>/dev/null || printf '%s' "$v"
}

for entry in "${PINS[@]}"; do
  IFS='|' read -r repo arg display <<<"$entry"
  current=$(grep -E "^ARG ${arg}=" "$DOCKERFILE" | head -1 | sed -E "s/^ARG ${arg}=//") || true
  if [[ -z "$current" ]]; then
    echo "== $display ($repo) — no ARG $arg= in Dockerfile; skipping"
    continue
  fi
  echo "== $display ($repo) — currently pinned to $current"

  # gh returns tags newest-first as long as the repo publishes releases.
  # Fall back to git ls-remote if releases are empty.
  tags=$(gh api "repos/$repo/releases" --paginate --jq '.[].tag_name' 2>/dev/null | head -20)
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

  if [[ "$behind" -lt 3 ]]; then
    echo "  → within tolerance (need > 2 behind to trigger)"
    continue
  fi
  if [[ "$current" == "$n_minus_1" ]]; then
    echo "  → already at n-1; nothing to do"
    continue
  fi

  echo "  → BUMP suggested: $current  →  $n_minus_1"
  echo "  review upstream diff:"
  echo "    https://github.com/$repo/compare/${current}...${n_minus_1}"
  echo "  apply with:"
  echo "    sed -i 's|^ARG ${arg}=.*|ARG ${arg}=${n_minus_1}|' $DOCKERFILE"
  echo
done

echo
echo "Nothing was written. This is an audit tool; act on the suggestions above by hand."
