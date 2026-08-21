#!/usr/bin/env bash
# Interactive setup for the mbsync → suchi deployment recipe.
#
# Asks which IMAP provider to wire up, symlinks the matching template
# under ./templates/mbsyncrc.tmpl, and writes two env files:
#
#   ./.env         — compose-level, non-secret suchi settings
#   ./config/.env  — mbsync-only credentials and sync settings
#
# Idempotent: safe to re-run to change provider or rotate credentials.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")" && pwd)"
cd "$ROOT"

# ---------- pick provider ----------
echo "Which mail provider?"
echo "  1) Proton Mail  (via Bridge, running as a sibling container)"
echo "  2) Gmail        (imap.gmail.com, app password)"
echo "  3) Fastmail     (imap.fastmail.com, app password)"
echo "  4) Other        (generic IMAPS/IMAP endpoint)"
read -rp "> " choice

case "$choice" in
    1) PROVIDER=proton     ; DEFAULT_HOST=protonmail-bridge ; DEFAULT_PORT=143 ; DEFAULT_SSL=None    ; PROFILE=proton  ;;
    2) PROVIDER=gmail      ; DEFAULT_HOST=imap.gmail.com    ; DEFAULT_PORT=993 ; DEFAULT_SSL=IMAPS   ; PROFILE=        ;;
    3) PROVIDER=fastmail   ; DEFAULT_HOST=imap.fastmail.com ; DEFAULT_PORT=993 ; DEFAULT_SSL=IMAPS   ; PROFILE=        ;;
    4) PROVIDER=generic    ; DEFAULT_HOST=                  ; DEFAULT_PORT=993 ; DEFAULT_SSL=IMAPS   ; PROFILE=        ;;
    *) echo "unknown choice: $choice" >&2 ; exit 1 ;;
esac

# ---------- endpoint ----------
if [ "$PROVIDER" = "generic" ]; then
    read -rp "IMAP host: " MAIL_HOST
    read -rp "IMAP port [${DEFAULT_PORT}]: " MAIL_PORT
    MAIL_PORT="${MAIL_PORT:-$DEFAULT_PORT}"
    read -rp "SSL type (IMAPS / STARTTLS / None) [${DEFAULT_SSL}]: " MAIL_SSL
    MAIL_SSL="${MAIL_SSL:-$DEFAULT_SSL}"
else
    MAIL_HOST="$DEFAULT_HOST"
    MAIL_PORT="$DEFAULT_PORT"
    MAIL_SSL="$DEFAULT_SSL"
fi

# ---------- credentials ----------
read -rp "IMAP username: " MAIL_USER
if [ "$PROVIDER" = "proton" ]; then
    cat <<'EOF'

For Proton, run this first to get an IMAP password from Bridge:

    docker compose --profile proton run --rm -it bridge init
    >>> login       # your Proton email + password + 2FA
    >>> info        # copy the "IMAP Password" line
    >>> exit

The IMAP Password shown by `info` is NOT your Proton account password.
Paste that into the prompt below.

EOF
fi
read -rsp "IMAP password: " MAIL_PASSWORD
echo

# ---------- what to sync ----------
read -rp "Folders to sync [INBOX]: " MAIL_FOLDERS_RAW
MAIL_FOLDERS_RAW="${MAIL_FOLDERS_RAW:-INBOX}"
MAIL_FOLDERS="\"${MAIL_FOLDERS_RAW//\"/}\""  # quote for mbsync

read -rp "Max messages per folder [200]: " MAIL_MAX_MESSAGES
MAIL_MAX_MESSAGES="${MAIL_MAX_MESSAGES:-200}"

read -rp "Max attachment size (K/M suffix ok) [25m]: " MAIL_MAX_SIZE
MAIL_MAX_SIZE="${MAIL_MAX_SIZE:-25m}"

# ---------- suchi ----------
read -rp "Suchi listening port [8000]: " SUCHI_PORT
SUCHI_PORT="${SUCHI_PORT:-8000}"

# ---------- write config/.env (mbsync only) ----------
mkdir -p config
umask 077
cat > config/.env <<EOF
# Container-side credentials for mbsync only. Rendered by setup.sh.
MAIL_HOST=${MAIL_HOST}
MAIL_PORT=${MAIL_PORT}
MAIL_SSL=${MAIL_SSL}
MAIL_USER=${MAIL_USER}
MAIL_PASSWORD=${MAIL_PASSWORD}
MAIL_FOLDERS=${MAIL_FOLDERS}
MAIL_MAX_MESSAGES=${MAIL_MAX_MESSAGES}
MAIL_MAX_SIZE=${MAIL_MAX_SIZE}
SYNC_INTERVAL=300
EOF

# ---------- write .env (compose-level) ----------
cat > .env <<EOF
# Compose-level env. Auto-loaded by docker compose from the file next
# to docker-compose.yml. Rendered by setup.sh.
COMPOSE_PROFILES=${PROFILE}
SUCHI_PORT=${SUCHI_PORT}
MAIL_UID=$(id -u)
MAIL_GID=$(id -g)
SUCHI_OWNER_EMAIL=${MAIL_USER}
EOF

# ---------- symlink template ----------
ln -sfn "${PROVIDER}.mbsyncrc.tmpl" templates/mbsyncrc.tmpl

# ---------- create bind-mount dirs ----------
mkdir -p maildir ingest suchi-data
[ "$PROVIDER" = "proton" ] && mkdir -p bridge-data

echo
echo "Wrote:"
echo "  config/.env               (container env, 600)"
echo "  .env                      (compose env)"
echo "  templates/mbsyncrc.tmpl → ${PROVIDER}.mbsyncrc.tmpl"
echo

if [ "$PROVIDER" = "proton" ]; then
    cat <<EOF
Next: bring up the stack.

    docker compose up -d
    docker compose logs -f mbsync suchi

Then grab the /setup token from suchi's log, complete /setup, and log
in at http://127.0.0.1:${SUCHI_PORT}.
EOF
else
    cat <<EOF
Next: bring up the stack.

    docker compose up -d
    docker compose logs -f mbsync suchi

Then grab the /setup token from suchi's log, complete /setup, and log
in at http://127.0.0.1:${SUCHI_PORT}.
EOF
fi
