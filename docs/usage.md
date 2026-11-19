# Usage

This page assumes seedmirage is installed (see [`install.md`](install.md)). Read the usage warning in the
[README](../README.md) before pointing it at a real tracker.

- [First launch](#first-launch)
- [Adding torrents](#adding-torrents)
- [How torrents are seeded](#how-torrents-are-seeded)
- [Choosing the client profile](#choosing-the-client-profile)
- [Settings](#settings)
- [Statistics and status](#statistics-and-status)
- [Clean shutdown](#clean-shutdown)
- [When a tracker returns an error](#when-a-tracker-returns-an-error)
- [Language and theme](#language-and-theme)
- [FAQ](#faq)

## First launch

```bash
seedmirage serve
```

The first launch creates the data folder with a `config.json` holding the default settings, the `clients/` folder with the
embedded profiles and a random access token (`ui-token`). Your browser opens on `http://127.0.0.1:7070/?token=...`
(the same link is printed in the terminal, in the `UI ready` line); this stores a session cookie. With Docker, the link is
in `docker compose logs seedmirage`.

The announced port is drawn once, between 49152 and 65534, and then kept in `config.json`. seedmirage does not listen on it:
it only tells the trackers which port to report.

If the interface answers "unauthorized", open the link with the token again (or read it in `ui-token`).

## Adding torrents

Two ways, equivalent:

- **From the interface**: *Torrents* page, "Add .torrent files" (drag and drop or file picker; 10 MB per file and 50 files per
  request at most). Each file is parsed before being written; an invalid file is refused, a torrent that is already present
  (active, pending or archived) is reported as a duplicate, and nothing is ever overwritten silently.
- **From the folder**: copy `.torrent` files into `torrents/` inside the data folder. seedmirage scans the folder every
  few seconds and picks them up.

The tracker of a torrent is read from the file: HTTP(S) and UDP trackers, with tiers. Magnet links are not supported (see the
[FAQ](#faq)).

The *Torrents* page has three tabs: **Active** (being announced), **Pending** (waiting for a free slot) and **Archived**.
You can filter by name or info_hash, open a torrent to see its trackers, swarm and latest announces, and archive,
restore or delete it. Deleting erases the `.torrent` file.

## How torrents are seeded

For every active torrent, seedmirage sends the announce requests a real client would send: `started`, regular announces every
`interval` (never more often than the tracker's `min interval`, except for `started` and `stopped`), and `stopped`.
The reported `uploaded` never decreases during a session. No data is transferred and no peer is contacted.

- At most **Active torrents** (default 5) are announced at the same time; the others are *pending*.
- With selection **By swarm** (default), seedmirage scrapes the trackers and prefers torrents that have leechers.
  A torrent without leecher uploads nothing.
- A torrent is **archived** (stopped, moved to `torrents/archived/`, restorable) when it reaches the target ratio, when its
  swarm has no leecher and "Keep torrents without leechers" is off, when the tracker keeps failing, when it cannot be
  started, or when you archive it. The reason is shown on the *Archived* tab.
- The total uploaded per torrent is kept in `state.json` between runs.

## Choosing the client profile

The *Profiles* page lists the embedded and your own profiles, newest first. **Use** activates one (it is saved in
`config.json`). Changing the profile restarts the announces: a `stopped` is sent with the old client, then `started`
with the new one. Each profile page shows a preview of the exact `started` request (fictitious torrent, example tracker).

*Import a profile* adds a JSON file in the [profile format](profiles.md) (1 MB at most, checked by the linter, never overwriting
an existing profile). *Check for updates* asks the public release APIs of the clients, on demand only, whether a newer
stable version exists than your most recent profile; it is the only action that contacts anything other than your trackers.

The default is the most recent embedded qBittorrent profile.

## Settings

The *Settings* page applies changes immediately and saves them to `config.json`. Rates are in KiB/s. You can also edit
`config.json` by hand while seedmirage is stopped; durations accept `"6h"` or `"90m"`.

### Speed

| Setting (`config.json` key) | Meaning | Default |
|---|---|---|
| Minimum rate (`uploadRateMin`) | Lower bound of the global announced upload speed. | 30 |
| Maximum rate (`uploadRateMax`) | Upper bound; at least the minimum. | 170 |
| Speed model (`speedModel`) | `smooth`: continuous variation with per-torrent noise. `uniform`: random draw between the bounds. | smooth |

### Torrents

| Setting | Meaning | Default |
|---|---|---|
| Active torrents (`maxActiveTorrents`) | Maximum number of torrents announced at the same time. Lowering it stops the most recent ones. | 5 |
| Torrent selection (`selection`) | `swarm`: favour torrents that have leechers (scrape). `random`: simple draw. | swarm |
| Keep torrents without leechers (`keepIdleTorrents`) | Off: a torrent without leechers is archived. | off |
| Rotation of idle torrents (`rotateIdleAfter`) | With "keep" on and selection by swarm: after this long without leechers, a torrent is replaced by a waiting one that has leechers. `0` = never. | 6h |
| Target ratio (`ratioTarget`) | A torrent that reaches this ratio is stopped and archived. `0` = disabled. | 0 |
| Ratio computed on (`ratioScope`) | `session`: the current session. `total`: the cumulative upload of all runs. | session |
| Start on launch (`autoStart`) | Start seeding as soon as `serve` starts (applies at the next launch). Otherwise press **Start** on the dashboard. | on |

### Emulated client

| Setting | Meaning | Default |
|---|---|---|
| Profile (`profile`) | Client family and version to imitate. Changing it restarts the announces. | most recent qBittorrent |

### Network

| Setting | Meaning | Default |
|---|---|---|
| Announced port (`port`) | Port reported to trackers, 1 to 65535; `0` keeps the current (drawn once at first launch). Changing it restarts the announces. | random, 49152-65534 |
| Announced IP (`announceIP`) | Empty: not sent (the tracker already sees your IP). Changing it restarts the announces. | empty |

### Realism

| Setting | Meaning | Default |
|---|---|---|
| Cap per leecher, KiB/s (`caps.perLeecherMax`) | Maximum speed reported for one leecher. `0` = no cap. | 500 |
| Declared uplink, KiB/s (`caps.lineMax`) | Cap on the global speed, as if it were your line speed. `0` = unlimited. | 0 |
| Maximum ratio per 24 h (`caps.dailyRatioMax`) | Upload over 24 hours stays under this ratio times the torrent size. `0` = disabled. | 0 |
| Maximum swarm share (`caps.swarmShare`) | Share of the swarm's demand one torrent can absorb, in ]0 ; 1]. | 0.5 |
| Bound upload by swarm demand (`caps.swarmBudget`) | Over 24 hours a torrent uploads at most your share of what its swarm could have downloaded: `share × (completions × size + leechers × size × 0.5)`, where `share = min(1/(seeders+1), swarmShare)`. Completions are only counted when the tracker reports them. The speed fades towards 0 as the budget is used up. | on |
| Simulate a download (`simulateDownload`) | A torrent seen for the first time first "downloads", then seeds. Applies to torrents added afterwards. | off |
| Minimum / maximum download, KiB/s (`minDownloadRate`, `maxDownloadRate`) | Bounds of the simulated download speed. | 100 / 2000 |

### Hourly schedule (`schedule`)

| Setting | Meaning | Default |
|---|---|---|
| Enable the schedule (`schedule.enabled`) | Off: constant speed and no pauses. | off |
| Time zone (`schedule.timezone`) | IANA name such as `Europe/Paris`; empty: the machine's time zone (the `TZ` variable in Docker). | empty |
| Multiplier per hour (`schedule.hourly`) | 24 values; multiplies the model speed (0 = silence, 1 = unchanged, 2 = double). Reached on the hour, interpolated between two hours. Presets: Constant, Office, Evening. | 1 all day |
| Pauses (`schedule.pauses`) | One per line: day (`mon` to `sun`) then `HH:MM-HH:MM`, for example `mon 22:00-23:30`. A range that crosses midnight (`fri 23:00-02:00`) ends the next day. Torrents are stopped cleanly during the pause, then resumed. | none |

### Advanced

| Setting | Meaning | Default |
|---|---|---|
| Tolerated tracker failures (`maxTrackerFailures`) | Consecutive failures after which a torrent is dropped. Only applies to torrents started afterwards. | 5 |
| Check for updates (`updateCheck`) | Look for a newer release once a day (see [Update notice](#update-notice)). Applies at once. | on |
| Install updates automatically (`autoUpdate`) | Install a new release by itself at the next check (see [Installing an update](#installing-an-update)). Applies at once. | off |

## Statistics and status

- **Dashboard**: running state with Start / Stop, active profile, global speed, uploaded (session and total), and the table of
  active torrents (swarm, speed, ratio, next announce, last announce).
- **Statistics**: upload per hour over 24 hours, 7 days or 30 days, global speed over the last 10 minutes, global ratio, the ten
  torrents with the most cumulative upload, and announces and failures per tracker host (since startup).
- **Status**: version, uptime, data folder, active profile, memory and goroutines, latest warnings and errors. It also tells
  whether `GET /metrics` is enabled, and the state of the update check (last check, error if any).

All pages update live over a single event stream; no refresh is needed.

## Update notice

seedmirage tells you when a newer stable release exists; the check itself downloads nothing (installing is a separate action, see [below](#installing-an-update)). About a minute after
startup, then every 24 hours, it makes **one** request, the only one that is not a tracker announce:

```
GET https://api.github.com/repos/Houtmann/seedmirage/releases/latest
User-Agent: seedmirage/<your version>
```

Nothing else is sent: no identifier, token or setting (only `Accept` and, to save the API quota, `If-None-Match`). The answer
is read with a 1 MiB limit; pre-releases are ignored. A failure is only logged at `debug` level and retried at the next slot.
Development builds (`dev`, `-dirty`) never check.

When a newer version exists, a notice with its release notes appears at the top of every page; **Hide** hides it for that
version only (a cookie). Outside a container it links to the release page; in a Docker container it shows
`docker compose pull && docker compose up -d` instead.

To turn it off: untick **Check for updates** in *Settings* (`"updateCheck": false` in `config.json`, applied at once), or start
with `--no-update-check` (`SEEDMIRAGE_NO_UPDATE_CHECK=1`) for one run.

### Installing an update

With the native binary, the notice has an **Update and restart** button. It:

1. downloads `SHA256SUMS` and the archive for your platform from the release, over HTTPS, from `github.com` and GitHub's
   asset hosts only (any redirect to another host is refused; 50 MB maximum);
2. checks the archive's SHA-256 against `SHA256SUMS`. Releases are **not signed**: the trust rests on HTTPS and on those
   hosts. Nothing is written to disk before this check passes, and any failure leaves the binary and its folder untouched;
3. writes the new executable next to the current one, swaps it in (the previous one stays as `seedmirage.old`);
4. stops cleanly (a `stopped` announce for every torrent, state saved, port and instance lock released), then relaunches
   itself with the same arguments. The page reloads by itself once the new version answers.

The version installed is always the one the release check found. Only one installation runs at a time.
If the new version does not become ready (the web server never answers), the next launch puts `seedmirage.old` back and
the *Status* page says so. This guard acts once, so it cannot loop.

The button is not offered (and the route is refused) in a Docker container (use `docker compose pull`), for development
builds, and when the folder holding the binary is not writable (system-wide install): the notice then explains it and
links to the release.

**Install updates automatically** (`autoUpdate`, off by default) does the same at the next daily check, unless a torrent
was started in the last minute (it then waits for the following check).

## Clean shutdown

Press Ctrl-C in the terminal (or send `SIGTERM`, or `docker compose down`). seedmirage sends one `stopped` announce for every
started torrent, within 10 seconds, saves its state, then exits. Stopping, archiving or deleting a torrent, and changing the
profile, port or announced IP, also send `stopped` first. Docker waits 15 seconds before killing the container.

If the process is killed hard (`kill -9`, power loss), no `stopped` is sent and trackers will consider the peer gone only after
their own timeout.

## When a tracker returns an error

- A tracker failure (network error, HTTP error, `failure reason`) is retried with an exponential backoff, and the failure
  count is shown in the torrent's state. Other trackers of the torrent are tried according to its tiers.
- After **Tolerated tracker failures** consecutive failures (default 5), the torrent is archived with the reason "too many
  tracker failures". Restore it from the *Archived* tab once the problem is fixed.
- The *Status* page lists the latest warnings and errors; a torrent's page shows its last announce error and the latest
  announces. For more detail run with `--log-level debug` (passkeys in tracker URLs are masked in the logs).
- Frequent causes: a typo in the tracker URL inside the `.torrent`, a tracker that rejects the client you chose (try another
  profile or version), a tracker that requires the torrent to be registered, a tracker that has banned the account.
  A tracker may reject a request for reasons that seedmirage cannot see; there is no workaround built in, by design.

## Language and theme

The interface is available in English and French. The first visit follows your browser's `Accept-Language`; the **EN / FR**
selector in the header overrides it and is remembered in a cookie. The **Theme** button switches between light and dark
(the default follows your system).

## FAQ

**How is this different from JOAL?**
seedmirage was inspired by [JOAL](https://github.com/anthonyraymond/joal) but is a different program, written from
scratch in Go: a single binary with no Java runtime, a local web interface with live updates, its own client-profile format
written from the clients' source code and verified by captures, UDP trackers, scrape-based torrent selection, speed
models, plausibility caps and a schedule. It does not read JOAL's configuration or client files.

**Why local only?**
The interface controls what your machine announces, and a leaked access token would let someone else drive it. It
therefore listens on `127.0.0.1` without TLS, checks the `Host` and `Origin` headers against DNS-rebinding and CSRF
attacks, and requires a token. Listening on another interface is possible with `--listen`, but it prints a warning and is
unsupported. For a remote machine, use an SSH tunnel to `127.0.0.1:7070`.

**Why no magnet links?**
A magnet link carries no `.torrent` metadata: no size and no private flag. seedmirage would have to
download the metadata from other peers (DHT, peer exchange), and it deliberately never connects to peers. Download the
`.torrent` file instead.

**Does it download or upload anything?**
No. It only talks to trackers (announce and scrape). It never opens a connection to a peer and does not listen on the
announced port.

**Which clients can it imitate?**
See the table in [`profiles.md`](profiles.md#embedded-profiles). A profile for another client or version can be built with
[capture mode](capture.md).
