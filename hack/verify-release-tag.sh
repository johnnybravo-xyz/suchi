#!/bin/sh
# SPDX-License-Identifier: AGPL-3.0-or-later
set -eu

if [ "$#" -ne 1 ]; then
  echo "usage: $0 <tag>" >&2
  exit 64
fi

tag=$1
root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
key="$root/.github/release-signing-key.asc"
expected_fingerprint=D0B0F9E868440860B543E2A7D8C255808C9A0D9C
ref="refs/tags/$tag"

if [ "$(git -C "$root" cat-file -t "$ref" 2>/dev/null || true)" != tag ]; then
  echo "$tag must be an annotated tag; lightweight and missing tags are rejected" >&2
  exit 1
fi

gnupg_home=$(mktemp -d)
trap 'rm -rf "$gnupg_home"' EXIT HUP INT TERM
chmod 700 "$gnupg_home"

key_fingerprint=$(
  GNUPGHOME="$gnupg_home" gpg --batch --with-colons \
    --import-options show-only --import "$key" 2>/dev/null |
    awk -F: '$1 == "fpr" { print $10; exit }'
)
if [ "$key_fingerprint" != "$expected_fingerprint" ]; then
  echo "release key fingerprint is $key_fingerprint, expected $expected_fingerprint" >&2
  exit 1
fi
GNUPGHOME="$gnupg_home" gpg --batch --quiet --import "$key"

if ! status=$(GNUPGHOME="$gnupg_home" git -C "$root" verify-tag --raw "$tag" 2>&1); then
  printf '%s\n' "$status" >&2
  echo "$tag does not have a valid release signature" >&2
  exit 1
fi
signer=$(
  printf '%s\n' "$status" |
    awk '$1 == "[GNUPG:]" && $2 == "VALIDSIG" { print $3; exit }'
)
if [ "$signer" != "$expected_fingerprint" ]; then
  echo "$tag was signed by $signer, expected $expected_fingerprint" >&2
  exit 1
fi

printf 'verified signed annotated tag %s with %s\n' "$tag" "$expected_fingerprint"
