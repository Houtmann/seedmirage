#!/usr/bin/env bash
# Pilote le rTorrent du profil compose « rtorrent » (JSON-RPC de rTorrent 0.16, via nginx du conteneur)
# selon la procédure de docs/capture.md. Les .torrent sont copiés dans le dossier surveillé.
# Usage : scripts/capture/rtorrent.sh <dossier de capture>
set -euo pipefail
cd "$(dirname "$0")/../.."
source scripts/capture/lib.sh
DIR="${1:?dossier de capture}"
COMPOSE=(docker compose -f deploy/capture/docker-compose.yml --profile rtorrent)
CT=capture-rtorrent-1
WATCH=deploy/capture/config/rtorrent/rtorrent/watch

# rpc <méthode> '<params json>' : JSON-RPC (le XML-RPC n'est pas compilé dans l'image).
rpc() { docker exec "$CT" curl -s -H 'Content-Type: application/json' -d "{\"jsonrpc\":\"2.0\",\"method\":\"$1\",\"params\":$2,\"id\":1}" http://127.0.0.1:8000/; }
hashes() { rpc d.multicall2 '["","main","d.hash="]' | python3 -c 'import sys,json;print(" ".join(r[0] for r in json.load(sys.stdin)["result"]))'; }
wait_rpc() { local i; for ((i = 0; i < 30; i++)); do rpc system.client_version '[]' | grep -q result && return 0; sleep 2; done; return 1; }

step "attente du client"
wait_rpc
chmod -R a+rwX "$WATCH" 2> /dev/null || true
for t in $(torrents "$DIR"); do cp "$t" "$WATCH/"; done
chmod a+r "$WATCH"/*.torrent
wait_for 15 "chargement par le dossier surveillé"
step "torrents chargés : $(hashes)"

wait_for $((INTERVAL * 2 + 30)) "deux annonces régulières"
first="$(hashes | cut -d' ' -f1)"
step "pause / reprise de $first"
rpc d.stop "[\"$first\"]" > /dev/null
wait_for 10 "pause"
rpc d.start "[\"$first\"]" > /dev/null
wait_for 15 "reprise"

step "redémarrage du client"
"${COMPOSE[@]}" stop -t 15 rtorrent
wait_for 10 "client arrêté"
"${COMPOSE[@]}" start rtorrent
wait_rpc
wait_for $((INTERVAL + 30)) "annonce régulière après redémarrage"

step "retrait des torrents"
for h in $(hashes); do rpc d.erase "[\"$h\"]" > /dev/null; done
wait_for 10 "stopped"
step "terminé : Ctrl-C dans seedmirage capture"
