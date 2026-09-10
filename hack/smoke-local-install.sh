#!/usr/bin/env bash
# Native Linux service smoke; only a uniquely named transient unit and temp archive.
set -euo pipefail

root=$(cd "$(dirname "$0")/.." && pwd)
for tool in systemd-run systemctl curl jq magick qpdf pdftotext pdftoppm pdfinfo tesseract; do
  command -v "$tool" >/dev/null || { echo "Required: $tool" >&2; exit 1; }
done
test -n "${XDG_RUNTIME_DIR:-}" || { echo "Run in a Linux desktop user session." >&2; exit 1; }
systemctl --user show-environment >/dev/null
test -x "$root/dist/suchi" || { echo "Run make build first." >&2; exit 1; }

smoke_dir=$(mktemp -d /tmp/suchi-local-smoke.XXXXXXXX)
unit="suchi-local-smoke-${smoke_dir##*.}"
runtime_dir="$XDG_RUNTIME_DIR/$unit"
port=${PORT:-18769}
base="http://suchi.localhost:$port"
cleanup() {
  local result=$?
  if (( result != 0 )); then
    journalctl --user -u "$unit.service" -n 40 --no-pager >&2 || true
  fi
  if systemctl --user stop "$unit.service" 2>/dev/null; then
    rm -rf -- "$smoke_dir"
  else
    echo "Service cleanup needs checking; test files retained at $smoke_dir" >&2
  fi
  systemctl --user reset-failed "$unit.service" 2>/dev/null || true
  return "$result"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

mkdir -m 0700 "$smoke_dir/archive"
printf '\n' > "$smoke_dir/config.toml"
magick -size 1600x900 xc:white -font DejaVu-Sans -fill black -pointsize 56 \
  -gravity NorthWest -annotate +80+100 'LOCAL ARCHIVE RECEIPT' \
  -annotate +80+220 'Coffee beans purchase' -annotate +80+340 'Total 425.00' \
  "$smoke_dir/receipt.png"

# env -i matches the installed service: no inherited model/mail/dev settings.
# systemd, not the launching shell, expands its own notification socket.
# shellcheck disable=SC2016
systemd-run --user --unit="$unit" --property=Type=notify \
  --property=RuntimeDirectory="$unit" --property=RuntimeDirectoryMode=0700 \
  --property=UMask=0077 --property=NoNewPrivileges=yes \
  --property=KillMode=control-group --property=TimeoutStartSec=45s \
  /usr/bin/env -i "HOME=$HOME" PATH=/usr/local/bin:/usr/bin:/bin \
  'NOTIFY_SOCKET=${NOTIFY_SOCKET}' "SUCHI_CONFIG=$smoke_dir/config.toml" \
  "DATA_DIR=$smoke_dir/archive" "LISTEN_ADDR=127.0.0.1:$port" "PUBLIC_URL=$base" \
  "$root/dist/suchi" serve --systemd --setup-token-file="$runtime_dir/setup-token"

curl -fsS "$base/readyz" >/dev/null
test "$(stat -c %a "$runtime_dir")" = 700
test "$(stat -c %a "$runtime_dir/setup-token")" = 600
test "$(curl -sS -o /dev/null -w '%{http_code}' "$base/api/documents/")" = 401

# Credentials are synthetic; feed even these through stdin, not curl's argv.
jq -n --rawfile token "$runtime_dir/setup-token" \
  '{token: ($token | rtrimstr("\n")), email: "local-smoke@example.invalid", password: "local-smoke-password"}' |
  curl -fsS "$base/setup" -H 'Content-Type: application/json' --data-binary @- >/dev/null
test ! -e "$runtime_dir/setup-token"
printf '%s' '{"email":"local-smoke@example.invalid","password":"local-smoke-password"}' |
  curl -fsS "$base/api/login" -c "$smoke_dir/cookies" \
    -H 'Content-Type: application/json' --data-binary @- >/dev/null
doc_id=$(curl -fsS "$base/api/documents/" -b "$smoke_dir/cookies" \
  -H 'Sec-Fetch-Site: same-origin' -F "document=@$smoke_dir/receipt.png" | jq -er '.id')

for (( attempt=0; attempt<90; attempt++ )); do
  if curl -fsS "$base/api/documents/$doc_id" -b "$smoke_dir/cookies" |
    jq -e '.content | contains("Coffee beans")' >/dev/null; then
    break
  fi
  sleep 1
done
curl -fsS "$base/api/documents/$doc_id" -b "$smoke_dir/cookies" |
  jq -e '.content | contains("Coffee beans")' >/dev/null
curl -fsS "$base/api/search/?q=Coffee" -b "$smoke_dir/cookies" |
  jq -e --argjson id "$doc_id" '.results | any(.id == $id)' >/dev/null

systemctl --user restart "$unit.service"
test ! -e "$runtime_dir/setup-token"
curl -fsS "$base/api/documents/$doc_id" -b "$smoke_dir/cookies" |
  jq -e '.content | contains("Coffee beans")' >/dev/null
if journalctl --user -u "$unit.service" -o cat --no-pager | grep -q '"token":'; then
  echo "A token was logged by file-mode bootstrap." >&2
  exit 1
fi
echo 'PASS: native readiness, private bootstrap, authentication, receipt OCR/search, and restart persistence'
