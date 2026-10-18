# Kinora Server

The Kinora media server, MVP stage: it scans Movies and TV folders, matches them
to TMDB, serves a JSON API for the apps, direct-plays files with seeking, tracks
progress and the next episode, and has a small built-in web page for checking
that libraries load correctly.

Not in this version yet: transcoding, profiles, TV pairing, remote access,
Kinora Cloud. See the architecture plan for the roadmap.

## Requirements

- Go 1.24 or newer
- ffmpeg (for `ffprobe`); 7.1 or later to stream Dolby Vision profile 7 to
  Apple players and browsers without converting it
- A TMDB API key or read access token, free for development at
  https://www.themoviedb.org/settings/api (optional, but without it titles come
  from file names and there is no artwork)

## First run

```sh
go mod tidy            # fetches the one dependency, modernc.org/sqlite
cp .env.example .env   # then put your TMDB key in .env
go run ./cmd/kinora-server
```

Settings are read from `.env` in the folder you start the server from
(`KINORA_ENV_FILE` points somewhere else). Variables already set in your shell
win over the file. `.env` is git-ignored so your key never gets committed.

Open http://localhost:8420, create the admin account, then add your folders
under **Add a library** (for example `/media/Movies` as Movies and `/media/TV`
as TV shows). The first scan starts right away; the page refreshes while it runs.

Pointing Kinora at an existing Plex library is safe: it only reads the media
folders and keeps its own data in `./kinora-data` (database and image cache).
For extra safety, mount the media read-only.

### Command line

```sh
kinora-server                                   # run the server
kinora-server user add -username alex -admin    # add a user
kinora-server user passwd -username alex        # change a password
kinora-server library add -kind movies /media/Movies
kinora-server library list
kinora-server scan                              # scan everything now and wait
```

| Flag | Environment | Default |
| --- | --- | --- |
| `-data` | `KINORA_DATA` | `./kinora-data` |
| `-listen` | `KINORA_LISTEN` | `:8420` |
| `-tmdb-key` | `KINORA_TMDB_KEY` | none |
| `-ffprobe` | `KINORA_FFPROBE` | `ffprobe` on PATH |
| `-rescan` | | `6h` (0 turns it off) |
| `-scan-workers` | `KINORA_SCAN_WORKERS` | by drive type: SSD one per CPU up to 8, hard drive 2, network 6 |

### Remote play with a Kinara account

Set `KINORA_CLOUD_URL` to the Kinara cloud (`https://cloud.<domain>`), restart,
then open the manager, **Remote access**, and choose **Link to my Kinara
account**. Approve the code it shows at the cloud's `/link` page. From then on
the server:

- reports its addresses to the cloud every minute,
- opens a port on the router by NAT-PMP or UPnP (or uses the one you
  forwarded, `KINORA_PUBLIC_PORT`),
- gets a certificate for `*.<server-id>.direct.<domain>` and serves HTTPS on
  `KINORA_HTTPS_PORT`, so players connect straight to it over IPv6 or IPv4,
- keeps a few connections open to the cloud's relay, so players that can't
  connect directly (Roku, browsers away from home) still reach it.

Apps signed in to the account get a short-lived token from the cloud and
exchange it at `POST /api/v1/auth/cloud` for a normal session; the server
checks the token with the cloud's public key, without calling the cloud.
Requests from outside the home network (relay, HTTPS from the internet, the
iroh tunnel) can't create accounts or set the server up.

| Environment | Default | Meaning |
| --- | --- | --- |
| `KINORA_CLOUD_URL` | none (off) | The Kinara cloud |
| `KINORA_HTTPS_PORT` | `32443` | Port of the HTTPS listener (0: relay only) |
| `KINORA_PUBLIC_PORT` | none | A port you forwarded on the router to `KINORA_HTTPS_PORT` |
| `KINORA_PORT_MAPPING` | `on` | `off` stops UPnP / NAT-PMP |

On Windows, allow `kinora-server.exe` through the firewall on the HTTPS port.

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

- `KINORA_HWACCEL`: `auto` (default), `nvenc`, `qsv`, `vaapi`, `amf`,
  `videotoolbox`, or `none` to use only the CPU.
- `KINORA_HW_DEVICE`: on Linux, the render node to use
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

Anything Kinora can't match shows up under **Needs a look** on the library
page, where you can fix it with the TMDB ID.

## Requests, Discover and devices

Anyone signed in can request a movie or show that isn't on the server
(`POST /api/v1/requests`; shows can ask for every season, some seasons or just
the pilot). Owners approve or decline them under Manage > Requests. Approving
emits `request.approved`, which your scripts and tools can pick up through a
webhook. A request becomes "available" once the title shows up in a library.

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

- **Outgoing webhooks.** Kinora posts JSON to your URLs when things happen
  (titles added or removed, scans, playback, accounts, new devices, requests,
  incoming updates, conversions that failed, remote access changes). Each
  delivery is signed: `X-Kinora-Signature` is `sha256=` and the hex HMAC
  SHA-256 of `<X-Kinora-Timestamp>.<body>` with the webhook's secret.
  Deliveries to one webhook go out in order from a queue that survives
  restarts; failures are retried with growing waits (about 1, 5 and 15
  minutes, then 1, 2, 4, 8 and 12 hours, up to the webhook's retry count).
  The log is kept 7 days. Only http and https addresses are used, and
  redirects to another host are not followed. File paths are only sent to
  webhooks that ask for them.
- **Incoming hooks.** Each has a secret address
  (`POST /api/v1/hooks/in/<token>`) that your tools post the status of titles
  on their way to, in the Kinora format or their own JSON mapped field by
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
cmd/kinora-server     entry point and command line
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

- Direct play only: the client must handle the file's codecs.
- One watch history per user; profiles come next.
- New files are picked up by the 6-hourly rescan or **Scan now**; there is no
  file watching yet. Files modified in the last minute are skipped until the
  next scan so half-copied files are never matched.
- Multi-episode files are listed under their first episode.
- Passwords use PBKDF2-SHA256 (standard library); Argon2id can replace it later
  without breaking existing hashes.
- The web page uses plain HTTP on your network. HTTPS comes with remote access.
