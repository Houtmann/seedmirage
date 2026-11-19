#!/usr/bin/env bash
# Drives the Deluge of the "deluge" compose profile (WebUI JSON-RPC, default password "deluge") following the procedure in docs/capture.md.
# Usage: scripts/capture/deluge.sh <capture dir>
set -euo pipefail
cd "$(dirname "$0")/../.."
# libtorrent clamps the announce interval to 5 minutes (min_announce_interval): the capture tracker's 60 s interval is ignored.
INTERVAL="${INTERVAL:-300}"
source scripts/capture/lib.sh
DIR="${1:?capture dir}"
COMPOSE=(docker compose -f deploy/capture/docker-compose.yml --profile deluge)
URL=http://127.0.0.1:8112/json
JAR="$(mktemp)"
N=0

# call <method> '<params json>': one WebUI JSON-RPC call.
call() { N=$((N + 1)); curl -s -b "$JAR" -c "$JAR" -H 'Content-Type: application/json' -d "{\"method\":\"$1\",\"params\":$2,\"id\":$N}" "$URL"; }
login() {
  wait_http "$URL" 60
  call auth.login '["deluge"]' | grep -q '"result": *true'
  # Connect to the daemon (the first registered host).
  local host
  host="$(call web.get_hosts '[]' | python3 -c 'import sys,json;print(json.load(sys.stdin)["result"][0][0])')"
  call web.connect "[\"$host\"]" > /dev/null
  sleep 3
}
hashes() { call web.update_ui '[["name"],{}]' | python3 -c 'import sys,json;print(" ".join(json.load(sys.stdin)["result"]["torrents"].keys()))'; }

step "login"
login
for t in $(torrents "$DIR"); do
  b64="$(base64 < "$t" | tr -d '\n')"
  call core.add_torrent_file "[\"$(basename "$t")\",\"$b64\",{\"download_location\":\"/downloads\"}]" > /dev/null
done
step "torrents added: $(hashes)"

wait_for $((INTERVAL + 30)) "one regular announce (libtorrent: 5 min minimum)"
first="$(hashes | cut -d' ' -f1)"
step "pause / resume of $first"
call core.pause_torrent "[[\"$first\"]]" > /dev/null
wait_for 10 "pause"
call core.resume_torrent "[[\"$first\"]]" > /dev/null
wait_for 15 "resume"

step "client restart"
"${COMPOSE[@]}" stop -t 15 deluge
wait_for 10 "client stopped"
"${COMPOSE[@]}" start deluge
login
wait_for $((INTERVAL + 20)) "regular announce after restart"

step "removing the torrents"
for h in $(hashes); do call core.remove_torrent "[\"$h\",false]" > /dev/null; done
wait_for 10 "stopped"
rm -f "$JAR"
step "done: Ctrl-C in seedmirage capture"
