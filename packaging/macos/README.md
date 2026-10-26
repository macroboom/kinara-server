# Kinara for macOS

`build.sh` makes `out/Kinara-<version>-macOS.pkg`: one installer for Apple
silicon and Intel Macs (macOS 12 or later), signed with Macroboom LLC's
Developer ID and notarized by Apple, so it opens without warnings.

## What the installer does

It says all of this on its first page, before anything happens.

- Installs `/usr/local/kinara/`: `kinara-server`, `kinara-iroh` (remote
  access), `ffmpeg/` (Jellyfin's FFmpeg: VideoToolbox hardware encoding,
  HDR tone mapping), `web/`, `kinara-launch`, `kinara.env` (the settings
  template), the login items and `uninstall.sh`.
- Installs `/Applications/Kinara.app`, the menu bar item (`menu/main.swift`):
  shows whether the server is running, with Open Kinara, Restart, Stop or
  Start, Show Log and Edit Settings. Opening it from Applications or
  Launchpad opens the web app (and starts the server if it was stopped).
  "Hide Menu Bar Icon" quits only the icon; the server keeps running.
- Makes the server and the menu bar item login items for the user who
  installs it (`~/Library/LaunchAgents/media.kinara.server.plist` and
  `media.kinara.menu.plist`), starts both, and on a first install opens
  `http://localhost:8420` for setup.
- Keeps everything in that user's Library: data and `kinara.env` in
  `Application Support/Kinara`, the cache in `Caches/Kinara`, the log in
  `Logs/Kinara`.

It runs as the user, not as a system service: media in the user's folders,
external drives and macOS's permission prompts all work better that way.
Upgrading is installing a newer package; settings and data stay.

## Updates

The server checks `dl.kinara.media/server/latest.json` twice a day (the
owner can turn this off under Manage, Updates, or with
`KINARA_UPDATE_URL=off`). When a newer version is out, the Manage pages show
it and the menu bar item gets "Update to Kinara X…". **Update** downloads
`Kinara-<version>-macOS.pkg` into `~/Library/Caches/Kinara/updates`, checks
its size and SHA-256 against latest.json, that it's signed by Macroboom LLC
(`pkgutil --check-signature`) and that macOS accepts it as notarized
(`spctl`), then runs `installer` behind macOS's own administrator password
prompt. The postinstall restarts the server and the menu bar item; the
package is deleted when the installer ends, and the new server empties the
updates folder when it starts.

## One-time setup (on the Mac that builds)

1. **Tools:** Xcode (`sudo xcode-select -s /Applications/Xcode.app`), Go,
   Rust (`rustup`), Bun, and `kinara-web` next to this repository.
2. **Certificates.** In the Apple Developer portal (Certificates, Identifiers
   & Profiles; only the Account Holder can make these), create a
   **Developer ID Application** and a **Developer ID Installer** certificate,
   download both and double-click them so they land in the login keychain.
   Check with:
   ```bash
   security find-identity -v | grep "Developer ID"
   ```
3. **Notarizing.** Make an app-specific password at account.apple.com
   (Sign-In and Security), then store it once under the name the script uses:
   ```bash
   xcrun notarytool store-credentials kinara-notary \
     --apple-id you@example.com --team-id <TEAMID> --password <app-specific password>
   ```
4. **TMDB key** in `tmdb.key` next to `build.sh` (git-ignored), as for the
   other platforms.

## Build

```bash
VERSION=0.1.2 packaging/macos/build.sh
```

`UNSIGNED=1` skips signing and notarizing for a quick test on this Mac.
`SKIP_WEB=1` leaves the web app out.

The first build of an FFmpeg version downloads Jellyfin's two portable
macOS builds from GitHub and records their SHA-256 in
`ffmpeg-<version>.sha256`. Commit that file; later builds refuse a download
that doesn't match. The script also refuses an FFmpeg that links anything
other than the system's own libraries, since other Macs wouldn't have it.

## Release workflow (GitHub Actions)

Pushing a tag (`v0.2.0`) builds this package on GitHub's Macs too, signed and
notarized, next to the Windows and Linux builds, and publishes it to
dl.kinara.media (see `.github/workflows/release.yml`). It needs your
certificates as repository secrets (Settings, Secrets and variables,
Actions), once:

1. **The certificates.** In Keychain Access, My Certificates, select both
   Developer ID certificates (each with its key), right-click, Export 2
   items, as `certs.p12` with a password. Then:
   ```bash
   base64 -i certs.p12 | pbcopy
   ```
   and paste it as the secret **MACOS_CERTS_P12**. The password goes in
   **MACOS_CERTS_PASSWORD**. Delete `certs.p12` afterwards (keep your backup
   copy somewhere safe).
2. **Notarizing.** **APPLE_ID** (the Apple ID email), **APPLE_TEAM_ID**
   (`3ZSCTM7V92`) and **APPLE_APP_PASSWORD** (an app-specific password;
   make a separate one for GitHub, so it can be revoked on its own).

The run makes a keychain of its own with these and deletes it at the end.
`ffmpeg-<version>.sha256` must be committed: the workflow won't record a
checksum by itself.

## Testing a build

On a Mac that never had Kinara (or after `sudo /usr/local/kinara/uninstall.sh`):

1. Open the `.pkg`: no "unidentified developer" warning.
2. After installing, the browser opens the setup page; the log is in
   `~/Library/Logs/Kinara/kinara-server.log`.
3. macOS asks to allow incoming connections for `kinara-server`: Allow. A
   TV or phone on the same network finds the server.
4. Play something that needs converting: the log should mention
   `videotoolbox` when the server starts, and converting should be quick.
5. Sign out and in again: the server starts by itself.

## Handy commands

```bash
launchctl kickstart -k gui/$(id -u)/media.kinara.server     # restart (after editing kinara.env)
launchctl bootout gui/$(id -u)/media.kinara.server          # stop until next sign-in
tail -f ~/Library/Logs/Kinara/kinara-server.log
sudo /usr/local/kinara/uninstall.sh                         # remove (keeps data)
```
