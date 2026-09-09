#!/usr/bin/env bash
# Package a prebuilt Suchi binary; never execute it or download dependencies.
set -euo pipefail

fail() { printf 'package-local: %s\n' "$*" >&2; exit 1; }

[[ $# == 5 ]] || fail 'Usage: hack/package-local.sh BINARY VERSION GOOS GOARCH OUTPUT_DIR'
binary=$1 version=$2 target_os=$3 target_arch=$4 output=$5
[[ -f $binary && ! -L $binary && -x $binary ]] || fail 'BINARY must be a regular executable file, not a symlink.'
[[ $version =~ ^[a-zA-Z0-9][a-zA-Z0-9.+_-]{0,127}$ ]] || fail 'VERSION must be a simple release or snapshot name, not a path.'
case "$target_os" in
    linux) platform=linux ;;
    darwin) platform=macos ;;
    *) fail 'Only linux and darwin local bundles are supported.' ;;
esac
[[ $target_arch == amd64 || $target_arch == arm64 ]] || fail 'GOARCH must be amd64 or arm64.'
command -v go >/dev/null || fail 'Go is required to inspect the prebuilt binary.'
metadata=$(go version -m "$binary") || fail 'Could not read Go build metadata.'
for setting in "GOOS=$target_os" "GOARCH=$target_arch"; do
    awk -v setting="$setting" '$1 == "build" && $2 == setting { found=1 } END { exit !found }' <<< "$metadata" ||
        fail "Binary build metadata does not match $setting."
done

root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
scripts=$root/deploy/local-$platform
files=(install.sh launch.sh)
if [[ $target_os == darwin ]]; then
    files+=(common.sh settings.sh Install.command)
    icon=$scripts/icon.png
else
    files+=(Install.sh)
    icon=$root/core/ui/assets/brand/suchi-tile.svg
fi
for file in "${files[@]}" README.md; do
    [[ -f $scripts/$file && ! -L $scripts/$file ]] || fail "Missing regular package source: $scripts/$file"
done
[[ -f $root/LICENSE ]] || fail 'Missing Suchi LICENSE.'
[[ -f $icon && ! -L $icon ]] || fail 'Missing regular Suchi icon.'
mkdir -p -- "$output"
output=$(cd -- "$output" && pwd)
bundle=suchi-$version-local-$target_os-$target_arch
archive=$bundle.tar.gz
[[ ! -e $output/$archive && ! -L $output/$archive && ! -e $output/$archive.sha256 && ! -L $output/$archive.sha256 ]] ||
    fail 'Output already exists; use a new output directory or version.'
stage=$(mktemp -d "$output/.suchi-package.XXXXXX")
trap 'rm -rf -- "$stage"' EXIT
mkdir "$stage/$bundle"
install -m 0755 "$binary" "$stage/$bundle/suchi"
for file in "${files[@]}"; do install -m 0755 "$scripts/$file" "$stage/$bundle/$file"; done
install -m 0644 "$scripts/README.md" "$stage/$bundle/README.md"
install -m 0644 "$root/LICENSE" "$stage/$bundle/LICENSE"
install -m 0644 "$icon" "$stage/$bundle/$(basename "$icon")"
printf '%s\n' "$version" > "$stage/$bundle/VERSION"
tar -C "$stage" -czf "$stage/$archive" "$bundle"
(
    cd "$stage"
    if command -v sha256sum >/dev/null; then
        sha256sum "$archive" > "$archive.sha256"
    else
        shasum -a 256 "$archive" > "$archive.sha256"
    fi
)
# Hard links publish complete files without overwriting an existing artifact.
ln "$stage/$archive" "$output/$archive"
ln "$stage/$archive.sha256" "$output/$archive.sha256"
printf '%s\n' "$output/$archive" "$output/$archive.sha256"
