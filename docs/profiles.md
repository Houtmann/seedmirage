# Client profiles (`schema` 1 format)

A profile describes how a BitTorrent client announces: peer_id, key, encoding, query parameters (in order), headers and
`numwant`. The format is specific to seedmirage; it is designed to reproduce the real client's request **byte for byte**.
Code: `internal/emulation`. Embedded profiles: `internal/emulation/profiles/*.json`, copied to `clients/` at startup (see
[Installation in `clients/`](#installation-in-clients)). This page is the complete reference of the format.

The truth about a client comes from its **source code**: each profile cites in `references` the files, tags and lines that were
read, and says in `notes` what remains uncertain or is not reproduced.

## Keys

| Key | Type | Purpose |
|---|---|---|
| `schema` | integer | Always `1`. |
| `name` | text | `<family>-<version>`, must match the file name. |
| `family`, `version` | text | For example `qbittorrent`, `5.3.0`. |
| `released` | `YYYY-MM-DD` | Release date of the version (if needed that of the tagged commit, stated in `notes`). |
| `source` | `manual` \| `capture` | Origin of the profile. |
| `notes` | text | Differences, approximations, uncertainties. |
| `references` | list of `{url, note}` | Source files that were read: permanent link (tag or commit, `#Lx-Ly` range) and what it justifies. Mandatory, `https://`. |
| `peerId` | object | See below. |
| `key` | object, optional | See below. Absent: the client does not send a key. |
| `encoding` | object | `keep`: bytes left unencoded besides `[0-9A-Za-z]` (printable ASCII, without `%`); `hex`: `upper` or `lower`. A `query` parameter can carry its own `encoding` (see "query"). |
| `query` | ordered list | Parameters `{name, value, omit?}`. |
| `headers` | ordered list | Headers `{name, value}` with a fixed value, in sending order. `Host` is added by the HTTP client, first; to place it elsewhere (BiglyBT/JDK), the entry `{"name": "Host", "value": "{host}"}` sets its position (the only value allowed). |
| `udp` | object, optional | `urlData` (boolean, true by default): the client attaches the BEP 41 "URLData" option (path and query of the tracker URL) to its UDP announces. libtorrent (qBittorrent, Deluge) sends it, Transmission does not: confirmed by capture. |
| `numwant` | object | `none`, `started`, `completed`, `stopped`: the four keys are mandatory (integers >= -1; -1 = tracker default: the client does not send `numwant` over HTTP and sends -1 over UDP, like rTorrent). |

Unknown fields are rejected (`Parse`): a profile is a contract.

### `peerId`

`prefix` (text) or `prefixHex` (hexadecimal, for non-printable bytes) + `suffix` + optional checksum byte.
`suffix` is a `regexgen` pattern (a subset of the `regexp/syntax` syntax, bytes 0x00-0xFF) of **fixed** length.
The total is exactly **20 bytes**: `Validate` checks it over 64 draws, and every generation checks it again.

`checksum` (optional) names a transformation applied to the suffix:

| Name | Effect | Client |
|---|---|---|
| `mod36` | Appends a final byte drawn from `0-9a-z` such that the sum of the indices (in `0-9a-z`) of the suffix and of that byte is zero modulo 36; the suffix must be within `0-9a-z`. | Transmission 4.1.4 (`tr_peerIdInit`, `session.cc`) |

### `key`

`pattern`: a `regexgen` pattern (for example `[0-9A-F]{8}` = 32 bits in uppercase hexadecimal). The UDP client (BEP 15) can reuse `Announce.Key`.

### Renewal scopes (`renewal.scope`)

For the **peer_id** only and the `torrent` scope, `renewal` additionally accepts `ttlHours` (the peer_id is renewed at the first
announce after it is `ttlHours` hours old) and `ttlPublicOnly` (private torrents are never renewed by age). Transmission 3.00:
`{"scope": "torrent", "ttlHours": 6, "ttlPublicOnly": true}`.

| Scope | Behavior | Clients that use it |
|---|---|---|
| `torrent` | One value per torrent, stable as long as the torrent state is kept: the life of the process, or until eviction after `IdleTTL` (24 h by default) without an announce. A repeated `started` does not renew it (a real client keeps the torrent object across pause and resume). | qBittorrent 4.6.7 and 5.3.0 (peer_id: `torrent.cpp`; key: `tracker_key()`), Deluge 2.2.0, Transmission 4.1.4 and 3.00 (peer_id), rTorrent 0.16.28 (peer_id and key) |
| `process` | One value for the whole process, shared by all torrents. | Transmission 3.00 (key: `tr_announcer`); available for captures. |
| `session` | One value per torrent, renewed at every `started` announce. | No client studied so far; available for captures. |

Every value is drawn with the injected `*rand.Rand` source; state is evicted through the injected `clock.Clock`.

### `query`

Each parameter is emitted in list order, as `name=value`, separated by `&`. A parameter can have its own `encoding` (same shape as
the profile's) which replaces the profile's for its variables: Transmission 3.00 encodes the info_hash in lowercase hexadecimal
and `ipv6` in uppercase. The result (`Announce.RawQuery`, without `?`) is never "cleaned up": a parameter is either emitted or
omitted according to explicit rules.

`value` is a template: literal text (no braces) and `{name}` variables, encoded according to `encoding`. `{name:raw}` inserts the
value as is (the real client does not encode it; only current use: Transmission's `trackerid` and `ip`).
`{info_hash:raw}` is forbidden.

| Variable | Value | Required omission |
|---|---|---|
| `{info_hash}` | 20 bytes, encoded byte by byte | |
| `{peer_id}` | the 20 bytes of the peer_id | |
| `{port}`, `{uploaded}`, `{downloaded}`, `{left}`, `{numwant}` | decimal | |
| `{event}` | `started`, `stopped` or `completed` | `no-event` |
| `{key}` | the torrent's key | `no-key` if the profile has no `key` section |
| `{ip}` | `PublicIP` | `no-ip` |
| `{ipv4}` / `{ipv6}` | `PublicIP` if IPv4 / IPv6 (never an IPv4-mapped IPv6) | `no-ipv4` / `no-ipv6` |
| `{tracker_id}` | `trackerid` received from the tracker | `no-tracker-id` |

`Validate` rejects a parameter that uses a variable without the matching omission.

`omit` rules (the parameter is removed if **any** condition is true):

| Rule | True when |
|---|---|
| `no-event` | regular announce (no event) |
| `no-ip` | `PublicIP` unknown |
| `no-ipv4` / `no-ipv6` | `PublicIP` missing or of another family |
| `no-key` | the profile has no `key` |
| `no-tracker-id` | no trackerid known |
| `public-torrent` | the torrent does not have the `private` flag |

### `headers`

Fixed values, in the profile's order. The HTTP client writes the request by hand and puts `Host` first, unless `headers` has a
`Host` entry `{host}`: it then writes it at that position, with the host of the announce URL. It must decode the codings advertised
in `Accept-Encoding` (Transmission profile: `deflate, gzip`).

Known difference (v1): Transmission sends an IPv4 announce then an IPv6 one when the host has both; seedmirage sends
**one announce per tracker**, on the address the system resolves. The tracker therefore does not see the second announce.
The HTTP client (`internal/tracker`) adds no header (neither `Accept-Encoding` nor Go's `User-Agent`), joins the query to the one
of the announce URL with `&` (otherwise `?`) and omits the port from `Host` when it is the scheme's default.

## Embedded profiles

Sorted by family then by semantic version (`CompareVersions`: `4.10.0` after `4.9.0`, `3.00` = `3.0`).
`emulation.LatestEmbedded("qbittorrent")` gives the default profile of the configuration (`config.Defaults`).
Each profile cites the client's source code (tag, file, lines) in `references`; what could not be established is stated in `notes`.

| Family | Version | Release date | Source code cited |
|---|---|---|---|
| qbittorrent | 5.3.0 | 2026-10-19 | qBittorrent `release-5.3.0` + libtorrent `v2.0.15` |
| qbittorrent | 4.6.7 | 2024-09-16 | qBittorrent `release-4.6.7` + libtorrent `v1.2.20` (announce identical to 2.0.15) |
| transmission | 4.1.4 | 2026-10-22 | Transmission `4.1.4` |
| transmission | 3.00 | 2020-05-22 | Transmission `3.00` |
| deluge | 2.2.0 | 2025-04-28 | Deluge `deluge-2.2.0` + libtorrent `v2.0.15` (User-Agent: `lt.__version__` = 2.0.15.0 assumed) |
| rtorrent | 0.16.28 | 2026-11-11 | libtorrent (rakshasa) `v0.16.28` + rTorrent `v0.16.28` |

### Verification by capture

The 6 profiles were compared, on 2026-11-19, with **real clients** running in Docker (`deploy/capture/docker-compose.yml`,
driven by `scripts/capture/<client>.sh`, procedure in [`capture.md`](capture.md): `started`, regular announce, pause and resume of
one torrent, client restart, `stopped`, over 4 HTTP torrents including a private one and 1 UDP torrent). The captures, reduced to
the announces and anonymized (public IP replaced by `192.0.2.10`), are in `internal/emulation/testdata/captures/<profile>/`;
`TestCapturedFixtures` (`internal/capture/fixtures_test.go`) replays them offline against the embedded profile with `capture.Diff`
(HTTP) and `capture.DiffUDP` (UDP BEP 15). `known-diffs.txt` lists, per capture, the only tolerated differences and their reason.

| Profile | Captured client (Docker image) | Result |
|---|---|---|
| qbittorrent-5.3.0 | qBittorrent 5.3.0, libtorrent 2.1.2 (`lscr.io/linuxserver/qbittorrent:5.3.0`) | identical, HTTP and UDP |
| qbittorrent-4.6.7 | qBittorrent 4.6.7, libtorrent 1.2.19 (`lscr.io/linuxserver/qbittorrent:4.6.7-libtorrentv1`) | identical, HTTP and UDP |
| transmission-4.1.4 | Transmission 4.1.4 (`lscr.io/linuxserver/transmission:4.1.4`) | **fixed**: `ip` removed (only `ipv4`/`ipv6`), UDP without the BEP 41 option, UDP `num_want` 0 for `stopped`; only remaining difference: `Accept-Encoding` |
| transmission-3.00 | Transmission 3.00 (`lscr.io/linuxserver/transmission:version-3.00-r8`) | identical (UDP without the BEP 41 option); only difference: `Accept-Encoding` |
| deluge-2.2.0 | Deluge 2.2.0 (`lscr.io/linuxserver/deluge:2.2.0`) | identical; only difference: libtorrent version in the `User-Agent` (2.0.13.0 in the image) |
| rtorrent-0.16.28 | rTorrent **0.16.25** (`crazymax/rtorrent-rutorrent:5.3.18-0.16.25`, no 0.16.28 image) | identical apart from versions (`User-Agent`, peer_id prefix `-lt1019-`, justified by the source code); UDP without the BEP 41 option |

Difference shared by Transmission and rTorrent: the `Accept-Encoding` value comes from the libcurl compiled into the client
(`deflate, gzip, br, zstd` with the Alpine images'). The profiles keep `deflate, gzip` because seedmirage's HTTP client does not
decode `br` or `zstd`; a real client on another platform (official macOS/Windows binaries) may advertise something else, which
only a capture of that client could settle.

`udp.urlData` (see "Keys"): libtorrent (qBittorrent, Deluge) sends the BEP 41 `URLData` option in its UDP announces;
Transmission and rTorrent do not.

**Scrape** (observed): the captured clients scrape over HTTP encoding the `info_hash` **like their announce** (qBittorrent and Deluge
in lowercase hexadecimal, Transmission 4.1.4 and rTorrent in uppercase, Transmission 3.00 in lowercase); Transmission 4.1.4 sends
several `info_hash` parameters in a single `/scrape` request.

### Families without a profile

- **µTorrent / BitTorrent**: closed-source Windows (and macOS) clients, with no Linux image and no source code to read. A profile
  requires a **human capture** with `seedmirage capture --listen <LAN address>` (see "Client on the host" in
  [`capture.md`](capture.md)). Not provided.
- **BiglyBT**: not provided, for lack of a way to make it faithful byte for byte. The format can place `Host` (the `Host`
  entry `{host}` of `headers`, honored by `internal/tracker` and `capture`), but the real request depends on (a) the JDK bundled by
  the installer (`HttpURLConnection`: order of `User-Agent`, `Connection`, `Accept-Encoding`, `Host`, `Accept`, and a default
  `Accept` value that varies with the version), (b) a local BiglyBT HTTP filter (`ClientIDManagerImpl`, v4.2.0.0) which, when
  active, rewrites `Host` as an explicit `host:port` even for port 80, and (c) the optional `User-Agent` suffix
  (`;system;Java x`). There is no reliable Docker image (community images bundle a VNC or a VPN) and no capture: a `manual`
  profile would have too low a confidence for the byte-for-byte guarantee. To do: capture a real BiglyBT 4.2.0.0, then write the
  profile with `Host` placed according to the capture. The latest published version is BiglyBT 4.2.0.0 (2026-11-19).

### Installation in `clients/`

At startup, each embedded profile is copied to `clients/<name>.json`:

- absent: copied;
- present and identical to the embedded one: nothing to do;
- present, identical to what had been installed (SHA-256 hash in `clients/.embedded.json`) but different from the current embedded one: updated with the binary;
- present and modified by the user (or a file that was not installed by seedmirage): **never overwritten**.

Writes are atomic (temporary file then rename). A profile removed from the embedded ones leaves the manifest but its file stays.

## Go API

```go
p, _ := emulation.LoadEmbedded("qbittorrent-5.3.0") // or emulation.Parse(data)
e, _ := emulation.New(p, clk, rng, emulation.Options{})
a, _ := e.BuildAnnounce(emulation.AnnounceParams{
    InfoHash: ih, Port: 51413, Uploaded: u, Downloaded: d, Left: 0,
    Event: emulation.EventStarted, PublicIP: ip, Private: priv, TrackerID: tid,
})
// a.RawQuery, a.Headers, a.PeerID, a.Key, a.Numwant
```

`Emulator` is safe for concurrent use. `Forget(infoHash)` forgets a deleted or archived torrent.

## Annotated example (excerpt of `transmission-4.1.4.json`)

```jsonc
{
  "schema": 1,
  "name": "transmission-4.1.4",
  "peerId": {
    "prefix": "-TR4140-",              // -TR + base62(4,1,4) + 0 (stable) + -
    "suffix": "[0-9a-z]{11}",          // 11 random characters...
    "checksum": "mod36",               // ...+ 1 checksum character = 20 bytes
    "renewal": {"scope": "torrent"}
  },
  "key": {"pattern": "[0-9A-F]{8}", "renewal": {"scope": "torrent"}},
  "encoding": {"keep": "-_.~", "hex": "upper"},
  "query": [
    {"name": "info_hash", "value": "{info_hash}"},
    {"name": "numwant",   "value": "{numwant}"},
    {"name": "compact",   "value": "1"},                              // fixed value
    {"name": "event",     "value": "{event}", "omit": ["no-event"]}, // absent from a regular announce
    {"name": "ipv4",      "value": "{ipv4}", "omit": ["no-ipv4"]}   // absent if PublicIP is unknown or IPv6
  ],
  "headers": [{"name": "User-Agent", "value": "Transmission/4.1.4"}],
  "numwant": {"none": 80, "started": 80, "completed": 80, "stopped": 0}
}
```

## Linter (`seedmirage check-profile`)

```bash
seedmirage check-profile internal/emulation/profiles      # a folder (*.json) or files
```

Prints one finding per line (`<file>: <level> <code>: <message>`) or `ok`. Exit code **1** if there is at least one error, 0
otherwise (warnings do not fail). Code: `emulation.Lint` / `emulation.LintData`.

| Code | Level | Rule |
|---|---|---|
| `parse` | error | Unreadable JSON or unknown field. |
| `invalid` | error | `Validate` failed (the rules of this document). |
| `host-header` | error | `Host` header other than the `{host}` position entry. |
| `duplicate-param` | error | Duplicate query parameter. |
| `ua-version` | error | The `User-Agent` does not contain the profile's `version`. |
| `peerid-prefix` | error | peer_id prefix inconsistent with the version (qBittorrent `-qB5300-`, Deluge `-DE220s-`, Transmission `-TR4140-`; rTorrent is not checked: its prefix is libtorrent's; Transmission 4.x beyond one digit per component: rule skipped). |
| `no-user-agent` | warn | No `User-Agent`. |
| `numwant-atypical` | warn | `numwant` differs from the family's usual value (200 qBittorrent/Deluge, 80 Transmission, -1 rTorrent; 0 on stop except rTorrent). |
| `accept-encoding` | warn | `Accept-Encoding` missing, or different from the family's, or advertised when the family does not send one. |
| `accept-decode` | warn | Advertised coding that the HTTP client does not decode (other than `gzip`, `deflate`, `identity`). |
| `outdated` | warn | At least `MaxVersionsBehind` (2) newer embedded profiles in the same family. |

Unknown families (captures of clients that are not covered) only get the generic rules.

## Outdated versions (`seedmirage profiles outdated --online`)

Queries the public release APIs (GitHub: `qbittorrent/qBittorrent`, `transmission/transmission`, `BiglySoftware/BiglyBT`,
`rakshasa/rtorrent`; `deluge-X.Y.Z` tags of `deluge-torrent/deluge`) and lists the families whose latest stable version is newer
than the most recent embedded profile (or that have no profile). Network access **only** with `--online`; no token,
`User-Agent: seedmirage-release-check`, results cached for 1 hour in the process. Code: `internal/releases`
(`New(Config{...}).Check(ctx, embedded)`, base URL and `http.Client` injectable). The same check is available from the
*Profiles* page of the interface.

Options: `--format text|json` (default `text`; `json` prints a list of `{family, embedded, latest, url}`, `[]` when up to date).
Exit codes: `0` up to date, `1` error, `2` usage, `3` at least one family is behind.

### Weekly watch

The `Client versions` workflow (`client-versions.yml`, every Monday and on demand) runs
`profiles outdated --online --format json` and, for each family that is behind, opens **one** issue titled
`Client update: <family> <version>` with the `profiles` label (created if missing). It first looks for an existing issue
with that exact title, open **or closed**, so a version is never reported twice. The issue links to the client's release page
and lists the update procedure (capture, profile, golden files, release; see below). It only uses the workflow's
own token (`contents: read`, `issues: write`). Profile generation stays manual.

## Adding a profile

1. **Capture**: `seedmirage capture` against the real client, or read its source code (an exact tag) and note in `references` every file and line range read.
2. **Write** `internal/emulation/profiles/<family>-<version>.json` (`source`: `capture` or `manual`, differences in `notes`).
3. **Lint**: `seedmirage check-profile internal/emulation/profiles` must report no error; justify every warning in `notes`.
4. **Golden**: `go test ./internal/emulation/ -update`, review the files of `testdata/golden/` against the cited source code, then `go test ./internal/emulation/...`.
5. **Review**: `make check`, then a pull request (one profile per PR, with the "Embedded profiles" table of this document up to date).

To use a profile without rebuilding seedmirage, import it from the *Profiles* page of the interface.

## Golden tests

`internal/emulation/testdata/golden/<profile>-<event>.txt` holds the query, headers, peer_id (hex), key and `numwant` for
`started`, `regular`, `completed`, `stopped` (fixed seed and clock), plus `started-ip` (address, trackerid, private torrent).
Regeneration: `go test ./internal/emulation/ -update`, then review against the source code cited in `references`.
