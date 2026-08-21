#!/bin/sh
# Render mbsyncrc from a template, then loop: pull mail into
# /var/mail (Maildir), fan every new file into /ingest as
# <sha>.eml. Suchi's fs-watch picks it up from /ingest and treats
# each file as message/rfc822.
#
# Envsubst substitutes MAIL_* / PROTON_BRIDGE_* vars but keeps
# $MAIL_PASSWORD literal — mbsync's PassCmd shell resolves it at
# sync time so the password never lands in a config file on disk.
#
# The fan step uses -newer against /var/mail/.fanned-marker so each
# cycle only touches Maildir files added since last sync. This keeps
# it O(delta) rather than O(mailbox size) on repeated runs and stops
# us from re-dropping messages that suchi's already ingested.

set -eu

: "${MAIL_USER:?MAIL_USER must be set}"
: "${MAIL_PASSWORD:?MAIL_PASSWORD must be set}"
: "${MAIL_HOST:?MAIL_HOST must be set}"
: "${MAIL_PORT:?MAIL_PORT must be set}"
: "${MAIL_SSL:?MAIL_SSL must be set (IMAPS | STARTTLS | None)}"
: "${MAIL_FOLDERS:=INBOX}"
: "${MAIL_MAX_MESSAGES:=200}"
: "${MAIL_MAX_SIZE:=25m}"

RCFILE=/tmp/mbsyncrc
# shellcheck disable=SC2016 # Keep $MAIL_PASSWORD literal for mbsync PassCmd.
envsubst '$MAIL_USER $MAIL_HOST $MAIL_PORT $MAIL_SSL $MAIL_FOLDERS $MAIL_MAX_MESSAGES $MAIL_MAX_SIZE' \
    < /work/mbsyncrc.tmpl > "$RCFILE"
chmod 600 "$RCFILE"

mkdir -p /var/mail /ingest
MARKER=/var/mail/.fanned-marker
[ -f "$MARKER" ] || touch -d '1970-01-01' "$MARKER"

SYNC_INTERVAL="${SYNC_INTERVAL:-300}"
ONE_SHOT="${ONE_SHOT:-0}"

fan() {
    # Copy every Maildir file that landed since the last successful
    # fan into /ingest/<sha>.eml. sha256 stem is idempotent: a re-run
    # of the same message hashes the same, and the destination is a
    # no-op copy. Suchi's fs-watch deletes each file after successful
    # ingest, and its post-ingest.email dedup handles cases where the
    # source Maildir file reappears with the same Message-Id.
    next_marker="${MARKER}.next"
    file_list="${MARKER}.files"
    touch "$next_marker"
    if ! find /var/mail -type f \
        \( -path '*/cur/*' -o -path '*/new/*' \) \
        -newer "$MARKER" ! -newer "$next_marker" > "$file_list"
    then
        rm -f "$next_marker" "$file_list"
        return 1
    fi

    count=0
    failed=0
    while read -r src; do
        if ! sum=$(sha256sum "$src" 2>/dev/null | awk '{print $1}') || [ -z "$sum" ]; then
            echo "fan: could not hash $src" >&2
            failed=1
            continue
        fi
        dst="/ingest/$sum.eml"
        [ -e "$dst" ] && continue
        tmp="${dst}.tmp.$$"
        if ! cp "$src" "$tmp" || ! mv "$tmp" "$dst"; then
            rm -f "$tmp"
            echo "fan: could not copy $src" >&2
            failed=1
            continue
        fi
        count=$((count + 1))
        echo "  -> $sum.eml"
    done < "$file_list"
    rm -f "$file_list"
    echo "fan: $count new file(s)"

    if [ "$failed" -eq 0 ]; then
        mv "$next_marker" "$MARKER"
        return 0
    fi
    rm -f "$next_marker"
    return 1
}

run_once() {
    ts=$(date -Iseconds)
    echo "$ts sync starting"
    if mbsync -c "$RCFILE" -a; then
        echo "$ts sync ok, fanning"
        if fan; then
            return 0
        fi
        echo "$ts fan failed; marker unchanged"
        return 1
    else
        rc=$?
        echo "$ts sync failed (exit $rc); skipping fan"
        return $rc
    fi
}

if [ "$ONE_SHOT" = "1" ]; then
    run_once
    exit $?
fi

while :; do
    run_once || true
    echo "sleeping ${SYNC_INTERVAL}s"
    sleep "$SYNC_INTERVAL"
done
