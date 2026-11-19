# Docker (local)

seedmirage tourne en conteneur **sur votre machine** ; l'UI n'est joignable que depuis celle-ci
(`127.0.0.1:7070`). Pas de VPS, pas de TLS.

```bash
mkdir -p data                                 # propriétaire = votre utilisateur (voir UID/GID)
docker compose up -d --build                  # construit seedmirage:local depuis les sources et le lance
docker compose logs seedmirage | grep token   # lien d'accès avec le token
docker compose down                           # envoie les `stopped` (délai max 15 s)
```

`compose.yaml` est à la racine du dépôt (image distroless). Sans `mkdir -p data` préalable, Docker crée
`data/` en root et le conteneur (UID/GID `1000:1000` par défaut) ne peut pas y écrire. Pour changer un
réglage, copier `.env.example` en `.env`. `make up` / `make down` sont des raccourcis ; `make docker`
construit seulement l'image (`seedmirage:dev`).

Ouvrir le lien `http://127.0.0.1:7070/?token=…` affiché dans les logs. Les `.torrent` se déposent dans
`./data/torrents/`.

## Variables

| Variable | Rôle |
|---|---|
| `SEEDMIRAGE_IMAGE` | Nom de l'image construite (défaut `seedmirage:local`). |
| `SEEDMIRAGE_VERSION` | Version injectée dans le binaire (défaut `dev`). |
| `SEEDMIRAGE_UID` / `SEEDMIRAGE_GID` | UID/GID de l'hôte pour les fichiers de `data/` (défaut `1000:1000` ; `id -u`, `id -g`). |
| `SEEDMIRAGE_UI_TOKEN` | Fixe le token d'accès : au moins 32 caractères parmi lettres, chiffres, `-_.~` (`openssl rand -hex 32`). Prime sur `data/ui-token` sans l'écraser. Un token invalide arrête le démarrage avec un message clair (et `restart: unless-stopped` relance en boucle : corriger la valeur). |
| `SEEDMIRAGE_LOG_FORMAT` | `json` (défaut dans l'image) ou `text`. Équivalent de `--log-format`. |
| `TZ` | Fuseau horaire (défaut `Etc/UTC`). |

Dans l'image : `SEEDMIRAGE_LISTEN=0.0.0.0:7070` et `SEEDMIRAGE_IN_CONTAINER=1` (pas d'avertissement « adresse non
loopback » : c'est le port publié, `127.0.0.1:7070:7070`, qui limite l'exposition). Le contrôle de `Host`
(localhost, 127.0.0.1, ::1, `SEEDMIRAGE_ALLOWED_HOSTS`) reste actif.

## Healthcheck

`seedmirage healthcheck` fait un `GET /healthz` sur l'adresse d'écoute locale (`--listen`, sinon
`SEEDMIRAGE_LISTEN`), code de sortie 0/1, sans token. Il sert au `HEALTHCHECK` de l'image (pas de curl dans distroless).

## Joindre un tracker de l'hôte

Le compose ajoute `host.docker.internal` (`host-gateway`) : un tracker qui tourne sur la machine
(ex. `go run ./scripts/demo-tracker --listen 0.0.0.0:6969`) est joignable à `http://host.docker.internal:6969/announce`.
