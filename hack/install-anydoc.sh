#!/bin/sh
# install-anydoc.sh — fetch the anydoc binary from the latest suchi
# release, verify SHA256 against the release's SHA256SUMS, install to
# /usr/local/bin (falls back to ~/.local/bin when not root).
#
# Usage (one-liner):
#
#   curl -sfL https://raw.githubusercontent.com/johnnybravo-xyz/suchi/main/hack/install-anydoc.sh | sh
#
# Flags:
#   --dry-run  print what would be downloaded and exit 0
#   --version <tag>  pin a specific release (default: latest)
#
# Docker users don't need this — anydoc is baked into both slim and
# full images (see Dockerfile:anydoc-build). This script is for bare-
# metal / go-run installs.

set -eu

REPO="johnnybravo-xyz/suchi"
DRY_RUN=""
VERSION=""

while [ $# -gt 0 ]; do
  case "$1" in
    --dry-run) DRY_RUN=1 ;;
    --version) shift; VERSION="$1" ;;
    -h|--help)
      sed -n '2,17p' "$0" | sed 's/^# \{0,1\}//'
      exit 0
      ;;
    *) echo "unknown flag: $1" >&2; exit 2 ;;
  esac
  shift
done

# Detect OS + arch. Only linux/amd64 + linux/arm64 are published for
# v0.1 — Darwin / Windows anydoc builds are a follow-up. Refuse
# unsupported combinations up front.
os="$(uname -s | tr '[:upper:]' '[:lower:]')"
arch="$(uname -m)"
case "$arch" in
  x86_64|amd64) arch=amd64 ;;
  aarch64|arm64) arch=arm64 ;;
  *) echo "unsupported arch: $arch" >&2; exit 1 ;;
esac
if [ "$os" != "linux" ]; then
  echo "unsupported OS: $os (only linux prebuilt binaries are published today)" >&2
  echo "workaround: build from source at https://github.com/firecrawl/anydoc" >&2
  exit 1
fi

# Resolve version — either the operator-supplied tag or the latest
# release's tag_name (JSON parsed with sed to keep this dep-free).
if [ -z "$VERSION" ]; then
  api="https://api.github.com/repos/$REPO/releases/latest"
  VERSION="$(curl -sfL "$api" | sed -n 's/.*"tag_name": *"\(v[^"]*\)".*/\1/p' | head -n1)"
  if [ -z "$VERSION" ]; then
    echo "could not resolve latest release tag from $api" >&2
    exit 1
  fi
fi

tarball="anydoc-${VERSION}-linux-${arch}.tar.gz"
base="https://github.com/$REPO/releases/download/$VERSION"
tarball_url="$base/$tarball"
sums_url="$base/SHA256SUMS"

echo "anydoc: $VERSION linux/$arch"
echo "  tarball: $tarball_url"
echo "  sha256:  $sums_url (verified via subset match)"

if [ -n "$DRY_RUN" ]; then
  echo "dry-run: no download, no install"
  exit 0
fi

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
cd "$tmp"

curl -sfL -o "$tarball" "$tarball_url"
curl -sfL -o "SHA256SUMS" "$sums_url"

# Verify — grep the tarball line out and feed to sha256sum -c.
if ! grep -F "  $tarball" SHA256SUMS > "$tarball.sha256"; then
  echo "no SHA256SUMS entry for $tarball — refusing to install" >&2
  exit 1
fi
sha256sum -c "$tarball.sha256"

tar -xzf "$tarball"
# Layout: anydoc-<version>-linux-<arch>/{anydoc,LICENSE,NOTICE}
extracted="$(basename "$tarball" .tar.gz)"

# Prefer /usr/local/bin; fall back to ~/.local/bin without sudo.
if [ "$(id -u)" = "0" ] || [ -w /usr/local/bin ]; then
  dest="/usr/local/bin/anydoc"
else
  mkdir -p "$HOME/.local/bin"
  dest="$HOME/.local/bin/anydoc"
fi

install -m 0755 "$extracted/anydoc" "$dest"
echo "installed: $dest"
echo "verify with: suchi doctor | grep anydoc"
