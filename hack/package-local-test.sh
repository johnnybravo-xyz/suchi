#!/usr/bin/env bash
set -euo pipefail

root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
package=$root/hack/package-local.sh
test_dir=$(mktemp -d)
trap 'rm -rf -- "$test_dir"' EXIT
printf 'package main\nfunc main() { panic("packager must not execute the binary") }\n' > "$test_dir/main.go"

for target in linux/amd64 darwin/arm64; do
    target_os=${target%/*} target_arch=${target#*/}
    GOWORK=off CGO_ENABLED=0 GOOS=$target_os GOARCH=$target_arch \
        go build -o "$test_dir/binary" "$test_dir/main.go"
    version=v0.1.0-local-test
    bundle=suchi-$version-local-$target_os-$target_arch
    output=$test_dir/output-$target_os
    bash "$package" "$test_dir/binary" "$version" "$target_os" "$target_arch" "$output" >/dev/null
    (
        cd "$output"
        if command -v sha256sum >/dev/null; then
            sha256sum -c "$bundle.tar.gz.sha256"
        else
            shasum -a 256 -c "$bundle.tar.gz.sha256"
        fi
    )
    mkdir "$test_dir/unpacked-$target_os"
    tar -xzf "$output/$bundle.tar.gz" -C "$test_dir/unpacked-$target_os"
    unpacked=$test_dir/unpacked-$target_os/$bundle
    cmp "$test_dir/binary" "$unpacked/suchi"
    cmp "$root/LICENSE" "$unpacked/LICENSE"
    [[ $(< "$unpacked/VERSION") == "$version" ]]
    [[ -x $unpacked/install.sh && -x $unpacked/launch.sh && -s $unpacked/README.md ]]
    if [[ $target_os == darwin ]]; then
        cmp "$root/deploy/local-macos/icon.png" "$unpacked/icon.png"
        [[ -x $unpacked/Install.command && -x $unpacked/common.sh && -x $unpacked/settings.sh ]]
    else
        cmp "$root/core/ui/assets/brand/suchi-tile.svg" "$unpacked/suchi-tile.svg"
        [[ -x $unpacked/Install.sh ]]
    fi
    if bash "$package" "$test_dir/binary" "$version" "$target_os" "$target_arch" "$output" >/dev/null 2>&1; then
        printf 'Overwrote an existing package.\n' >&2; exit 1
    fi
    if bash "$package" "$test_dir/binary" "$version" "$target_os" bad "$output" >/dev/null 2>&1; then
        printf 'Accepted an unsupported target.\n' >&2; exit 1
    fi
done

if bash "$package" "$test_dir/binary" valid linux arm64 "$test_dir/wrong-target" >/dev/null 2>&1; then
    printf 'Accepted a binary for the wrong operating system.\n' >&2; exit 1
fi
if bash "$package" "$test_dir/binary" ../escape darwin arm64 "$test_dir/unsafe-name" >/dev/null 2>&1; then
    printf 'Accepted path traversal in version.\n' >&2; exit 1
fi
ln -s "$test_dir/binary" "$test_dir/symlink"
if bash "$package" "$test_dir/symlink" valid darwin arm64 "$test_dir/symlink-output" >/dev/null 2>&1; then
    printf 'Accepted a symlink as the binary.\n' >&2; exit 1
fi
printf 'Local bundle metadata, contents, checksums and refusal tests passed.\n'
