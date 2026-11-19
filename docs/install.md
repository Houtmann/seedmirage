# Installation

seedmirage runs **on your own machine**, either as a single binary or in a local Docker container. The web interface
listens on `127.0.0.1:7070` by default and is not meant to be exposed to the Internet (there is no TLS).

Read the usage warning in the [README](../README.md) first: most private trackers forbid this kind of tool.

- [Binary](#binary): macOS, Linux, Windows
- [Docker](#docker)
- [Where the data lives](#where-the-data-lives)
- [Upgrade](#upgrade)
- [Uninstall](#uninstall)

## Binary

### Build from source

Requirements: Go 1.26 or later, `make`, `git`.

```bash
git clone https://github.com/Houtmann/seedmirage.git
cd seedmirage
make build                       # writes bin/seedmirage
./bin/seedmirage version
```

To build archives for every supported platform (macOS arm64 and amd64, Linux amd64 and arm64, Windows amd64) in `dist/`,
with a `SHA256SUMS` file:

```bash
make dist
```

### Use a release archive

If you have an archive (`seedmirage_<version>_<os>_<arch>.tar.gz`, or `.zip` on Windows), check it against `SHA256SUMS`
(`shasum -a 256 -c SHA256SUMS --ignore-missing` on macOS, `sha256sum -c SHA256SUMS --ignore-missing` on Linux), then extract it.
It contains a single directory with the `seedmirage` executable.

### macOS

```bash
tar xzf seedmirage_<version>_darwin_arm64.tar.gz
cd seedmirage_<version>_darwin_arm64
```

The executable is not signed. A file downloaded with a browser carries the quarantine attribute, and Gatekeeper refuses to
run it ("cannot be opened because the developer cannot be verified"). Remove the attribute once, after you have checked the
checksum:

```bash
xattr -d com.apple.quarantine ./seedmirage
./seedmirage serve
```

A binary you built yourself with `make build` is not quarantined.

### Linux

```bash
tar xzf seedmirage_<version>_linux_amd64.tar.gz
install -m 0755 seedmirage_<version>_linux_amd64/seedmirage ~/.local/bin/seedmirage   # or any folder in your PATH
seedmirage serve
```

Without a graphical session (`DISPLAY` and `WAYLAND_DISPLAY` unset), the browser is not opened: copy the access link
printed in the terminal instead.

### Windows

Extract the `.zip`, then in PowerShell:

```powershell
cd seedmirage_<version>_windows_amd64
.\seedmirage.exe serve
```

Windows may show a SmartScreen warning for an unsigned executable; check the checksum before choosing to run it.

### First launch

```bash
seedmirage serve
```

On the first launch seedmirage creates the data folder (see below), generates a random access token in `ui-token`,
and opens `http://127.0.0.1:7070/?token=...` in your browser. The link also appears in the terminal (`UI ready` line).
Opening it stores a session cookie, so the token is not needed afterwards in the same browser.

Useful flags (each one has an environment variable, see `seedmirage serve -h`):

| Flag | Environment variable | Purpose |
|---|---|---|
| `--data-dir` | `SEEDMIRAGE_DATA_DIR` | Data folder (default: see below). |
| `--listen` | `SEEDMIRAGE_LISTEN` | Address of the interface (default `127.0.0.1:7070`). Any other interface prints a warning. |
| `--open=false` | `SEEDMIRAGE_OPEN` | Do not open the browser. |
| `--allowed-host` | `SEEDMIRAGE_ALLOWED_HOSTS` | Extra `Host` header accepted, besides `localhost`, `127.0.0.1` and `::1` (protection against DNS rebinding). Repeatable or comma-separated. |
| `--no-auth` | `SEEDMIRAGE_NO_AUTH` | Disable the access token. Only for a throwaway local test. |
| `--metrics` | `SEEDMIRAGE_METRICS` | Expose `GET /metrics` (Prometheus text format, behind the access token). |
| `--no-update-check` | `SEEDMIRAGE_NO_UPDATE_CHECK` | Never look for a newer release (see [Update notice](usage.md#update-notice)). |
| `--log-level` | `SEEDMIRAGE_LOG_LEVEL` | `debug`, `info` (default), `warn`, `error`. |
| `--log-format` | `SEEDMIRAGE_LOG_FORMAT` | `text` (default) or `json`. |

`SEEDMIRAGE_UI_TOKEN` (environment only) fixes the access token instead of reading `ui-token`; it must be at least 32
characters among letters, digits and `-_.~`.

Only one instance can use a data folder at a time: a second `seedmirage serve` on the same folder opens the running
instance and exits.

Stop seedmirage with Ctrl-C (or `SIGTERM`): it sends a `stopped` announce for every torrent before exiting
(10 seconds at most).

## Docker

Requirements: Docker with the Compose plugin. `docker compose up -d --build` builds the image from the sources; releases are
also published as `ghcr.io/houtmann/seedmirage:<version>` and `:latest` (`linux/amd64` and `linux/arm64`).

```bash
git clone https://github.com/Houtmann/seedmirage.git
cd seedmirage
mkdir -p data                                   # create it yourself, otherwise Docker creates it as root
docker compose up -d --build
docker compose logs seedmirage | grep token     # the access link
```

Open the link `http://127.0.0.1:7070/?token=...` from the logs. Your `.torrent` files go in `./data/torrents/` or
through the web interface. Stop with `docker compose down` (it waits up to 15 seconds so that the `stopped`
announces can leave).

The container runs as `1000:1000` by default. If your user has other ids, copy `.env.example` to `.env` and set
`SEEDMIRAGE_UID` / `SEEDMIRAGE_GID` (`id -u`, `id -g`) before the first start. All variables, the healthcheck and how to reach
a tracker running on the host are in [`docker.md`](docker.md). `make up` and `make down` are shortcuts for the two compose
commands.

## Releases

Versions follow semver and are tagged `vX.Y.Z`. Each [GitHub release](https://github.com/Houtmann/seedmirage/releases)
contains the archives for macOS (arm64, amd64), Linux (amd64, arm64) and Windows (amd64) and a `SHA256SUMS` file.
The container image is `ghcr.io/houtmann/seedmirage:X.Y.Z` (and `:latest`). Pre-releases (a version with a suffix, such as
`0.1.0-rc1`) are marked as pre-release and never move `latest`.

Download the archive and `SHA256SUMS` into the same folder, then verify the archive:

```bash
sha256sum -c SHA256SUMS --ignore-missing      # shasum -a 256 -c ... on macOS
```

## Where the data lives

By default the data folder is `seedmirage` inside your user configuration directory:

| System | Default folder |
|---|---|
| macOS | `~/Library/Application Support/seedmirage` |
| Linux | `~/.config/seedmirage` (or `$XDG_CONFIG_HOME/seedmirage`) |
| Windows | `%AppData%\seedmirage` |
| Docker | `./data` next to `compose.yaml` (mounted as `/data`) |

Use `--data-dir` (or `SEEDMIRAGE_DATA_DIR`) to choose another one. Layout:

| Path | Content |
|---|---|
| `config.json` | Settings (edited from the interface, applied without a restart). Created with defaults. |
| `torrents/` | Your `.torrent` files. |
| `torrents/archived/` | Archived torrents (stopped, kept on disk, restorable from the interface). |
| `clients/` | Client profiles, copied from the embedded ones at startup. A profile you modified is never overwritten. |
| `state.json` | Cumulated upload per torrent, hourly history, archived torrents. Written once there is something to record. |
| `ui-token` | The access token of the interface (keep it private). |
| `.lock` | Prevents two instances from sharing the folder. |

Back up the folder by copying it while seedmirage is stopped.

## Upgrade

- **Binary from source**: `git pull`, `make build`, restart. The data folder is kept as is.
- **Archive**: replace the executable with the one from the new archive.
- **Docker**: `docker compose pull && docker compose up -d` (published image), or `git pull` then `docker compose up -d --build` (from the sources).

Embedded profiles are refreshed on startup: a profile in `clients/` that you have not modified is updated with the
new binary, a profile you edited is left alone (see [`profiles.md`](profiles.md#installation-in-clients)).
Settings you do not know about in `config.json` are ignored and missing ones take their default value.

## Uninstall

1. Stop seedmirage (Ctrl-C, or `docker compose down`).
2. Delete the executable, or for Docker the clone, and the image: `docker image rm ghcr.io/houtmann/seedmirage:latest`.
3. Delete the data folder if you no longer need your settings and statistics.
