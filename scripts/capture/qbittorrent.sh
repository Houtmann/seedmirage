#!/usr/bin/env bash
# Pilote le qBittorrent du profil compose « qbittorrent » (WebUI) selon la procédure de docs/capture.md :
# ajout, annonces régulières, pause/reprise d'un torrent, redémarrage du client, retrait.
# Prérequis : seedmirage capture et `docker compose -f deploy/capture/docker-compose.yml --profile qbittorrent up -d`.
# Usage : scripts/capture/qbittorrent.sh <dossier de capture>
set -euo pipefail
cd "$(dirname "$0")/../.."
# libtorrent borne l'intervalle d'annonce à 5 minutes (min_announce_interval) : l'intervalle de 60 s du tracker de capture est ignoré.
INTERVAL="${INTERVAL:-300}"
source scripts/capture/lib.sh
DIR="${1:?dossier de capture}"
COMPOSE=(docker compose -f deploy/capture/docker-compose.yml --profile qbittorrent)
API=http://127.0.0.1:8080/api/v2
JAR="$(mktemp)"

login() {
  wait_http "$API/app/version" 60
  local pass
  # qBittorrent 5 : mot de passe temporaire écrit dans les logs à chaque démarrage tant qu'aucun n'est défini.
  pass="$("${COMPOSE[@]}" logs qbittorrent 2>&1 | grep -o 'session: [^ ]*' | tail -1 | cut -d' ' -f2)"
  # 4.x répond 200 « Ok. », 5.x répond 204.
  local code
  code="$(curl -s -o /dev/null -w '%{http_code}' -c "$JAR" --data-urlencode "username=admin" --data-urlencode "password=$pass" "$API/auth/login")"
  [[ "$code" == 200 || "$code" == 204 ]] || { echo "connexion refusée ($code)" >&2; return 1; }
}

step "connexion"
login
# Sans cela, la file d'attente de qBittorrent (3 téléchargements actifs) retient des torrents : ils n'annonceraient pas.
curl -s -b "$JAR" --data-urlencode 'json={"queueing_enabled":false}' "$API/app/setPreferences" > /dev/null
for t in $(torrents "$DIR"); do
  curl -s -b "$JAR" -F "torrents=@$t" -F "savepath=/downloads" "$API/torrents/add" > /dev/null
done
hashes() { curl -s -b "$JAR" "$API/torrents/info" | python3 -c 'import sys,json;print("|".join(t["hash"] for t in json.load(sys.stdin)))'; }
step "torrents ajoutés : $(curl -s -b "$JAR" "$API/torrents/info" | python3 -c 'import sys,json;print(len(json.load(sys.stdin)))')"

wait_for $((INTERVAL + 30)) "une annonce régulière (libtorrent : 5 min mini)"
first="$(hashes | cut -d'|' -f1)"
step "pause / reprise de $first"
# 5.x : stop/start ; 4.x : pause/resume.
act() { [[ "$(curl -s -o /dev/null -w '%{http_code}' -b "$JAR" -d "hashes=$first" "$API/torrents/$1")" == 200 ]] || curl -s -b "$JAR" -d "hashes=$first" "$API/torrents/$2" > /dev/null; }
act stop pause
wait_for 10 "pause"
act start resume
wait_for 15 "reprise"

step "redémarrage du client"
"${COMPOSE[@]}" stop -t 15 qbittorrent
wait_for 10 "client arrêté"
"${COMPOSE[@]}" start qbittorrent
login
wait_for $((INTERVAL + 20)) "annonce régulière après redémarrage"

step "retrait des torrents"
curl -s -b "$JAR" -d "hashes=all" -d "deleteFiles=false" "$API/torrents/delete" > /dev/null
wait_for 10 "stopped"
rm -f "$JAR"
step "terminé : Ctrl-C dans seedmirage capture"
