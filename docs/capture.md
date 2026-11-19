# Capture mode: building a profile from the real client

`seedmirage capture` starts a local tracker (HTTP **and** UDP, same port) that records what a real BitTorrent client
sends, then infers a profile in the [`schema: 1`](profiles.md) format (`source: capture`). This is how you follow a new client version
without waiting: the truth comes from what the client actually sends.

Code: `internal/capture` (recording, inference, `Diff`) and `cmd/seedmirage/capture.go`.

## Step by step

1. **Start the tracker** (loopback by default):

   ```bash
   seedmirage capture --listen 127.0.0.1:6969 --out ./capture --torrents 4
   ```

   It writes into `./capture`: the fake `.torrent` files (`capture-NN-http.torrent`, `capture-NN-udp.torrent`),
   `manifest.json` (generated torrents, including the `private` flag), then, as requests come in, `raw.jsonl`. A folder that
   was already used is refused: nothing is overwritten.
   The torrents carry no data (size 90 MiB to 4 GiB, varied): the client announces `left > 0`, `downloaded = 0`.
   The `info_hash` values are picked among 20,000 candidates so that they contain the hard-to-encode bytes (0x00, 0x20, 0x80, 0xFF,
   `- _ . ~ ! * ( ) ' % +`): this is what makes it possible to infer which bytes the client leaves raw and which it encodes.
   The last HTTP torrent is private (some clients only send `ipv4`/`ipv6` for those).
   `--udp-torrents` (default 1) adds torrents announced **only** over UDP: a client generally announces to one tracker per tier
   only, so distinct torrents are needed to see both protocols.

2. **In the real client**, in this order:
   1. add all the `.torrent` files, without downloading any file or running a recheck; let at least two announces go by per
      torrent (the returned interval is 60 s, the peer lists are empty);
   2. pause **one** torrent only, wait 10 s, resume it: without this step, the `torrent` and `session` renewal scopes cannot
      be told apart;
   3. quit the client, wait 10 s, start it again: all the torrents restart together (this tells the `process` scope apart and
      checks what survives a restart); wait for a regular announce;
   4. remove the torrents from the client (`stopped` announce).

3. **Ctrl-C** in `seedmirage capture`: the tracker stops, then `capture/<family>-<version>.json` and
   `capture/report.md` are written.

4. **Read `report.md`**. It lists the **uncertainties** first (confidence `low`, then `medium`), then the safe deductions.
   Typically: a partially observed suffix alphabet, info_hash bytes never seen, `completed` event not observed
   (the `numwant` value is then copied), `session` scope not ruled out. Fix the profile by hand if needed, add
   the source code references (`references`, `--reference`), complete `notes`.

5. **Validate**: `seedmirage check-profile capture/<profile>.json` (no error), then the steps of
   ["Adding a profile"](profiles.md#adding-a-profile) (golden, review).

`seedmirage capture --infer-only --out ./capture` re-runs the analysis of an existing `raw.jsonl` (for instance one received
from another machine) without starting a tracker; the derived files are rewritten.

## Docker client

`deploy/capture/docker-compose.yml` provides qBittorrent, Transmission, Deluge and rTorrent (one compose profile per client,
pinned versions). The tracker runs on the host and the containers reach it through `host.docker.internal`:

```bash
seedmirage capture --listen 172.17.0.1:6969 --announce-host host.docker.internal:6969 --out ./capture
docker compose -f deploy/capture/docker-compose.yml --profile qbittorrent up -d
```

`172.17.0.1` is the usual address of the Docker bridge; `--listen 0.0.0.0:6969` also works but exposes the tracker on all
interfaces. Any non-loopback address prints a **warning**: any machine that can reach it can send announces (the raw requests
are recorded in the output folder). Stop the capture afterwards.

The tags in the compose file were checked on 2026-11-19 (see "Verification by capture" in [`profiles.md`](profiles.md)):
qBittorrent 5.3.0, Transmission 4.1.4 (and 3.00: `TRANSMISSION_TAG=version-3.00-r8`), Deluge 2.2.0, rTorrent 0.16.25
(community image `crazymax/rtorrent-rutorrent`, no 0.16.28 published). Check the version that is actually running (container log,
web interface) and that the resulting profile carries that version. On Docker Desktop/OrbStack for macOS,
`--listen 127.0.0.1:6969 --announce-host host.docker.internal:6969` is enough (TCP and UDP): there is no need to expose the tracker.

### Driving scripts

`scripts/capture/<client>.sh <capture folder>` (qbittorrent, transmission, deluge, rtorrent) runs the procedure above through the
client's local API (WebUI, RPC, JSON-RPC): adding, regular announce, pause/resume of one torrent, container stop/restart,
removal. The `INTERVAL` variable is the wait for a regular announce (60 s by default; **300 s** for qBittorrent and Deluge:
libtorrent caps the announce interval at 5 minutes, so the capture tracker's 60 s interval is ignored). Several clients can be captured
in parallel with trackers on different ports.
`go run ./scripts/capture/reduce <capture> <output>` reduces a capture to its announces and replaces the announced IP addresses
with documentation addresses (192.0.2.10): this is how the fixtures in
`internal/emulation/testdata/captures/<profile>/` are produced; `TestCapturedFixtures` replays them without a network.

## Verifying the embedded profiles: `make capture-verify`

One command checks, for each open client (qBittorrent, Transmission, Deluge, rTorrent), that the request of the **real** client is
identical, byte for byte, to the one of the embedded profile. It needs Docker and takes 10 to 15 minutes (libtorrent announces every
5 minutes at best), so it is **not** part of `make check` nor of the CI.

```bash
make capture-verify                              # the four clients, in parallel
make capture-verify CLIENTS=qbittorrent,deluge   # a subset
QBITTORRENT_VERSION=4.6.7 make capture-verify CLIENTS=qbittorrent
UPDATE_PROFILE=1 make capture-verify CLIENTS=transmission
```

For each client, `scripts/capture/verify.sh` starts a capture tracker (loopback, one port per client), starts the container from a clean
state, drives it with `scripts/capture/<client>.sh`, stops the tracker, then runs `seedmirage capture --compare <profile>`.
Everything lands in `capture/verify/<client>/` (raw capture, report, tracker log; the folder is recreated at each run).

Variables: `<CLIENT>_VERSION` is the version of the embedded profile to verify (defaults: qbittorrent 5.3.0, transmission 4.1.4,
deluge 2.2.0, rtorrent 0.16.28) and also gives the Docker tag; `<CLIENT>_PROFILE` overrides the profile (name or file);
`QBITTORRENT_TAG`, `TRANSMISSION_TAG`, `DELUGE_TAG`, `RTORRENT_TAG` force an image tag (there is no rTorrent 0.16.28 image: the default
tag is 0.16.25, so its user agent and peer_id prefix are expected to differ).

### `seedmirage capture --compare`

```bash
seedmirage capture --compare qbittorrent-5.3.0 --out ./capture          # embedded profile, existing capture folder
seedmirage capture --compare ./my-profile.json --out ./capture
seedmirage capture --compare qbittorrent-5.3.0 --update-profile --out ./capture
```

No tracker is started: the `raw.jsonl` of `--out` is replayed against the profile. The report lists, for each field that differs, how
many announces differ, on which events, what the profile produces and what the client sent:

| Field | Meaning |
|---|---|
| `query.order` | order (or set) of the query parameters |
| `query.<param>` | value or encoding of a parameter (hex case, escaped characters, `numwant` of an event...) |
| `query.peer_id`, `query.key` | shape only (prefix, alphabet, checksum, encoding): the random part is never compared by value |
| `headers.order`, `headers.<name>`, `headers.host` | order, case, value of the headers; position of `Host` |
| `udp.*` | BEP 15 announce: peer_id and key shape, IP, `num_want`, event, BEP 41 options |

The exit status is 0 when everything is identical up to the randomness, 1 otherwise (or when the capture has no announce). The report
also says what the capture did not cover (no UDP announce, no `stopped`...): an event that was not captured is not verified.
A difference is information, not an instruction: decide whether the profile or the capture setup is wrong (see "Known limits"
above: a client library that differs per installation, a container older than the profile).

`--update-profile` (with `--compare`) also writes the profile inferred from the capture as `<name>.candidate.json`: next to the profile
file if `--compare` names a file, in `--out` for an embedded profile. An existing candidate is never overwritten and an embedded
profile is never modified: review the candidate, then adopt it by hand (steps of ["Adding a profile"](profiles.md#adding-a-profile)).

In Go: `capture.Compare(profile, records, manifest, capture.DiffOptions{})` returns a `Comparison` (differences grouped by field,
`Text()` for the report); it calls `Diff` and `DiffUDP` below.

## Client on the host (µTorrent on Windows, etc.)

1. Run `seedmirage capture --listen <host-LAN-address>:6969 --announce-host <that address>:6969 --out capture`
   (or, if the client runs on the same machine, keep `127.0.0.1`: some clients refuse a loopback tracker;
   in that case use the LAN address and accept the warning).
2. Add the `.torrent` files of `capture/` to the client and follow the steps above.
3. Turn off in the client whatever gets in the way: DHT, PEX, local discovery, the "at least one tracker" limit; a Windows client
   may ask to allow port 6969 through the firewall.
4. For a client that is not on the same machine: copy `capture/` after Ctrl-C and re-run the analysis with `--infer-only`
   if needed.

## What is recorded (`raw.jsonl`)

One JSON line per request:

| Field | Content |
|---|---|
| `time`, `transport` (`http`/`udp`), `remote` | Timestamp (injected clock), protocol, client address. |
| `head` (HTTP) | Request line and **raw** headers, separated by CRLF, in base64: this is the exact reference (original order and case, non-UTF-8 bytes included). |
| `lines` (HTTP) | The same content in clear text, for human reading (may lose non-UTF-8 bytes; not used for the analysis). |
| `packet` (UDP) | Received datagram, in hexadecimal. |

The HTTP request is read from the TCP connection by the fake tracker of `internal/trackertest`, never by `net/http`: no
normalization (header case, `%XX` hexadecimal case, order). `info_hash`, `peer_id` and `key` are decoded byte by byte
(`PercentDecode`), never converted to text.

## What is inferred

| Element | Inference | Confidence |
|---|---|---|
| Parameter order | sequence shared by the requests; missing parameters are inserted after their predecessor | high; low if the order differs between requests |
| Fixed values (`compact=1`, `supportcrypto=1`...) | identical value in all requests | high |
| Variables (`{info_hash}`, `{peer_id}`, `{port}`, `{uploaded}`, `{event}`, `{numwant}`, `{key}`, `{ip}`...) | recognized by the parameter name | high |
| Omission rules | `no-event` if the parameter is missing exactly from regular announces; `public-torrent` if absent from public torrents (from `manifest.json`); `no-ip`, `no-ipv4`, `no-ipv6`, `no-tracker-id` required by the format | medium |
| `encoding` | non-alphanumeric bytes seen raw → `keep`; case of `%XX` → `hex`; a parameter's own encoding (e.g. `info_hash` in lowercase hexadecimal, `peer_id` in uppercase) → the parameter's `encoding` | high to low depending on the number of values; bytes never observed are listed (they are encoded by default) |
| `peerId` | prefix = `-XXnnnn-` fingerprint (8 bytes) or the common part; suffix = class of the observed bytes (an alphanumeric group is taken whole if seen at 75%); `mod36` checksum detected if it holds for every peer_id; non-printable-ASCII bytes → `[\x00-\xff]` | depends on the number of bytes per distinct character |
| `key` | length, alphabet, case | same; **low** when the key is only seen over UDP (32-bit integer: hexadecimal case does not exist) |
| Renewal scopes (peer_id and key) | `process`: all torrents of a same startup wave share the value; `session`: a torrent's value changes when it is resumed alone; `torrent`: it does not change on that resume. A wave = starts less than 30 s apart; an isolated resume = a wave of one torrent | high; medium without an isolated resume in the capture; low if the value changes mid-session |
| `numwant` per event | value observed per event; missing → copied from a neighboring event (low); parameter absent → -1 | high / low |
| `headers` | received order, `Host` only if it is not first (a `{host}` entry), fixed values; a header absent from some requests or with a varying value is reported | high / low |
| Family and version | `User-Agent` (`Name/version`), otherwise peer_id prefix (`-qB5300-` → qbittorrent 5.3.0, low confidence) | high / low |

Known limits: a client that speaks HTTP/1.0 or does not use `GET` is reported (seedmirage's HTTP client does not reproduce it;
a `Host` that is not first is reproduced by the `Host` `{host}` entry of `headers`); the BEP 41 option of UDP announces is inferred
(`udp.urlData`); periodic renewal of a peer_id during a session (`ttlHours`) is reported but not modeled; `released` is
the capture date, not the client's release date; the source code reference is that of the published client version, to be completed
with the files and lines actually read.

The resulting profile is then **replayed** on every captured request (self-check: `selfcheck` finding), and passes
`emulation.Lint`.

## `capture.Diff`

```go
mismatches, err := capture.Diff(profile, rec, capture.DiffOptions{Private: true})
```

Compares a captured HTTP request to the one the profile would produce for the same torrent, event and counters. Random values
are neutralized: `peer_id` and `key` must match the profile's pattern (prefix, alphabet, checksum) and be encoded the way the
profile encodes; everything else (order and value of parameters, order, case and value of headers, position of `Host`,
`HTTP/1.1`) must be identical byte for byte. An empty list means identical up to randomness.

`capture.DiffUDP(profile, rec, capture.DiffOptions{URLData: "/announce"})` does the same for a UDP announce (BEP 15): connection id
and transaction id are neutralized, the peer_id and the key (32-bit integer read as hexadecimal) must follow the profile's pattern;
the action, info_hash, counters, event, IP address (0), `num_want`, port and BEP 41 options are compared byte for byte
(the URLData option is only expected if the profile sends it, `udp.urlData`).

The `TestCapturedFixtures` test replays the real captures of `internal/emulation/testdata/captures/` against the embedded
profiles; justified differences (a value that depends on the HTTP library of the installation) are listed in
`known-diffs.txt` next to the capture, any other difference makes the test fail.
