#!/usr/bin/env bash
# Helpers shared by the driver scripts: waiting, announcing steps.
# Usage: source lib.sh. Variables: INTERVAL (seconds between two regular announces, 60 by default).
set -euo pipefail

: "${INTERVAL:=60}"  # scripts may set it before sourcing this file

step() { printf '\n[%s] %s\n' "$(date +%H:%M:%S)" "$*"; }

# wait_for <seconds> <reason>: explicit wait (the client has no event to watch for).
wait_for() { step "waiting ${1}s: ${2}"; sleep "$1"; }

# torrents <dir>: lists the .torrent files to add.
torrents() { find "$1" -maxdepth 1 -name 'capture-*.torrent' | sort; }

# wait_http <url> <tries>: waits for the client UI to answer.
wait_http() {
  local i
  for ((i = 0; i < $2; i++)); do
    if curl -s -o /dev/null --max-time 2 "$1"; then return 0; fi
    sleep 2
  done
  echo "the client does not answer on $1" >&2
  return 1
}
