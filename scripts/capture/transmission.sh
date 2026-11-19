#!/usr/bin/env bash
# Pilote le Transmission du profil compose « transmission » (RPC) selon la procédure de docs/capture.md.
# Usage : scripts/capture/transmission.sh <dossier de capture>
set -euo pipefail
cd "$(dirname "$0")/../.."
source scripts/capture/lib.sh
DIR="${1:?dossier de capture}"
COMPOSE=(docker compose -f deploy/capture/docker-compose.yml --profile transmission)
URL=http://127.0.0.1:9091/transmission/rpc

# rpc '<json>' : gère la poignée de main X-Transmission-Session-Id (409).
rpc() {
  local sid
  sid="$(curl -s -i "$URL" | tr -d '\r' | awk -F': ' 'tolower($1)=="x-transmission-session-id"{print $2}')"
  curl -s -H "X-Transmission-Session-Id: $sid" -d "$1" "$URL"
}
ids() { rpc '{"method":"torrent-get","arguments":{"fields":["id"]}}' | python3 -c 'import sys,json;print(" ".join(str(t["id"]) for t in json.load(sys.stdin)["arguments"]["torrents"]))'; }

step "attente du client"
wait_http "$URL" 60
for t in $(torrents "$DIR"); do
  b64="$(base64 < "$t" | tr -d '\n')"
  rpc "{\"method\":\"torrent-add\",\"arguments\":{\"metainfo\":\"$b64\",\"download-dir\":\"/downloads\"}}" > /dev/null
done
step "torrents ajoutés : $(ids)"

wait_for $((INTERVAL * 2 + 20)) "deux annonces régulières"
first="$(ids | cut -d' ' -f1)"
step "pause / reprise de $first"
rpc "{\"method\":\"torrent-stop\",\"arguments\":{\"ids\":[$first]}}" > /dev/null
wait_for 10 "pause"
rpc "{\"method\":\"torrent-start-now\",\"arguments\":{\"ids\":[$first]}}" > /dev/null
wait_for 15 "reprise"

step "redémarrage du client"
"${COMPOSE[@]}" stop -t 15 transmission
wait_for 10 "client arrêté"
"${COMPOSE[@]}" start transmission
wait_http "$URL" 60
wait_for $((INTERVAL + 20)) "annonce régulière après redémarrage"

step "retrait des torrents"
rpc '{"method":"torrent-remove","arguments":{"ids":['"$(ids | tr ' ' ',')"'],"delete-local-data":false}}' > /dev/null
wait_for 10 "stopped"
step "terminé : Ctrl-C dans seedmirage capture"
