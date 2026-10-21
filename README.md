# Kinara Server

The Kinara media server, for your movies and shows. It scans your folders,
finds posters, descriptions and cast on TMDB, and streams to the Kinara apps
(Android TV, phones, Roku and the web):

- plays files as they are when the player can, and converts them when it
  can't, with graphics card encoding and HDR and Dolby Vision handling;
- watch history, Continue Watching and next episodes per profile, Skip intro,
  preview thumbnails while skipping, and watch parties;
- remote play away from home with a Kinara account: direct when the network
  allows, through the Kinara relay when it doesn't, with no port forwarding;
- requests and Discover from TMDB, plus webhooks, incoming status hooks and
  API keys for your own tools, such as a bridge to Sonarr or Radarr.

This repository is the server and its remote access sidecar. The apps are
separate (see License).

## Installing

- **Windows:** the installer from `packaging/windows` (see its README).
- **Debian and Ubuntu:** `apt install kinara-server` from `apt.kinara.media`.
- **Docker:** `ghcr.io/macroboom/kinara-server`.

The Linux package and image, how to install them, and how releases are
made: [packaging/linux/README.md](packaging/linux/README.md). The rest of
this file is for running the server from source.

## Requirements

- Go (the version in `go.mod`)
- Rust, only for remote access (`sidecar/`, see its README)
- ffmpeg (for `ffprobe`); 7.1 or later to stream Dolby Vision profile 7 to
  Apple players and browsers without converting it
- A TMDB API key or read access token, free for development at
  https://www.themoviedb.org/settings/api (optional, but without it titles come
  from file names and there is no artwork)

## First run

```sh
go mod tidy            # fetches the one dependency, modernc.org/sqlite
cp .env.example .env   # then put your TMDB key in .env
go run ./cmd/kinara-server
```

Settings are read from `.env` in the folder you start the server from
(`KINARA_ENV_FILE` points somewhere else). Variables already set in your shell
win over the file. `.env` is git-ignored so your key never gets committed.

Open http://localhost:8420, create the admin account, then add your folders
under **Add a library** (for example `/media/Movies` as Movies and `/media/TV`
as TV shows). The first scan starts right away; the page refreshes while it runs.

Pointing Kinara at an existing Plex library is safe: it only reads the media
folders and keeps its own data in `./kinara-data` (database and image cache).
For extra safety, mount the media read-only.

### Command line

```sh
kinara-server                                   # run the server
kinara-server user add -username alex -admin    # add a user
kinara-server user passwd -username alex        # change a password
kinara-server library add -kind movies /media/Movies
kinara-server library list
kinara-server scan                              # scan everything now and wait
```

| Flag | Environment | Default |
| --- | --- | --- |
| `-data` | `KINARA_DATA` | `./kinara-data` |
| `-listen` | `KINARA_LISTEN` | `:8420` |
| `-tmdb-key` | `KINARA_TMDB_KEY` | none |
| `-ffprobe` | `KINARA_FFPROBE` | next to the server, then `/usr/lib/jellyfin-ffmpeg` (Linux), then `ffprobe` on PATH |
| `-rescan` | | `6h` (0 turns it off) |
| `-scan-workers` | `KINARA_SCAN_WORKERS` | by drive type: SSD one per CPU up to 8, hard drive 2, network 6 |

### Remote play with a Kinara account

Servers use the Kinara cloud at `https://cloud.kinara.media` unless
`KINARA_CLOUD_URL` names another (`off` turns accounts and remote play off).
Nothing is sent to it until you link the server: open the manager, **Remote access**, and choose **Link to my Kinara
account**. Approve the code it shows at the cloud's `/link` page. From then on
the server:

- reports its addresses to the cloud every minute,
- opens a port on the router by NAT-PMP or UPnP (or uses the one you
  forwarded, `KINARA_PUBLIC_PORT`),
- gets a certificate for `*.<server-id>.direct.<domain>` and serves HTTPS on
  `KINARA_HTTPS_PORT`, so players connect straight to it over IPv6 or IPv4,
- keeps a few connections open to the cloud's relay, so players that can't
  connect directly (Roku, browsers away from home) still reach it.

Apps signed in to the account get a short-lived token from the cloud and
exchange it at `POST /api/v1/auth/cloud` for a normal session; the server
checks the token with the cloud's public key, without calling the cloud.
Requests from outside the home network (relay, HTTPS from the internet, the
iroh tunnel) can't create accounts or set the server up.

| Environment | Default | Meaning |
| --- | --- | --- |
| `KINARA_CLOUD_URL` | `https://cloud.kinara.media` | The Kinara cloud (`off`: no accounts or remote play) |
| `KINARA_HTTPS_PORT` | `32443` | Port of the HTTPS listener (0: relay only) |
| `KINARA_PUBLIC_PORT` | none | A port you forwarded on the router to `KINARA_HTTPS_PORT` |
| `KINARA_PORT_MAPPING` | `on` | `off` stops UPnP / NAT-PMP |
| `KINARA_TRUSTED_PROXIES` | none | Reverse proxies in front of the server (`127.0.0.1`, `10.0.0.0/8`): their `X-Forwarded-For` names the visitor |

**On a public address** (a VPS, a forwarded port, or behind a reverse proxy
listed in `KINARA_TRUSTED_PROXIES`), visitors from public addresses are
treated like remote ones: they can sign in, but the server can't be set up
from there, nobody can make an account, and sign-in doesn't list the
accounts. Set a VPS up through an SSH tunnel
(`ssh -L 8420:127.0.0.1:8420 <server>`, then `http://localhost:8420`).

On Windows, allow `kinara-server.exe` through the firewall on the HTTPS port.

## Dolby Vision and HDR

When a player can't take a file as it is, the server converts it, and
Dolby Vision gets special care (`internal/transcode/dovi.go`). Players say
whether they show Dolby Vision with `dovi=1` or `dovi=0` on
`/transcode/{id}` and `/hls/{id}`:

| File | Player shows Dolby Vision | Player doesn't | Didn't say |
|---|---|---|---|
| Profile 5 | copied, tagged `dvh1` in MP4/HLS | converted with libplacebo (needs Vulkan); refused without it, rather than green and purple | as before |
| Profile 7 | copied (MKV), base layer only in MP4/HLS | HDR10 base layer only | base layer only in MP4/HLS |
| Profile 8.1 | copied, tagged `dvh1` in MP4/HLS | HDR10 base layer only | as before |

"Base layer only" strips the Dolby Vision metadata and profile 7's second
layer while copying, with no re-encode. It needs **FFmpeg 7.1 or later**
(the `dovi_rpu` bitstream filter); the log line `ffmpeg checked` says
`dolby_vision_strip=true` when it's there. Each file is checked once with a
two-second test strip; a file that fails it is sent as before.

Converting HDR to SDR: libplacebo when it works (best, and it applies Dolby
Vision itself), else zscale. The zscale path now declares the source's
colours first (`setparams`), leftover HDR metadata is removed from the
frames, and the output is tagged BT.709, so players don't mistake it for
HDR. Other conversion details:

- Surround to stereo lifts dialogue and limits peaks instead of a plain
  downmix (which leaves voices quiet); converted AAC smooths Matroska's
  timestamp gaps (`aresample=async=1`), which some players heard as crackle.
- Hardware encoders aim at 90% of the bitrate cap with the cap as the peak,
  instead of padding easy scenes up to the cap.
- HLS conversions put a keyframe at every 4 s segment boundary.
- The browser MP4 stream uses `delay_moov`, which copied E-AC-3 (Atmos)
  needs.

## Graphics cards for converting

The server converts on the graphics card when it finds one that works. It
tests each with one encoded frame at start, in this order:

| Card | Windows | Linux (and Docker) |
|---|---|---|
| NVIDIA | NVENC (driver new enough for your ffmpeg: FFmpeg 8 needs 610+) | NVENC |
| Intel | Quick Sync (through Direct3D 11) | Quick Sync (through VAAPI, `iHD` driver) |
| AMD | AMF | VAAPI (Mesa's VA driver) |
| Apple | | VideoToolbox (macOS) |

The startup log line `ffmpeg checked` names what it picked (`gpu=`,
`h264_encoder=`, and for VAAPI the device and rate control mode).

Each conversion tries three ways, in order, and moves to the next only when
ffmpeg fails before anything reached the player:

1. **gpu**: decode, scale and encode on the card. For SDR on NVIDIA, Intel
   and VAAPI the picture never leaves the card.
2. **cpu-decode**: decode and scale on the CPU, encode on the card.
3. **cpu**: everything on the CPU (libx264).

The way that worked is remembered per file for 15 minutes, so seeking or
playing it again starts there; the log says `trying the next way` when it
falls back. HDR is tone mapped on the CPU path (libplacebo or zscale)
before going back to the card for encoding. Intel's and AMD's decoders skip
H.264 High 10 (10-bit H.264), which goes straight to CPU decoding.

Settings:

- `KINARA_HWACCEL`: `auto` (default), `nvenc`, `qsv`, `vaapi`, `amf`,
  `videotoolbox`, or `none` to use only the CPU.
- `KINARA_HW_DEVICE`: on Linux, the render node to use
  (`/dev/dri/renderD128`); each is tried otherwise. In Docker, pass the
  card in with `--device /dev/dri:/dev/dri` (Intel/AMD) or the NVIDIA
  runtime.

NVIDIA is the setup that's been tested. The Intel and AMD commands follow
what Silo and Jellyfin run; if one fails on your hardware, the fallback
keeps playback working on the CPU, and the log shows the ffmpeg error.

## Checking playback without an app

Each file on an item's page shows a stream URL with your token. Paste it into
VLC (Media › Open Network Stream) on your laptop or on the Shield. Seeking works
because the server answers HTTP range requests.

The API is described in `internal/api/openapi.yaml`, also served at
http://localhost:8420/openapi.yaml.

## Folder naming

Plex-style naming works best:

```
Movies/Night Shift (2023)/Night Shift (2023).mkv
TV/The Long Harbor (2024)/Season 02/The Long Harbor - S02E05 - Low Tide.mkv
```

Scene names (`Night.Shift.2023.1080p.BluRay.mkv`, `Small.Hours.S03E01.mkv`),
`{tmdb-603}` and `[imdbid-tt0133093]` tags, `Specials` folders, multi-episode
files (`S01E01E02`), and split movies (`cd1`, `part2`) are understood. Samples
and extras folders are skipped for now.

Anything Kinara can't match shows up under **Needs a look** on the library
page, where you can fix it with the TMDB ID.

## Requests, Discover and devices

Anyone signed in can request a movie or show that isn't on the server
(`POST /api/v1/requests`; shows can ask for every season, some seasons or just
the pilot). Owners approve or decline them under Manage > Requests. Approving
emits `request.approved`, which your scripts and tools can pick up through a
webhook. A request becomes "available" once the title shows up in a library.

**Quick Connect** signs a TV in without typing a password: the TV asks for a
six-digit code (`POST /api/v1/auth/quick/start`) and shows it, someone
signed in on their phone (You > Sign in a TV) or in the web app enters it
(`POST /api/v1/auth/quick/approve`), and the TV's next poll
(`POST /api/v1/auth/quick/poll`) signs it in as them. Codes last 10 minutes,
work once and stay on the server (no Kinara account needed). Ten wrong codes
from one account or address within 10 minutes pause approving for a while.

Apps describe themselves when they sign in (a `device` object with a stable
install ID, platform, brand and model), so the device list shows what each
device is. An owner can trust a device (`PUT /api/v1/clients/{clientId}`):
its requests are approved straight away. Trust survives signing out and back
in on the same install.

`GET /api/v1/discover` serves the Discover page from TMDB (trending, new,
popular, coming soon, classics, acclaimed series, and genres), refreshed
hourly; `GET /api/v1/search/external` searches TMDB for titles that aren't in
the library. Both need a TMDB key. See `openapi.yaml` for the shapes.

## Integrations

Connect your own scripts and tools under Manage > Integrations
(`/api/v1/integrations/...`, see `openapi.yaml`).

- **Outgoing webhooks.** Kinara posts JSON to your URLs when things happen
  (titles added or removed, scans, playback, accounts, new devices, requests,
  incoming updates, conversions that failed, remote access changes). Each
  delivery is signed: `X-Kinara-Signature` is `sha256=` and the hex HMAC
  SHA-256 of `<X-Kinara-Timestamp>.<body>` with the webhook's secret.
  Deliveries to one webhook go out in order from a queue that survives
  restarts; failures are retried with growing waits (about 1, 5 and 15
  minutes, then 1, 2, 4, 8 and 12 hours, up to the webhook's retry count).
  The log is kept 7 days. Only http and https addresses are used, and
  redirects to another host are not followed. File paths are only sent to
  webhooks that ask for them.
- **Incoming hooks.** Each has a secret address
  (`POST /api/v1/hooks/in/<token>`) that your tools post the status of titles
  on their way to, in the Kinara format or their own JSON mapped field by
  field. Titles are matched by TMDB, TVDB or IMDb ID, or title and year. The
  apps show them as "On the way" (`GET /api/v1/ontheway`). "done" clears the
  status and can scan the library. Limits: 2 updates a second per title, 60
  a minute per hook.
- **API keys** (`kn_live_...`, sent as `Authorization: Bearer`) let scripts
  read the library, start scans (`POST /api/v1/libraries/{id}/scan?path=` for
  one folder), send statuses (`POST /api/v1/status`), read what's playing,
  and manage webhooks, each only with its scope. Keys never reach accounts,
  library settings or server settings. A "local" key only works from the
  home network, not through remote access. Only a hash of each key is kept.

## Tests

```sh
go test ./...                    # everything, including an end-to-end test
                                 # that generates clips with ffmpeg
go test -tags nosqlite ./...     # quick run without the database driver
```

To check the file name parser against your own library, create
`internal/parse/testdata/corpus.tsv` (format described in `parse_test.go`).
Every mismatch you find in daily use should become a line there.

## Layout

```
cmd/kinara-server     entry point and command line
internal/parse        file and folder names to titles, years, episodes
internal/probe        ffprobe: codecs, HDR, Dolby Vision profile, tracks
internal/scanner      walks folders, files items, marks missing files
internal/tmdb         TMDB client
internal/match        scoring, runtime tiebreaker, merging duplicate copies
internal/images       artwork cache with resizing
internal/store        SQLite schema, migrations, all SQL
internal/auth         password hashing, device tokens, login rate limits
internal/watch        progress, watched state, next episode
internal/api          JSON API, streaming, built-in web page
internal/cloud        Kinara account link: presence, router port, certificate, HTTPS, relay
```

## Known limits of this version

- Files modified in the last minute are skipped until the next check, so
  half-copied files are never matched.
- Multi-episode files are listed under their first episode.
- Passwords use PBKDF2-SHA256 (standard library); Argon2id can replace it later
  without breaking existing hashes.
- The web page uses plain HTTP on your network. HTTPS comes with remote access.

## License

Copyright (C) 2026 Macroboom LLC.

The Kinara server (this repository: the server, the tray app, the iroh
sidecar and the installer scripts) is free software: you can redistribute it
and/or modify it under the terms of the GNU Affero General Public License as
published by the Free Software Foundation, either version 3 of the License,
or (at your option) any later version. It comes with no warranty; see
[LICENSE](LICENSE) for the full terms.

The AGPL also covers running a modified server for other people over a
network: whoever does must offer those people the source of their version.

The Kinara apps are separate programs with their own terms and are not in
this repository. The web app, the Android TV and phone apps and the Roku
channel are proprietary (all rights reserved) and talk to the server only
through its HTTP API. The official installers include the web app; a server
built from this repository serves the API and the built-in pages without it.

FFmpeg, which the installers bundle unmodified, has its own license (GPL v3
for the Windows build); an installed copy lists it in `THIRD-PARTY.txt`.
Kinara uses the TMDB API but is not endorsed or certified by TMDB.
