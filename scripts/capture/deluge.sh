#!/usr/bin/env bash
# Pilote le Deluge du profil compose « deluge » (JSON-RPC du WebUI, mot de passe par défaut « deluge ») selon la procédure de docs/capture.md.
# Usage : scripts/capture/deluge.sh <dossier de capture>
set -euo pipefail
cd "$(dirname "$0")/../.."
# libtorrent borne l'intervalle d'annonce à 5 minutes (min_announce_interval) : l'intervalle de 60 s du tracker de capture est ignoré.
INTERVAL="${INTERVAL:-300}"
source scripts/capture/lib.sh
DIR="${1:?dossier de capture}"
COMPOSE=(docker compose -f deploy/capture/docker-compose.yml --profile deluge)
URL=http://127.0.0.1:8112/json
JAR="$(mktemp)"
N=0

# call <méthode> '<params json>' : un appel JSON-RPC du WebUI.
call() { N=$((N + 1)); curl -s -b "$JAR" -c "$JAR" -H 'Content-Type: application/json' -d "{\"method\":\"$1\",\"params\":$2,\"id\":$N}" "$URL"; }
login() {
  wait_http "$URL" 60
  call auth.login '["deluge"]' | grep -q '"result": *true'
  # Se connecter au démon (le premier hôte enregistré).
  local host
  host="$(call web.get_hosts '[]' | python3 -c 'import sys,json;print(json.load(sys.stdin)["result"][0][0])')"
  call web.connect "[\"$host\"]" > /dev/null
  sleep 3
}
hashes() { call web.update_ui '[["name"],{}]' | python3 -c 'import sys,json;print(" ".join(json.load(sys.stdin)["result"]["torrents"].keys()))'; }

step "connexion"
login
for t in $(torrents "$DIR"); do
  b64="$(base64 < "$t" | tr -d '\n')"
  call core.add_torrent_file "[\"$(basename "$t")\",\"$b64\",{\"download_location\":\"/downloads\"}]" > /dev/null
done
step "torrents ajoutés : $(hashes)"

wait_for $((INTERVAL + 30)) "une annonce régulière (libtorrent : 5 min mini)"
first="$(hashes | cut -d' ' -f1)"
step "pause / reprise de $first"
call core.pause_torrent "[[\"$first\"]]" > /dev/null
wait_for 10 "pause"
call core.resume_torrent "[[\"$first\"]]" > /dev/null
wait_for 15 "reprise"

step "redémarrage du client"
"${COMPOSE[@]}" stop -t 15 deluge
wait_for 10 "client arrêté"
"${COMPOSE[@]}" start deluge
login
wait_for $((INTERVAL + 20)) "annonce régulière après redémarrage"

step "retrait des torrents"
for h in $(hashes); do call core.remove_torrent "[\"$h\",false]" > /dev/null; done
wait_for 10 "stopped"
rm -f "$JAR"
step "terminé : Ctrl-C dans seedmirage capture"
