#!/bin/sh
# Le dépôt est public : vérifie que les fichiers suivis par git ne contiennent rien de
# personnel ni d'interne. Lancé par `make check`. Échoue (exit 1) si un fichier suivi :
#  - est un fichier de travail personnel (instructions d'agent, plan, suivi des tâches) ;
#  - y fait référence, ou cite un identifiant de tâche interne (T suivi de 3 chiffres) ;
#  - contient un chemin de home, une adresse e-mail réelle ou l'e-mail git de l'auteur.
# Ces fichiers personnels restent en local, ignorés via .git/info/exclude.
set -eu
cd "$(git rev-parse --show-toplevel)"

fail=0
err() { echo "public-check: ÉCHEC : $*" >&2; fail=1; }
show() { [ -n "$1" ] && { err "$2"; echo "$1" >&2; }; return 0; }

personal='^(CLAUDE\.md|AGENTS\.md|PLAN\.md|tasks/|docs/progress/|docs/decisions\.md|docs/joal-reference\.md|\.claude/|\.cursor|\.aider|\.windsurfrules)'
show "$(git ls-files | grep -E "$personal" || true)" "fichier personnel suivi par git (git rm --cached) :"

show "$(git grep -n -I -E 'PLAN\.md|AGENTS\.md|CLAUDE\.md|tasks/T|docs/progress|decisions\.md|joal-reference|\.claude/' -- . ':!tools/public-check.sh' || true)" \
	"référence à un fichier personnel :"
show "$(git grep -n -I -E '(^|[^A-Za-z0-9_])T0[0-9]{2}([^0-9]|$)' -- . ':!tools/public-check.sh' || true)" "identifiant de tâche interne :"

# Données personnelles. Exceptions : exemples génériques (/home/user…), noreply@…, domaines
# d'exemple ou réservés (example.com|org|net, .example, .test, .invalid).
show "$(git grep -n -I -E '/(Users|home)/[^/ ]+' -- . ':!tools/public-check.sh' \
	| grep -v -E '/(Users|home)/(user|username|you|name)([/ ]|$)' || true)" "chemin personnel :"
show "$(git grep -n -I -o -E '[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}' -- . ':!tools/public-check.sh' \
	| grep -v -i -E 'noreply|@(([A-Za-z0-9-]+\.)*example\.(com|org|net)|[A-Za-z0-9.-]*\.(example|test|invalid))$' || true)" "adresse e-mail :"
# E-mails git de l'auteur (dépôt et global : le dépôt peut utiliser une adresse noreply).
for email in $(git config --local user.email || true) $(git config --global user.email || true); do
	show "$(git grep -n -I -F -i -e "$email" -- . || true)" "e-mail git de l'auteur présent :"
done

[ "$fail" -eq 0 ] || exit 1
echo "public-check: OK"
