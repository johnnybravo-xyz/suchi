#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-or-later

set -euo pipefail

root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)

check_allowlist() {
  local allowlist=$1
  local paths=$2
  local path
  while IFS= read -r path; do
    [[ -z "$path" ]] && continue
    if ! grep -Eq -f "$allowlist" <<<"$path"; then
      echo "unexpected package path: $path" >&2
      return 1
    fi
  done <<<"$paths"
}

inspect_macos_app() {
  local app=$1
  local main="$app/Contents/MacOS/suchi-desktop"
  local sidecar="$app/Contents/MacOS/suchi-sidecar"
  local paths

  test -x "$main"
  test -x "$sidecar"
  test -s "$app/Contents/Resources/LICENSE"
  test -s "$app/Contents/Resources/NOTICE"
  test -s "$app/Contents/Resources/THIRD-PARTY-NOTICES.html"
  test -s "$app/Contents/Resources/icon.icns"
  paths=$(cd "$app" && find Contents -type f -o -type l | LC_ALL=C sort)
  check_allowlist "$root/desktop/package-allowlist-macos.txt" "$paths"

  test "$(plutil -extract CFBundleIdentifier raw "$app/Contents/Info.plist")" = "xyz.johnnybravo.suchi"
  lipo -archs "$main" | grep -qw x86_64
  lipo -archs "$main" | grep -qw arm64
  lipo -archs "$sidecar" | grep -qw x86_64
  lipo -archs "$sidecar" | grep -qw arm64
  "$sidecar" version | grep -q '^suchi '

  if codesign --verify --deep --strict --verbose=2 "$app" >/dev/null 2>&1; then
    :
  elif [[ ${REQUIRE_RELEASE_SIGNATURE:-0} = 1 ]]; then
    echo "macOS release app is unsigned or has an invalid signature" >&2
    return 1
  fi

  if [[ ${REQUIRE_RELEASE_SIGNATURE:-0} = 1 ]]; then
    codesign -dv --verbose=4 "$app" 2>&1 | grep -q '^Authority=Developer ID Application:'
    spctl --assess --type execute --verbose=2 "$app"
    xcrun stapler validate "$app"
  fi

  if [[ ${DESKTOP_LAUNCH_SMOKE:-0} = 1 ]]; then
    local home pid
    home=$(mktemp -d)
    HOME="$home" "$main" >"$home/launcher.log" 2>&1 &
    pid=$!
    sleep 5
    if ! kill -0 "$pid" 2>/dev/null; then
      cat "$home/launcher.log" >&2
      return 1
    fi
    kill "$pid"
    wait "$pid" 2>/dev/null || true
    rm -rf "$home"
  fi
}

inspect_macos() {
  local bundle_dir=${DESKTOP_PACKAGE_DIR:-"$root/desktop/src-tauri/target/universal-apple-darwin/release/bundle"}
  local app="$bundle_dir/macos/Suchi.app"
  local dmg

  test -d "$app"
  inspect_macos_app "$app"
  mapfile -t dmgs < <(find "$bundle_dir/dmg" -maxdepth 1 -type f -name 'Suchi_*.dmg' | LC_ALL=C sort)
  test "${#dmgs[@]}" -eq 1
  dmg=${dmgs[0]}
  test -s "$dmg"

  if [[ ${REQUIRE_RELEASE_SIGNATURE:-0} = 1 ]]; then
    codesign --verify --strict --verbose=2 "$dmg"
    spctl --assess --type open --context context:primary-signature --verbose=2 "$dmg"
    xcrun stapler validate "$dmg"
  fi

  echo "desktop package smoke passed: $app"
  echo "desktop package smoke passed: $dmg"
}

linux_package_paths() {
  local package=$1
  case "$package" in
    *.deb) dpkg-deb --fsys-tarfile "$package" | tar -tf - ;;
    *.rpm) rpm -qlp "$package" ;;
    *) return 2 ;;
  esac | sed -e 's#^\./##' -e '/\/$/d' | LC_ALL=C sort
}

inspect_linux_package() {
  local package=$1
  local paths
  paths=$(linux_package_paths "$package")
  check_allowlist "$root/desktop/package-allowlist-linux.txt" "$paths"
  grep -qx 'usr/bin/suchi-desktop' <<<"$paths"
  grep -qx 'usr/bin/suchi-sidecar' <<<"$paths"
  grep -Eq '^usr/lib/(Suchi|suchi)/LICENSE$' <<<"$paths"
  grep -Eq '^usr/lib/(Suchi|suchi)/NOTICE$' <<<"$paths"
  grep -Eq '^usr/lib/(Suchi|suchi)/THIRD-PARTY-NOTICES\.html$' <<<"$paths"

  if [[ ${REQUIRE_RELEASE_SIGNATURE:-0} = 1 ]]; then
    case "$package" in
      *.deb) dpkg-sig --verify "$package" | grep -q GOODSIG ;;
      *.rpm) rpm --checksig "$package" | grep -Eqi '(pgp|rsa).*(ok|signature)' ;;
    esac
  fi

  echo "desktop package smoke passed: $package"
}

inspect_linux() {
  local bundle_dir=${DESKTOP_PACKAGE_DIR:-"$root/desktop/src-tauri/target/release/bundle"}
  local package
  local debs=()
  local rpms=()

  if find "$bundle_dir" -type f -name '*.AppImage' -print -quit | grep -q .; then
    echo "AppImage is not a supported Suchi artifact" >&2
    return 1
  fi
  mapfile -t debs < <(find "$bundle_dir" -type f -name '*.deb' | LC_ALL=C sort)
  mapfile -t rpms < <(find "$bundle_dir" -type f -name '*.rpm' | LC_ALL=C sort)
  test "${#debs[@]}" -eq 1
  test "${#rpms[@]}" -eq 1
  for package in "${debs[@]}" "${rpms[@]}"; do
    inspect_linux_package "$package"
  done
}

case "$(uname -s)" in
  Darwin) inspect_macos ;;
  Linux) inspect_linux ;;
  *) echo "unsupported desktop package host" >&2; exit 1 ;;
esac
