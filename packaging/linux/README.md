# Kinara for Linux

Two ways to run the server on Linux, both for amd64 and arm64 (Raspberry Pi
4 and 5, ARM NAS boxes):

- **Debian and Ubuntu:** a package from `apt.kinara.media`, run by systemd.
- **Docker:** `ghcr.io/macroboom/kinara-server`, for anything else (Unraid,
  Synology, TrueNAS, other distributions).

Both are made by the release workflow (`.github/workflows/release.yml`) from
the same build, `build.sh` in this folder.

## Installing with apt

Debian 12 or newer, Ubuntu 22.04 or newer:

```sh
sudo curl -fsSL https://apt.kinara.media/kinara.gpg -o /usr/share/keyrings/kinara.gpg
sudo curl -fsSL https://apt.kinara.media/kinara.sources -o /etc/apt/sources.list.d/kinara.sources
sudo apt update
sudo apt install kinara-server
```

It starts right away and on every boot. Open `http://<this machine>:8420` to
make the owner account and add your folders. `apt upgrade` updates it with
the rest of the system.

**FFmpeg.** The package takes Jellyfin's FFmpeg when it can find it, else
the distribution's. Jellyfin's has the better HDR tone mapping, Dolby Vision
handling and graphics card drivers; to use it, add Jellyfin's repository
first (https://jellyfin.org/docs/general/installation/linux, the
"Repository" steps, then `sudo apt install jellyfin-ffmpeg7`). Kinara picks
it up from `/usr/lib/jellyfin-ffmpeg` by itself.

| | |
| --- | --- |
| Settings | `/etc/kinara/kinara.env` (kept on upgrades), then `sudo systemctl restart kinara-server` |
| Library (database, settings) | `/var/lib/kinara` |
| Cache (artwork, conversions) | `/var/cache/kinara` |
| Logs | `journalctl -u kinara-server` |
| Runs as | the `kinara` user, in the `render` and `video` groups for graphics cards |

**Your media** must be readable by the `kinara` user. For media in a home
folder or on a drive owned by you, either give the folder group read access
(`sudo chgrp -R kinara /srv/media && sudo chmod -R g+rX /srv/media`) or
add `kinara` to a group that can read it.

**Firewall.** Open TCP 8420 for your home network (and 32443 if you use
remote play with a Kinara account), for example `sudo ufw allow 8420/tcp`.

**Removing.** `sudo apt remove kinara-server` stops and removes it and keeps
your library; `apt purge` also removes the settings. The library is never
deleted by the package: `sudo rm -rf /var/lib/kinara /var/cache/kinara` does
that.

## Running with Docker

```sh
docker run -d --name kinara --network host --restart unless-stopped \
  -e PUID=1000 -e PGID=1000 \
  -v ./config:/config -v ./cache:/cache -v /srv/media:/media:ro \
  --device /dev/dri:/dev/dri \
  ghcr.io/macroboom/kinara-server
```

or `docker compose up -d` with `docker-compose.yml` from this folder. Then
open `http://<this machine>:8420`; your folders are under `/media`.

- **PUID and PGID:** the user and group that own your folders (`id` shows
  yours). The container makes `/config` and `/cache` theirs on first start.
- **Host networking** (`--network host`) is what lets the apps find the
  server on your network and remote play open a port on your router.
  With `-p 8420:8420` instead, type the server's address in the apps, and
  remote play goes through the relay.
- **Intel or AMD graphics:** `--device /dev/dri:/dev/dri`. The container
  joins whatever group owns the device.
- **NVIDIA:** install the NVIDIA Container Toolkit on the host and add
  `--gpus all` (or the `deploy` block in the compose file).
- **More settings:** as `-e` variables, or in `config/kinara.env`.
- **Tags:** `latest` (the newest release), `0.2` (the newest 0.2.x), `0.2.0`.

## Releasing (maintainers)

Push a tag: `git tag v0.2.0 && git push origin v0.2.0`. The workflow tests,
builds both architectures, installs and starts the .deb on each to check it,
then publishes the image, the APT repository and a GitHub release. A tag
with a dash (`v0.2.0-rc.1`) is a pre-release: image and GitHub release, but
not `latest` and not the APT repository. **Run workflow** on the Actions tab
builds and checks everything without publishing.

### One-time setup

1. **The repository.** Transfer `kinara-server` and `kinara-web` to the
   organization (Settings > General > Transfer), and make kinara-server
   public (Actions minutes are free for public repositories, including the
   arm64 runners).

2. **Secrets** (kinara-server > Settings > Secrets and variables > Actions):

   | Secret | What |
   | --- | --- |
   | `TMDB_KEY` | Kinara's TMDB key, built into the server |
   | `WEB_REPO_TOKEN` | A fine-grained token with read access to kinara-web's contents (your profile > Settings > Developer settings > Fine-grained tokens; resource owner: the organization; repository: kinara-web; Contents: read-only) |
   | `GPG_PRIVATE_KEY`, `GPG_PASSPHRASE` | The APT signing key, below |
   | `R2_ACCOUNT_ID`, `R2_ACCESS_KEY_ID`, `R2_SECRET_ACCESS_KEY`, `R2_BUCKET` | The bucket behind apt.kinara.media, below |

3. **The signing key**, on your own machine (keep the private key safe
   somewhere besides GitHub: losing it means every user re-adding a new key):

   ```sh
   gpg --quick-gen-key "Kinara packages <support@kinara.media>" ed25519 sign 5y
   gpg --armor --export-secret-keys "Kinara packages"   # GPG_PRIVATE_KEY
   ```

4. **The bucket.** Cloudflare > R2 > Create bucket (`kinara-apt`), then
   Settings > Custom Domains > `apt.kinara.media`. R2 > Manage API tokens >
   Create (Object Read & Write, that bucket only) gives the access key and
   secret; the account ID is on the R2 overview page.

5. **The image.** After the first release, open the organization's
   Packages > kinara-server > Package settings: set visibility to Public and
   link it to the kinara-server repository.

### Building by hand

On Linux, with Go, Rust, Bun and (for the .deb) nfpm:

```sh
packaging/linux/build.sh                              # dist/ and out/kinara-server_<version>_<arch>.deb
docker build -f packaging/linux/Dockerfile -t kinara-server dist
```

`build.sh` takes the TMDB key from `tmdb.key` next to it (git-ignored) or
the repo's `.env`, and the web app from `../kinara-web`; its header lists
the settings.
