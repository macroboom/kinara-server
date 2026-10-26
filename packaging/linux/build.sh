#!/usr/bin/env bash
# Builds Kinara for Linux on this machine's CPU (amd64 or arm64):
#
#   dist/linux_<arch>/kinara-server    the server (Go, no cgo)
#   dist/linux_<arch>/kinara-iroh      the remote access sidecar (Rust)
#   dist/web/                          the web app (from kinara-web, Bun)
#   out/kinara-server_<version>_<arch>.deb   (when nfpm is installed)
#
# The release workflow runs it on an amd64 and an arm64 runner; the Docker
# image and the .deb are both made from what it leaves in dist/.
#
# Run from anywhere:   packaging/linux/build.sh
#
# Settings (environment):
#   VERSION          version to stamp (default: the one in cmd/kinara-server/main.go)
#   KINARA_TMDB_KEY  TMDB key built into the server (default: tmdb.key next to
#                    this script, else KINARA_TMDB_KEY in the repo's .env, else none)
#   KINARA_OPENSUBTITLES_KEY  OpenSubtitles.com API key built in (default:
#                    opensubtitles.key next to this script, else none)
#   WEB_REPO         the kinara-web checkout (default: ../kinara-web next to this repo)
#   SKIP_WEB=1       leave the web app out (the server then serves the API and
#                    its built-in pages only)
#   SKIP_DEB=1       don't make the .deb
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo="$(cd "$here/../.." && pwd)"
cd "$repo"

step() { printf '\n==> %s\n' "$*"; }
die() { printf 'build.sh: %s\n' "$*" >&2; exit 1; }
need() { command -v "$1" >/dev/null 2>&1 || die "$1 not found. $2"; }

case "$(uname -m)" in
  x86_64 | amd64) arch=amd64 ;;
  aarch64 | arm64) arch=arm64 ;;
  *) die "unsupported CPU $(uname -m) (amd64 and arm64 only)" ;;
esac
[ "$(uname -s)" = Linux ] || die "run this on Linux (the sidecar is built for the machine it runs on)"

if [ -z "${VERSION:-}" ]; then
  VERSION="$(sed -nE 's/^(const|var) version = "([^"]+)"/\2/p' cmd/kinara-server/main.go | head -n1)"
fi
VERSION="${VERSION#v}"
[ -n "$VERSION" ] || die "no version (set VERSION, or check cmd/kinara-server/main.go)"
printf 'Kinara %s for linux/%s\n' "$VERSION" "$arch"

# The built-in TMDB key: never in the repository, only in the build.
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

out="$repo/dist/linux_$arch"
rm -rf "$out"
mkdir -p "$out" "$repo/out"

step "kinara-server (go build)"
need go "Install Go from https://go.dev/dl/"
ldflags="-s -w -X main.version=$VERSION"
[ -n "$key" ] && ldflags="$ldflags -X main.builtinTMDBKey=$key"
[ -n "$oskey" ] && ldflags="$ldflags -X main.builtinOpenSubtitlesKey=$oskey"
CGO_ENABLED=0 GOOS=linux GOARCH="$arch" go build -trimpath -ldflags "$ldflags" -o "$out/kinara-server" ./cmd/kinara-server
"$out/kinara-server" version
# The Docker build context is dist/: it needs the entrypoint too.
install -m 0755 "$here/docker-entrypoint.sh" "$repo/dist/docker-entrypoint.sh"

step "kinara-iroh (cargo build --release)"
need cargo "Install Rust from https://rustup.rs"
(cd sidecar && cargo build --release --locked -p kinara-iroh)
install -m 0755 sidecar/target/release/kinara-iroh "$out/kinara-iroh"

web="$repo/dist/web"
if [ "${SKIP_WEB:-}" = 1 ]; then
  printf 'warning: leaving the web app out\n' >&2
  rm -rf "$web"
else
  webrepo="${WEB_REPO:-$repo/../kinara-web}"
  [ -d "$webrepo" ] || die "no kinara-web at $webrepo (set WEB_REPO, or SKIP_WEB=1)"
  # Both arch builds make the same web app; build it once per checkout.
  if [ ! -f "$web/index.html" ] || [ "${REBUILD_WEB:-}" = 1 ]; then
    step "web app (bun run build in $webrepo)"
    need bun "Install Bun from https://bun.sh"
    (cd "$webrepo" && bun install --frozen-lockfile && bun run build)
    [ -f "$webrepo/dist/index.html" ] || die "the web build finished but dist/index.html is missing"
    rm -rf "$web"
    cp -r "$webrepo/dist" "$web"
  else
    step "web app (already in dist/web; REBUILD_WEB=1 to build it again)"
  fi
fi

if [ "${SKIP_DEB:-}" = 1 ]; then
  exit 0
fi
if ! command -v nfpm >/dev/null 2>&1; then
  printf 'warning: nfpm not found, so no .deb (https://nfpm.goreleaser.com/install/)\n' >&2
  exit 0
fi
[ -f "$web/index.html" ] || die "the .deb needs the web app (dist/web); build it, or SKIP_DEB=1"

step ".deb (nfpm)"
deb="$repo/out/kinara-server_${VERSION}_${arch}.deb"
# nfpm doesn't expand variables in file paths: fill them in first.
cfg="$(mktemp)"
trap 'rm -f "$cfg"' EXIT
sed -e "s|\${VERSION}|$VERSION|g" -e "s|\${ARCH}|$arch|g" \
  -e "s|\${DIST}|$repo/dist|g" -e "s|\${REPO}|$repo|g" "$here/nfpm.yaml" > "$cfg"
nfpm package --config "$cfg" --packager deb --target "$deb"
(cd "$repo/out" && sha256sum "$(basename "$deb")" > "$(basename "$deb").sha256")
printf '\nDone: %s\n' "$deb"
