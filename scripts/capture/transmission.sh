#!/usr/bin/env bash
# Drives the Transmission of the "transmission" compose profile (RPC) following the procedure in docs/capture.md.
# Usage: scripts/capture/transmission.sh <capture dir>
set -euo pipefail
cd "$(dirname "$0")/../.."
source scripts/capture/lib.sh
DIR="${1:?capture dir}"
COMPOSE=(docker compose -f deploy/capture/docker-compose.yml --profile transmission)
URL=http://127.0.0.1:9091/transmission/rpc

# rpc '<json>': handles the X-Transmission-Session-Id handshake (409).
rpc() {
  local sid
  sid="$(curl -s -i "$URL" | tr -d '\r' | awk -F': ' 'tolower($1)=="x-transmission-session-id"{print $2}')"
  curl -s -H "X-Transmission-Session-Id: $sid" -d "$1" "$URL"
}
ids() { rpc '{"method":"torrent-get","arguments":{"fields":["id"]}}' | python3 -c 'import sys,json;print(" ".join(str(t["id"]) for t in json.load(sys.stdin)["arguments"]["torrents"]))'; }

step "waiting for the client"
wait_http "$URL" 60
for t in $(torrents "$DIR"); do
  b64="$(base64 < "$t" | tr -d '\n')"
  rpc "{\"method\":\"torrent-add\",\"arguments\":{\"metainfo\":\"$b64\",\"download-dir\":\"/downloads\"}}" > /dev/null
done
step "torrents added: $(ids)"

wait_for $((INTERVAL * 2 + 20)) "two regular announces"
first="$(ids | cut -d' ' -f1)"
step "pause / resume of $first"
rpc "{\"method\":\"torrent-stop\",\"arguments\":{\"ids\":[$first]}}" > /dev/null
wait_for 10 "pause"
rpc "{\"method\":\"torrent-start-now\",\"arguments\":{\"ids\":[$first]}}" > /dev/null
wait_for 15 "resume"

step "client restart"
"${COMPOSE[@]}" stop -t 15 transmission
wait_for 10 "client stopped"
"${COMPOSE[@]}" start transmission
wait_http "$URL" 60
wait_for $((INTERVAL + 20)) "regular announce after restart"

step "removing the torrents"
rpc '{"method":"torrent-remove","arguments":{"ids":['"$(ids | tr ' ' ',')"'],"delete-local-data":false}}' > /dev/null
wait_for 10 "stopped"
step "done: Ctrl-C in seedmirage capture"
