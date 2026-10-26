#!/usr/bin/env bash
# Builds Kinara for macOS, for Apple silicon and Intel Macs in one package:
#
#   out/Kinara-<version>-macOS.pkg        the installer, signed and notarized
#   out/Kinara-<version>-macOS.pkg.sha256
#
# What goes in it (all universal binaries, both processors in one file):
#
#   /usr/local/kinara/kinara-server       the server (Go, no cgo)
#   /usr/local/kinara/kinara-iroh         the remote access sidecar (Rust)
#   /usr/local/kinara/ffmpeg/ffmpeg       Jellyfin's FFmpeg, pinned and checked
#   /usr/local/kinara/ffmpeg/ffprobe        (VideoToolbox, HDR tone mapping)
#   /usr/local/kinara/web/                the web app (from kinara-web, Bun)
#   /usr/local/kinara/kinara-launch       starts the server for the signed-in user
#   /usr/local/kinara/kinara.env          the settings file every user starts from
#   /usr/local/kinara/media.kinara.server.plist   the login item (LaunchAgent)
#   /usr/local/kinara/uninstall.sh
#   /Applications/Kinara.app             the menu bar item (menu/main.swift): status,
#                                         Open Kinara, Restart, Stop, log, settings
#
# The installer puts the login items (server and menu bar item) in the
# installing user's ~/Library/LaunchAgents and starts them; see
# scripts/postinstall.
#
# Run on a Mac:   packaging/macos/build.sh
#
# Settings (environment):
#   VERSION          version to stamp (default: the one in cmd/kinara-server/main.go)
#   KINARA_TMDB_KEY  TMDB key built into the server (default: tmdb.key next to
#                    this script, else KINARA_TMDB_KEY in the repo's .env, else none)
#   KINARA_OPENSUBTITLES_KEY  OpenSubtitles.com API key built in (default:
#                    opensubtitles.key next to this script, else none)
#   WEB_REPO         the kinara-web checkout (default: ../kinara-web next to this repo)
#   SKIP_WEB=1       leave the web app out
#   FFMPEG_VERSION   Jellyfin FFmpeg release (default below); the first build of a
#                    version records its checksums in ffmpeg-<version>.sha256 (commit it)
#   SIGN_APP         "Developer ID Application: Macroboom LLC (TEAMID)" (default: found
#                    in the keychain)
#   SIGN_INSTALLER   "Developer ID Installer: Macroboom LLC (TEAMID)" (same)
#   NOTARY_PROFILE   notarytool keychain profile (default kinara-notary; see README.md)
#   UNSIGNED=1       skip signing and notarizing (a test build: macOS will refuse to
#                    open it on other Macs)
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo="$(cd "$here/../.." && pwd)"
cd "$repo"

step() { printf '\n==> %s\n' "$*"; }
die() { printf 'build.sh: %s\n' "$*" >&2; exit 1; }
need() { command -v "$1" >/dev/null 2>&1 || die "$1 not found. $2"; }

[ "$(uname -s)" = Darwin ] || die "run this on a Mac (signing and packaging use Apple's tools)"
need xcrun "Install Xcode, then: sudo xcode-select -s /Applications/Xcode.app"
need lipo "Install Xcode's command line tools: xcode-select --install"

FFMPEG_VERSION="${FFMPEG_VERSION:-8.1.3-1}"
NOTARY_PROFILE="${NOTARY_PROFILE:-kinara-notary}"
# The oldest macOS the Go toolchain supports.
export MACOSX_DEPLOYMENT_TARGET=12.0

if [ -z "${VERSION:-}" ]; then
  VERSION="$(sed -nE 's/^(const|var) version = "([^"]+)"/\2/p' cmd/kinara-server/main.go | head -n1)"
fi
VERSION="${VERSION#v}"
[ -n "$VERSION" ] || die "no version (set VERSION, or check cmd/kinara-server/main.go)"
printf 'Kinara %s for macOS (Apple silicon and Intel)\n' "$VERSION"

# --- Signing identities ------------------------------------------------------------------

if [ "${UNSIGNED:-}" = 1 ]; then
  printf 'warning: UNSIGNED=1, so no signing or notarizing: only for testing on this Mac\n' >&2
else
  if [ -z "${SIGN_APP:-}" ]; then
    SIGN_APP="$(security find-identity -v -p codesigning | sed -nE 's/.*"(Developer ID Application: [^"]+)".*/\1/p' | head -n1)"
  fi
  if [ -z "${SIGN_INSTALLER:-}" ]; then
    SIGN_INSTALLER="$(security find-identity -v | sed -nE 's/.*"(Developer ID Installer: [^"]+)".*/\1/p' | head -n1)"
  fi
  [ -n "$SIGN_APP" ] || die "no \"Developer ID Application\" certificate in the keychain (see README.md, or UNSIGNED=1 for a test build)"
  [ -n "$SIGN_INSTALLER" ] || die "no \"Developer ID Installer\" certificate in the keychain (see README.md, or UNSIGNED=1 for a test build)"
  printf 'Signing as:   %s\nInstaller as: %s\n' "$SIGN_APP" "$SIGN_INSTALLER"
  xcrun notarytool history --keychain-profile "$NOTARY_PROFILE" >/dev/null 2>&1 ||
    die "no notarytool profile \"$NOTARY_PROFILE\" (see README.md: xcrun notarytool store-credentials)"
fi

# --- The built-in TMDB key: never in the repository, only in the build ------------------

key="${KINARA_TMDB_KEY:-}"
if [ -z "$key" ] && [ -f "$here/tmdb.key" ]; then key="$(tr -d '[:space:]' < "$here/tmdb.key")"; fi
if [ -z "$key" ] && [ -f "$repo/.env" ]; then
  key="$(sed -nE 's/^[[:space:]]*KINARA_TMDB_KEY[[:space:]]*=[[:space:]]*//p' "$repo/.env" | tail -n1 | tr -d "\"'[:space:]")"
fi
case "$key" in *[[:space:]]*) die "the TMDB key has spaces in it" ;; esac
if [ -n "$key" ]; then
  printf 'TMDB: building in a key ending ...%s\n' "${key: -4}"
else
  printf 'warning: no TMDB key to build in: titles come from file names, without artwork, until people add their own key\n' >&2
fi

# Kinara's OpenSubtitles key (finding subtitles), the same way: opensubtitles.key
# next to this script, else KINARA_OPENSUBTITLES_KEY. Optional.
oskey="${KINARA_OPENSUBTITLES_KEY:-}"
if [ -z "$oskey" ] && [ -f "$here/opensubtitles.key" ]; then oskey="$(tr -d '[:space:]' < "$here/opensubtitles.key")"; fi
case "$oskey" in *[[:space:]]*) die "the OpenSubtitles key has spaces in it" ;; esac
[ -n "$oskey" ] || printf 'note: no OpenSubtitles key to build in: owners can enter their own under Manage, Subtitles\n' >&2

work="$repo/dist/macos"
root="$work/root"
app="$root/usr/local/kinara"
rm -rf "$work"
mkdir -p "$app/ffmpeg" "$repo/out" "$repo/dist/cache"

# --- kinara-server ------------------------------------------------------------------------

step "kinara-server (go build, arm64 and amd64)"
need go "Install Go from https://go.dev/dl/"
ldflags="-s -w -X main.version=$VERSION"
[ -n "$key" ] && ldflags="$ldflags -X main.builtinTMDBKey=$key"
[ -n "$oskey" ] && ldflags="$ldflags -X main.builtinOpenSubtitlesKey=$oskey"
for arch in arm64 amd64; do
  CGO_ENABLED=0 GOOS=darwin GOARCH="$arch" go build -trimpath -ldflags "$ldflags" -o "$work/kinara-server-$arch" ./cmd/kinara-server
done
lipo -create "$work/kinara-server-arm64" "$work/kinara-server-amd64" -output "$app/kinara-server"
"$app/kinara-server" version

# --- kinara-iroh --------------------------------------------------------------------------

step "kinara-iroh (cargo build --release, arm64 and x86_64)"
need cargo "Install Rust from https://rustup.rs"
rustup target add aarch64-apple-darwin x86_64-apple-darwin >/dev/null
for t in aarch64-apple-darwin x86_64-apple-darwin; do
  (cd sidecar && cargo build --release --locked -p kinara-iroh --target "$t")
done
lipo -create sidecar/target/aarch64-apple-darwin/release/kinara-iroh sidecar/target/x86_64-apple-darwin/release/kinara-iroh \
  -output "$app/kinara-iroh"

# --- FFmpeg (Jellyfin's portable macOS builds, checksums pinned) --------------------------

step "FFmpeg (Jellyfin FFmpeg $FFMPEG_VERSION)"
sums="$here/ffmpeg-$FFMPEG_VERSION.sha256"
base="https://github.com/jellyfin/jellyfin-ffmpeg/releases/download/v$FFMPEG_VERSION"
parts=()
for target in macarm64 mac64; do
  file="jellyfin-ffmpeg_${FFMPEG_VERSION}_portable_${target}-gpl.tar.xz"
  dl="$repo/dist/cache/$file"
  if [ ! -f "$dl" ]; then
    curl -fL --retry 3 -o "$dl.part" "$base/$file" || die "couldn't download $base/$file (is FFMPEG_VERSION a real release?)"
    mv "$dl.part" "$dl"
  fi
  got="$(shasum -a 256 "$dl" | cut -d' ' -f1)"
  if [ -f "$sums" ] && grep -q " $file\$" "$sums"; then
    want="$(grep " $file\$" "$sums" | cut -d' ' -f1)"
    [ "$got" = "$want" ] || die "$file doesn't match the checksum recorded in $(basename "$sums"); delete dist/cache/$file to download it again"
  else
    # In the release workflow, a checksum nobody looked at isn't a check.
    [ -z "${CI:-}" ] || die "no checksum for $file in $(basename "$sums"): build once on the Mac to record it, then commit it"
    printf '%s  %s\n' "$got" "$file" >> "$sums"
    printf 'recorded %s  %s in %s: commit that file, so later builds check it\n' "$got" "$file" "$(basename "$sums")"
  fi
  dir="$work/ffmpeg-$target"
  mkdir -p "$dir"
  tar -xJf "$dl" -C "$dir"
  for tool in ffmpeg ffprobe; do
    [ -f "$dir/$tool" ] || die "no $tool in $file"
    # A portable build links only what every Mac has; anything else (Homebrew,
    # say) would be missing on other Macs.
    if otool -L "$dir/$tool" | tail -n +2 | awk '{print $1}' | grep -vE '^(/usr/lib/|/System/Library/)' | grep -q .; then
      otool -L "$dir/$tool" >&2
      die "$tool in $file needs libraries other Macs won't have"
    fi
  done
  parts+=("$dir")
done
for tool in ffmpeg ffprobe; do
  lipo -create "${parts[0]}/$tool" "${parts[1]}/$tool" -output "$app/ffmpeg/$tool"
  chmod 0755 "$app/ffmpeg/$tool"
done
"$app/ffmpeg/ffmpeg" -hide_banner -version | head -n1
cat > "$app/ffmpeg/README.txt" <<EOF
FFmpeg, as built by the Jellyfin project: jellyfin-ffmpeg $FFMPEG_VERSION
(portable macOS builds, GPL), unmodified, for Apple silicon and Intel.
Source: https://github.com/jellyfin/jellyfin-ffmpeg/tree/v$FFMPEG_VERSION
Licence: GNU GPL version 3 (https://www.gnu.org/licenses/gpl-3.0.html)
EOF

# --- Web app ------------------------------------------------------------------------------

if [ "${SKIP_WEB:-}" = 1 ]; then
  printf 'warning: leaving the web app out\n' >&2
else
  webrepo="${WEB_REPO:-$repo/../kinara-web}"
  [ -d "$webrepo" ] || die "no kinara-web at $webrepo (set WEB_REPO, or SKIP_WEB=1)"
  step "web app (bun run build in $webrepo)"
  need bun "Install Bun from https://bun.sh"
  (cd "$webrepo" && bun install --frozen-lockfile && bun run build)
  [ -f "$webrepo/dist/index.html" ] || die "the web build finished but dist/index.html is missing"
  cp -R "$webrepo/dist" "$app/web"
fi

# --- Kinara.app, the menu bar item -------------------------------------------------------

step "Kinara.app (swiftc, arm64 and x86_64)"
bundle="$root/Applications/Kinara.app"
mkdir -p "$bundle/Contents/MacOS" "$bundle/Contents/Resources"
for t in arm64 x86_64; do
  xcrun --sdk macosx swiftc -O -target "$t-apple-macos$MACOSX_DEPLOYMENT_TARGET" \
    -o "$work/Kinara-$t" "$here/menu/main.swift"
done
lipo -create "$work/Kinara-arm64" "$work/Kinara-x86_64" -output "$bundle/Contents/MacOS/Kinara"
sed -e "s|\${VERSION}|$VERSION|g" "$here/menu/Info.plist" > "$bundle/Contents/Info.plist"
cp "$here/menu/Kinara.icns" "$here/menu/MenuIcon.png" "$here/menu/MenuIcon@2x.png" "$bundle/Contents/Resources/"

# --- The rest of the folder ---------------------------------------------------------------

install -m 0755 "$here/kinara-launch" "$app/kinara-launch"
install -m 0755 "$here/uninstall.sh" "$app/uninstall.sh"
install -m 0644 "$here/kinara.env" "$app/kinara.env"
install -m 0644 "$here/media.kinara.server.plist" "$app/media.kinara.server.plist"
install -m 0644 "$here/media.kinara.menu.plist" "$app/media.kinara.menu.plist"
install -m 0644 LICENSE "$app/LICENSE"

# --- Signing ------------------------------------------------------------------------------

if [ "${UNSIGNED:-}" != 1 ]; then
  step "signing (Developer ID, hardened runtime)"
  for f in ffmpeg/ffmpeg ffmpeg/ffprobe kinara-iroh kinara-server; do
    id="media.kinara.$(basename "$f")"
    codesign --force --options runtime --timestamp --identifier "$id" --sign "$SIGN_APP" "$app/$f"
    codesign --verify --strict "$app/$f"
  done
  codesign --force --options runtime --timestamp --sign "$SIGN_APP" "$bundle"
  codesign --verify --deep --strict "$bundle"
fi

# --- Package ------------------------------------------------------------------------------

step "package (pkgbuild, productbuild)"
# Kinara.app stays in /Applications: an upgrade doesn't go looking for a
# copy moved elsewhere (pkgbuild's default for an app bundle).
pkgbuild --analyze --root "$root" "$work/components.plist" >/dev/null
plutil -replace 0.BundleIsRelocatable -bool NO "$work/components.plist"
pkgbuild --root "$root" --component-plist "$work/components.plist" --scripts "$here/scripts" \
  --identifier media.kinara.server --version "$VERSION" --install-location / \
  --ownership recommended "$work/component.pkg"
sed -e "s|\${VERSION}|$VERSION|g" "$here/distribution.xml" > "$work/distribution.xml"
pkg="$repo/out/Kinara-$VERSION-macOS.pkg"
rm -f "$pkg"
# Two calls, not an empty array: macOS's bash 3.2 treats "${sign[@]}" of an
# empty array as unset under set -u.
if [ "${UNSIGNED:-}" = 1 ]; then
  productbuild --distribution "$work/distribution.xml" --resources "$here/resources" \
    --package-path "$work" "$pkg"
else
  productbuild --distribution "$work/distribution.xml" --resources "$here/resources" \
    --package-path "$work" --sign "$SIGN_INSTALLER" --timestamp "$pkg"
fi

if [ "${UNSIGNED:-}" != 1 ]; then
  step "notarizing (Apple checks it; usually a few minutes)"
  if ! xcrun notarytool submit "$pkg" --keychain-profile "$NOTARY_PROFILE" --wait | tee "$work/notary.txt"; then
    die "notarizing failed; see the log with: xcrun notarytool log <id> --keychain-profile $NOTARY_PROFILE"
  fi
  grep -q 'status: Accepted' "$work/notary.txt" ||
    die "Apple didn't accept it; see why with: xcrun notarytool log <id shown above> --keychain-profile $NOTARY_PROFILE"
  xcrun stapler staple "$pkg"
  spctl --assess --type install -vv "$pkg"
fi

(cd "$repo/out" && shasum -a 256 "$(basename "$pkg")" > "$(basename "$pkg").sha256")
printf '\nDone: %s\n' "$pkg"
