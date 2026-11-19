#!/usr/bin/env bash
# Drives the rTorrent of the "rtorrent" compose profile (rTorrent 0.16 JSON-RPC, through the container's nginx)
# following the procedure in docs/capture.md. The .torrent files are copied into the watched folder.
# Usage: scripts/capture/rtorrent.sh <capture dir>
set -euo pipefail
cd "$(dirname "$0")/../.."
source scripts/capture/lib.sh
DIR="${1:?capture dir}"
COMPOSE=(docker compose -f deploy/capture/docker-compose.yml --profile rtorrent)
CT=capture-rtorrent-1
WATCH=deploy/capture/config/rtorrent/rtorrent/watch

# rpc <method> '<params json>': JSON-RPC (XML-RPC is not compiled into the image).
rpc() { docker exec "$CT" curl -s -H 'Content-Type: application/json' -d "{\"jsonrpc\":\"2.0\",\"method\":\"$1\",\"params\":$2,\"id\":1}" http://127.0.0.1:8000/; }
hashes() { rpc d.multicall2 '["","main","d.hash="]' | python3 -c 'import sys,json;print(" ".join(r[0] for r in json.load(sys.stdin)["result"]))'; }
wait_rpc() { local i; for ((i = 0; i < 30; i++)); do rpc system.client_version '[]' | grep -q result && return 0; sleep 2; done; return 1; }

step "waiting for the client"
wait_rpc
chmod -R a+rwX "$WATCH" 2> /dev/null || true
for t in $(torrents "$DIR"); do cp "$t" "$WATCH/"; done
chmod a+r "$WATCH"/*.torrent
wait_for 15 "loading through the watched folder"
step "torrents loaded: $(hashes)"

wait_for $((INTERVAL * 2 + 30)) "two regular announces"
first="$(hashes | cut -d' ' -f1)"
step "pause / resume of $first"
rpc d.stop "[\"$first\"]" > /dev/null
wait_for 10 "pause"
rpc d.start "[\"$first\"]" > /dev/null
wait_for 15 "resume"

step "client restart"
"${COMPOSE[@]}" stop -t 15 rtorrent
wait_for 10 "client stopped"
"${COMPOSE[@]}" start rtorrent
wait_rpc
wait_for $((INTERVAL + 30)) "regular announce after restart"

step "removing the torrents"
for h in $(hashes); do rpc d.erase "[\"$h\"]" > /dev/null; done
wait_for 10 "stopped"
step "done: Ctrl-C in seedmirage capture"
