# Docker (local)

seedmirage runs in a container **on your machine**; the interface is reachable only from that machine
(`127.0.0.1:7070`). No VPS, no TLS.

```bash
mkdir -p data                                 # owned by your user (see UID/GID)
docker compose up -d --build                  # builds the image from the sources and starts it
docker compose logs seedmirage | grep token   # access link with the token
docker compose down                           # sends the `stopped` announces (15 s at most)
```

`compose.yaml` is at the root of the repository (distroless image). Without a prior `mkdir -p data`, Docker creates
`data/` as root and the container (UID/GID `1000:1000` by default) cannot write to it. To change a setting, copy
`.env.example` to `.env`. `make up` / `make down` are shortcuts; `make docker` only builds the image (`seedmirage:dev`).

Open the `http://127.0.0.1:7070/?token=...` link printed in the logs. Drop your `.torrent` files in
`./data/torrents/`, or add them from the interface.

## Variables

| Variable | Purpose |
|---|---|
| `SEEDMIRAGE_IMAGE` | Image name (default `ghcr.io/houtmann/seedmirage:latest`; `--build` tags the locally built image with it, `docker compose pull` fetches the published one). |
| `SEEDMIRAGE_VERSION` | Version injected in the binary (default `dev`). |
| `SEEDMIRAGE_UID` / `SEEDMIRAGE_GID` | Host UID/GID for the files in `data/` (default `1000:1000`; `id -u`, `id -g`). |
| `SEEDMIRAGE_UI_TOKEN` | Fixes the access token: at least 32 characters among letters, digits and `-_.~` (`openssl rand -hex 32`). It takes precedence over `data/ui-token` without overwriting it. An invalid token stops the startup with a clear message (and `restart: unless-stopped` restarts it in a loop: fix the value). |
| `SEEDMIRAGE_LOG_FORMAT` | `json` (default in the image) or `text`. Same as `--log-format`. |
| `TZ` | Time zone (default `Etc/UTC`); it is the zone of the hourly schedule when none is set in the settings. |

In the image: `SEEDMIRAGE_LISTEN=0.0.0.0:7070` and `SEEDMIRAGE_IN_CONTAINER=1` (no "non-loopback address" warning: it is the
published port, `127.0.0.1:7070:7070`, that limits the exposure). The `Host` check (localhost, 127.0.0.1, ::1,
`SEEDMIRAGE_ALLOWED_HOSTS`) stays active.

## Healthcheck

`seedmirage healthcheck` sends a `GET /healthz` to the local listen address (`--listen`, otherwise
`SEEDMIRAGE_LISTEN`) and exits with code 0 or 1, without a token. The image uses it as its `HEALTHCHECK` (there is no curl
in distroless).

## Reaching a tracker on the host

The compose file adds `host.docker.internal` (`host-gateway`): a tracker running on the machine
(for instance `go run ./scripts/demo-tracker --listen 0.0.0.0:6969`) is reachable at `http://host.docker.internal:6969/announce`.
