#!/usr/bin/env bash
# Drives the qBittorrent of the "qbittorrent" compose profile (WebUI) following the procedure in docs/capture.md:
# add, regular announces, pause/resume of a torrent, client restart, removal.
# Prerequisites: seedmirage capture and `docker compose -f deploy/capture/docker-compose.yml --profile qbittorrent up -d`.
# Usage: scripts/capture/qbittorrent.sh <capture dir>
set -euo pipefail
cd "$(dirname "$0")/../.."
# libtorrent clamps the announce interval to 5 minutes (min_announce_interval): the capture tracker's 60 s interval is ignored.
INTERVAL="${INTERVAL:-300}"
source scripts/capture/lib.sh
DIR="${1:?capture dir}"
COMPOSE=(docker compose -f deploy/capture/docker-compose.yml --profile qbittorrent)
API=http://127.0.0.1:8080/api/v2
JAR="$(mktemp)"

login() {
  wait_http "$API/app/version" 60
  local pass
  # qBittorrent 5: temporary password written to the logs at every start as long as none is set.
  pass="$("${COMPOSE[@]}" logs qbittorrent 2>&1 | grep -o 'session: [^ ]*' | tail -1 | cut -d' ' -f2)"
  # 4.x answers 200 "Ok.", 5.x answers 204.
  local code
  code="$(curl -s -o /dev/null -w '%{http_code}' -c "$JAR" --data-urlencode "username=admin" --data-urlencode "password=$pass" "$API/auth/login")"
  [[ "$code" == 200 || "$code" == 204 ]] || { echo "login refused ($code)" >&2; return 1; }
}

step "login"
login
# Without this, qBittorrent's queue (3 active downloads) holds torrents back: they would not announce.
curl -s -b "$JAR" --data-urlencode 'json={"queueing_enabled":false}' "$API/app/setPreferences" > /dev/null
for t in $(torrents "$DIR"); do
  curl -s -b "$JAR" -F "torrents=@$t" -F "savepath=/downloads" "$API/torrents/add" > /dev/null
done
hashes() { curl -s -b "$JAR" "$API/torrents/info" | python3 -c 'import sys,json;print("|".join(t["hash"] for t in json.load(sys.stdin)))'; }
step "torrents added: $(curl -s -b "$JAR" "$API/torrents/info" | python3 -c 'import sys,json;print(len(json.load(sys.stdin)))')"

wait_for $((INTERVAL + 30)) "one regular announce (libtorrent: 5 min minimum)"
first="$(hashes | cut -d'|' -f1)"
step "pause / resume of $first"
# 5.x: stop/start; 4.x: pause/resume.
act() { [[ "$(curl -s -o /dev/null -w '%{http_code}' -b "$JAR" -d "hashes=$first" "$API/torrents/$1")" == 200 ]] || curl -s -b "$JAR" -d "hashes=$first" "$API/torrents/$2" > /dev/null; }
act stop pause
wait_for 10 "pause"
act start resume
wait_for 15 "resume"

step "client restart"
"${COMPOSE[@]}" stop -t 15 qbittorrent
wait_for 10 "client stopped"
"${COMPOSE[@]}" start qbittorrent
login
wait_for $((INTERVAL + 20)) "regular announce after restart"

step "removing the torrents"
curl -s -b "$JAR" -d "hashes=all" -d "deleteFiles=false" "$API/torrents/delete" > /dev/null
wait_for 10 "stopped"
rm -f "$JAR"
step "done: Ctrl-C in seedmirage capture"
