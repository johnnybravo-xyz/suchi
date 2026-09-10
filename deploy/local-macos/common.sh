#!/bin/bash

fail() {
    if declare -F show_error >/dev/null; then show_error "$*"; else printf 'suchi local: %s\n' "$*" >&2; fi
    return 1
}

desktop_home() { printf '%s\n' "$HOME"; }

check_path() {
    # These paths become literal shell arguments and XML text, not shell input.
    [[ $1 == /* && $1 != *[!a-zA-Z0-9_./\ -]* && $1 != *'/../'* && $1 != */.. ]] ||
        fail "Unsupported path: $1 (use letters, numbers, spaces, /, ., _, or -)."
}

check_owned_file() {
    [[ ! -L $1 ]] || { fail "Refusing symbolic link: $1"; return 1; }
    [[ ! -e $1 || ( -f $1 && -O $1 ) ]] || {
        fail "Refusing a file not owned by you: $1"; return 1;
    }
}

check_managed_file() {
    check_owned_file "$1" || return 1
    if [[ -e $1 ]] && ! grep -q 'Managed by Suchi local installer\.' "$1"; then
        fail "Refusing to replace a file not managed by Suchi: $1"; return 1
    fi
}

check_owned_directory() {
    [[ ! -L $1 && ( ! -e $1 || ( -d $1 && -O $1 ) ) ]] || {
        fail "Refusing a symlink or directory not owned by you: $1"; return 1;
    }
}

check_private_directory() {
    check_owned_directory "$1" || return 1
    [[ -d $1 && $(stat -f '%Lp' "$1") == 700 ]] || {
        fail "Expected a private directory (mode 700): $1"; return 1;
    }
}

job_pid() {
    local state=$1 expected=$2 path
    path=$(printf '%s\n' "$state" | sed -n 's/^[[:space:]]*path = //p' | head -n 1)
    # launchd may report different path casing on case-insensitive volumes.
    [[ $path -ef $expected ]] || {
        fail 'Another launchd job uses page.suchi.local. Resolve that conflict first.'; return 1;
    }
    printf '%s\n' "$state" | awk '$1 == "pid" && $2 == "=" && $3 ~ /^[0-9]+$/ { print $3; exit }'
}

listener_is_owned() {
    local pid=$1 port=$2 listeners
    [[ $pid =~ ^[1-9][0-9]*$ ]] || return 1
    listeners=$(lsof -nP -a -p "$pid" -iTCP@127.0.0.1:"$port" -sTCP:LISTEN -Fpn 2>/dev/null) || return 1
    # A healthy unrelated server, stale PID, or wildcard listener is not Suchi.
    printf '%s\n' "$listeners" | awk -v pid="$pid" -v endpoint="127.0.0.1:$port" '
        /^p/ { owner = substr($0, 2) }
        /^n/ && owner == pid && substr($0, 2) == endpoint { found = 1 }
        END { exit !found }'
}

stop_job() {
    local target=$1 pid=$2 attempt
    launchctl bootout "$target" || return 1
    [[ $pid =~ ^[1-9][0-9]*$ ]] || return 0
    for ((attempt=0; attempt<60; attempt++)); do
        if ! kill -0 "$pid" 2>/dev/null; then return 0; fi
        sleep 1
    done
    fail 'Suchi is still stopping. No program files were replaced; retry after it has exited.'
}

rotate_log() {
    local runtime=$1
    check_private_directory "$runtime" || return 1
    check_owned_file "$runtime/server.log" || return 1
    check_owned_file "$runtime/server.previous.log" || return 1
    if [[ -s $runtime/server.log ]]; then mv -f "$runtime/server.log" "$runtime/server.previous.log" || return 1; fi
}

set_login_start() (
    local program=$1 login=$2 user_home previous plist login_plist target domain state pid='' loaded=0 staged='' preference=''
    [[ $login == 0 || $login == 1 ]] || { fail 'Invalid login preference.'; return 1; }
    user_home=$(desktop_home)
    check_private_directory "$program" || return 1
    check_owned_file "$program/start-at-login" || return 1
    previous=$(< "$program/start-at-login")
    [[ $previous == 0 || $previous == 1 ]] || { fail 'Invalid saved login preference.'; return 1; }
    plist=$program/page.suchi.local.plist
    login_plist=$user_home/Library/LaunchAgents/page.suchi.local.plist
    check_owned_directory "$user_home/Library" || return 1
    check_owned_directory "$user_home/Library/LaunchAgents" || return 1
    check_managed_file "$plist" || return 1
    check_managed_file "$login_plist" || return 1
    [[ -f $plist ]] || { fail 'The local service definition is missing. Run the installer again.'; return 1; }
    if [[ $login == "$previous" ]]; then return 0; fi
    domain=gui/$(id -u)
    target=$domain/page.suchi.local
    local expected=$plist
    [[ $previous == 0 ]] || expected=$login_plist
    if state=$(launchctl print "$target" 2>/dev/null); then
        pid=$(job_pid "$state" "$expected") || return 1
        loaded=1
    fi
    umask 077
    trap '[[ -z $staged ]] || rm -f "$staged"; [[ -z $preference ]] || rm -f "$preference"' EXIT
    preference=$(mktemp "$program/.login.XXXXXX") || return 1
    printf '%s\n' "$login" > "$preference" || return 1
    if [[ $login == 1 ]]; then
        mkdir -p "$user_home/Library/LaunchAgents" || return 1
        staged=$(mktemp "$user_home/Library/LaunchAgents/.suchi.XXXXXX") || return 1
        install -m 600 "$plist" "$staged" || return 1
    fi
    if [[ $loaded == 1 ]]; then stop_job "$target" "$pid" || return 1; fi
    if [[ $login == 1 ]]; then
        mv -f "$staged" "$login_plist" || return 1
        staged=''
        plist=$login_plist
    else
        rm -f "$login_plist" || return 1
    fi
    mv -f "$preference" "$program/start-at-login" || return 1
    preference=''
    if [[ -n $pid ]]; then launchctl bootstrap "$domain" "$plist" || return 1; fi
)
