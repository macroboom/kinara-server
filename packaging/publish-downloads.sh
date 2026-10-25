#!/usr/bin/env bash
# Puts a release's installers on Cloudflare R2, which serves them at
# https://dl.kinara.media (DOWNLOADS_URL), for the website's download buttons.
#
#   packaging/publish-downloads.sh 0.2.0 out/Kinara-Setup-0.2.0-x64.exe out/Kinara-0.2.0-macOS.pkg out/*.deb
#
# Layout in the bucket:
#   server/<version>/<file>          every file of every release, kept (never changes)
#   server/<version>/<file>.sha256
#   server/latest/<stable name>      the newest release under names that don't change,
#                                    so the website links never need editing:
#                                      Kinara-Setup-x64.exe, Kinara-macOS.pkg,
#                                      kinara-server_amd64.deb, kinara-server_arm64.deb
#   server/latest.json               the newest version, and each file's link,
#                                    size and SHA-256 (the website shows these)
#
# A pre-release (a version with a "-", like 0.2.0-rc.1) goes in server/<version>/
# only: latest/ and latest.json stay on the last real release.
#
# Needs the aws CLI and, in the environment:
#   R2_ACCOUNT_ID, R2_ACCESS_KEY_ID, R2_SECRET_ACCESS_KEY  (as for the APT repository)
#   R2_DOWNLOADS_BUCKET   the bucket behind dl.kinara.media
#   DOWNLOADS_URL         its public address (default https://dl.kinara.media)
#
# Setting up the bucket (once), in the Cloudflare dashboard, R2:
#   1. Create a bucket, e.g. kinara-downloads; Settings, Custom Domains:
#      dl.kinara.media. Put its name in the R2_DOWNLOADS_BUCKET secret.
#   2. Give the R2 API token used for the APT repository access to it too
#      (R2, Manage API tokens: Object Read & Write on both buckets).
#   3. Settings, CORS policy, so kinara.media can read latest.json:
#        [{ "AllowedOrigins": ["https://kinara.media", "https://www.kinara.media"],
#           "AllowedMethods": ["GET", "HEAD"], "AllowedHeaders": ["*"], "MaxAgeSeconds": 3600 }]
set -euo pipefail

[ $# -ge 2 ] || { echo "usage: $0 <version> <file>..." >&2; exit 2; }
version="${1#v}"
shift
for v in R2_ACCOUNT_ID R2_ACCESS_KEY_ID R2_SECRET_ACCESS_KEY R2_DOWNLOADS_BUCKET; do
  [ -n "${!v:-}" ] || { echo "$v is not set" >&2; exit 2; }
done
base="${DOWNLOADS_URL:-https://dl.kinara.media}"
base="${base%/}"

export AWS_ACCESS_KEY_ID="$R2_ACCESS_KEY_ID" AWS_SECRET_ACCESS_KEY="$R2_SECRET_ACCESS_KEY"
export AWS_DEFAULT_REGION=auto
# R2 doesn't take the newer default checksums of recent aws CLI versions.
export AWS_REQUEST_CHECKSUM_CALCULATION=WHEN_REQUIRED AWS_RESPONSE_CHECKSUM_VALIDATION=WHEN_REQUIRED
endpoint="https://${R2_ACCOUNT_ID}.r2.cloudflarestorage.com"
bucket="s3://$R2_DOWNLOADS_BUCKET"
put() { aws s3 cp --endpoint-url "$endpoint" --only-show-errors "$@"; }

prerelease=0
case "$version" in *-*) prerelease=1 ;; esac

# The stable name under latest/, and the key in latest.json, for each kind of file.
stable() {
  case "$1" in
    Kinara-Setup-*-x64.exe) echo "windows-x64 Kinara-Setup-x64.exe" ;;
    Kinara-*-macOS.pkg) echo "macos Kinara-macOS.pkg" ;;
    kinara-server_*_amd64.deb) echo "linux-amd64-deb kinara-server_amd64.deb" ;;
    kinara-server_*_arm64.deb) echo "linux-arm64-deb kinara-server_arm64.deb" ;;
    *) echo "" ;;
  esac
}

immutable="public, max-age=31536000, immutable"
entries=()
for f in "$@"; do
  [ -f "$f" ] || { echo "no such file: $f" >&2; exit 2; }
  name="$(basename "$f")"
  sum="$(sha256sum "$f" 2>/dev/null | cut -d' ' -f1 || shasum -a 256 "$f" | cut -d' ' -f1)"
  size="$(wc -c < "$f" | tr -d ' ')"
  printf '%s  %s\n' "$sum" "$name" > "$f.sha256.publish"

  echo "==> server/$version/$name"
  put "$f" "$bucket/server/$version/$name" --cache-control "$immutable" --content-disposition "attachment; filename=\"$name\""
  put "$f.sha256.publish" "$bucket/server/$version/$name.sha256" --cache-control "$immutable" --content-type text/plain
  rm -f "$f.sha256.publish"

  read -r key latest <<<"$(stable "$name")"
  if [ -n "${key:-}" ]; then
    entries+=("$(printf '    "%s": { "name": "%s", "url": "%s/server/%s/%s", "latest": "%s/server/latest/%s", "size": %s, "sha256": "%s" }' \
      "$key" "$name" "$base" "$version" "$name" "$base" "$latest" "$size" "$sum")")
    if [ "$prerelease" = 0 ]; then
      echo "==> server/latest/$latest"
      # Short cache: the same name gets the next release's file.
      put "$f" "$bucket/server/latest/$latest" --cache-control "public, max-age=300" \
        --content-disposition "attachment; filename=\"$name\""
    fi
  fi
done

if [ "$prerelease" = 1 ]; then
  echo "==> $version is a pre-release: server/latest/ and latest.json left as they are"
  exit 0
fi

json="$(mktemp)"
trap 'rm -f "$json"' EXIT
{
  printf '{\n  "version": "%s",\n  "date": "%s",\n' "$version" "$(date -u +%Y-%m-%d)"
  printf '  "notes": "https://github.com/macroboom/kinara-server/releases/tag/v%s",\n' "$version"
  printf '  "files": {\n'
  for i in "${!entries[@]}"; do
    printf '%s' "${entries[$i]}"
    [ "$i" -lt $(( ${#entries[@]} - 1 )) ] && printf ','
    printf '\n'
  done
  printf '  }\n}\n'
} > "$json"
cat "$json"
echo "==> server/latest.json"
put "$json" "$bucket/server/latest.json" --cache-control "public, max-age=300" --content-type application/json
