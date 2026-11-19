#!/usr/bin/env bash
# Verifies the embedded profiles against the real clients (make capture-verify). For each client, in parallel:
# starts a capture tracker, starts the client's container, drives it (scripts/capture/<client>.sh), stops the
# tracker, then compares the capture to the embedded profile (seedmirage capture --compare).
# Exit status: 0 if every client matches its profile, 1 otherwise.
#
# Usage: scripts/capture/verify.sh [client ...]        (default: qbittorrent transmission deluge rtorrent)
#   <CLIENT>_VERSION  version of the embedded profile to compare to (defaults: see below)
#   <CLIENT>_PROFILE  profile name or file, overrides <CLIENT>_VERSION
#   QBITTORRENT_TAG, TRANSMISSION_TAG, DELUGE_TAG, RTORRENT_TAG  Docker image tags (default: derived from the version)
#   UPDATE_PROFILE=1  also write <profile>.candidate.json next to the capture (never overwrites an embedded profile)
#   INTERVAL          seconds between regular announces (client scripts have their own default)
# Needs Docker and bin/seedmirage (make build). Takes 10 to 15 minutes: libtorrent announces every 5 minutes at best.
# Not part of `make check` nor of the CI (heavy images, real wait times).
set -uo pipefail
cd "$(dirname "$0")/../.."

CLIENTS=("$@")
[[ ${#CLIENTS[@]} -gt 0 ]] || CLIENTS=(qbittorrent transmission deluge rtorrent)
# "a,b" is accepted as well as "a b".
CLIENTS=($(printf '%s\n' "${CLIENTS[@]}" | tr ',' '\n' | grep -v '^$'))

command -v docker > /dev/null || { echo "docker is required" >&2; exit 2; }
[[ -x bin/seedmirage ]] || { echo "bin/seedmirage is missing (make build)" >&2; exit 2; }

OUT=capture/verify
mkdir -p capture "$OUT"
COMPOSE_FILE=deploy/capture/docker-compose.yml

# default_version <client>: version of the embedded profile (the one to verify).
default_version() {
  case "$1" in
    qbittorrent) echo "${QBITTORRENT_VERSION:-5.3.0}" ;;
    transmission) echo "${TRANSMISSION_VERSION:-4.1.4}" ;;
    deluge) echo "${DELUGE_VERSION:-2.2.0}" ;;
    rtorrent) echo "${RTORRENT_VERSION:-0.16.28}" ;;
    *) return 1 ;;
  esac
}

# profile <client>
profile() {
  local override
  override="$(printenv "$(echo "$1" | tr a-z A-Z)_PROFILE" || true)"
  if [[ -n "$override" ]]; then echo "$override"; else echo "$1-$(default_version "$1")"; fi
}

# image tags derived from the profile version (compose defaults apply when nothing is derived).
export_tags() {
  local v
  case "$1" in
    qbittorrent)
      v="$(default_version qbittorrent)"
      # 4.x only exists with libtorrent 1.2 under this tag.
      if [[ -z "${QBITTORRENT_TAG:-}" ]]; then
        case "$v" in 4.*) export QBITTORRENT_TAG="$v-libtorrentv1" ;; *) export QBITTORRENT_TAG="$v" ;; esac
      fi ;;
    transmission)
      v="$(default_version transmission)"
      if [[ -z "${TRANSMISSION_TAG:-}" ]]; then
        case "$v" in 3.*) export TRANSMISSION_TAG="version-$v-r8" ;; *) export TRANSMISSION_TAG="$v" ;; esac
      fi ;;
    deluge) export DELUGE_TAG="${DELUGE_TAG:-$(default_version deluge)}" ;;
    rtorrent) : ;; # no image for 0.16.28: the compose default (0.16.25) applies unless RTORRENT_TAG is set
  esac
}

compose() { docker compose -f "$COMPOSE_FILE" --profile "$1" "${@:2}"; }

cleanup() {
  local c
  for c in "${CLIENTS[@]}"; do compose "$c" down > /dev/null 2>&1; done
}
trap cleanup EXIT INT TERM

# verify_one <client> <tracker port>: everything for one client; its log goes to $OUT/<client>.log.
verify_one() {
  local c="$1" port="$2" dir="$OUT/$1" prof pid i update=()
  prof="$(profile "$c")"
  rm -rf "$dir" "$OUT/$c.compare.txt"
  compose "$c" down > /dev/null 2>&1
  rm -rf "deploy/capture/config/$c"   # fresh client state: no torrent left from a previous run
  export_tags "$c"
  echo "== $c: profile $prof, tracker 127.0.0.1:$port"
  bin/seedmirage capture --listen "127.0.0.1:$port" --announce-host "host.docker.internal:$port" --out "$dir" > "$OUT/$c.tracker.log" 2>&1 &
  pid=$!
  # An interrupted run must not leave the tracker or the container behind.
  trap "kill -INT $pid 2> /dev/null; compose $c down > /dev/null 2>&1" EXIT
  for ((i = 0; i < 30; i++)); do [[ -f "$dir/manifest.json" ]] && break; sleep 1; done
  [[ -f "$dir/manifest.json" ]] || { echo "the capture tracker did not start:"; cat "$OUT/$c.tracker.log"; kill "$pid" 2> /dev/null; return 2; }
  compose "$c" up -d || { kill -INT "$pid"; wait "$pid"; return 2; }
  "scripts/capture/$c.sh" "$dir" || echo "the driving script failed (the capture may be partial)"
  kill -INT "$pid"; wait "$pid"
  compose "$c" down > /dev/null 2>&1
  [[ "${UPDATE_PROFILE:-}" == 1 ]] && update=(--update-profile)
  bin/seedmirage capture --compare "$prof" --out "$dir" ${update[@]+"${update[@]}"} > "$OUT/$c.compare.txt" 2>&1
  local rc=$?
  cat "$OUT/$c.compare.txt"
  return $rc
}

pids=()
port=6969
for c in "${CLIENTS[@]}"; do
  if ! default_version "$c" > /dev/null; then echo "unknown client: $c" >&2; exit 2; fi
  ( verify_one "$c" "$port" ) > "$OUT/$c.log" 2>&1 &
  pids+=($!)
  port=$((port + 1))
done

status=0
for i in "${!CLIENTS[@]}"; do
  c="${CLIENTS[$i]}"
  wait "${pids[$i]}"; rc=$?
  echo
  echo "################ $c (profile $(profile "$c"))"
  # The report only (the driving script's log stays in $OUT/$c.log).
  if [[ -f "$OUT/$c.compare.txt" ]]; then cat "$OUT/$c.compare.txt"; else tail -n 20 "$OUT/$c.log"; fi
  case $rc in
    0) result="$c: OK, identical" ;;
    1) result="$c: DIFFERENCES"; status=1 ;;
    *) result="$c: ERROR (see $OUT/$c.log)"; status=1 ;;
  esac
  SUMMARY+=("$result")
done
echo
echo "================ summary"
printf '%s\n' "${SUMMARY[@]}"
exit $status
