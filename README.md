# Kinora Server

The Kinora media server, MVP stage: it scans Movies and TV folders, matches them
to TMDB, serves a JSON API for the apps, direct-plays files with seeking, tracks
progress and the next episode, and has a small built-in web page for checking
that libraries load correctly.

Not in this version yet: transcoding, profiles, TV pairing, remote access,
Kinora Cloud. See the architecture plan for the roadmap.

## Requirements

- Go 1.24 or newer
- ffmpeg (for `ffprobe`)
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
