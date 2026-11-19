#!/usr/bin/env bash
# Fonctions communes des scripts de pilotage : attendre, annoncer les étapes.
# Usage : source lib.sh. Variables : INTERVAL (secondes entre deux annonces régulières, 60 par défaut).
set -euo pipefail

: "${INTERVAL:=60}"  # les scripts peuvent le fixer avant de sourcer ce fichier

step() { printf '\n[%s] %s\n' "$(date +%H:%M:%S)" "$*"; }

# wait_for <secondes> <raison> : attente explicite (le client n'a pas d'événement à guetter).
wait_for() { step "attente ${1}s : ${2}"; sleep "$1"; }

# torrents <dossier> : liste les .torrent à ajouter.
torrents() { find "$1" -maxdepth 1 -name 'capture-*.torrent' | sort; }

# wait_http <url> <essais> : attend que l'interface du client réponde.
wait_http() {
  local i
  for ((i = 0; i < $2; i++)); do
    if curl -s -o /dev/null --max-time 2 "$1"; then return 0; fi
    sleep 2
  done
  echo "le client ne répond pas sur $1" >&2
  return 1
}
