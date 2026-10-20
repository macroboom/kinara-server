#!/usr/bin/env bash
# Adds .deb files to Kinara's APT repository and uploads it to Cloudflare R2,
# which serves it at https://apt.kinara.media.
#
#   packaging/linux/apt/publish.sh out/kinara-server_0.2.0_amd64.deb out/kinara-server_0.2.0_arm64.deb
#
# Needs apt-utils (apt-ftparchive), gnupg, the aws CLI, and in the environment:
#   R2_ACCOUNT_ID, R2_ACCESS_KEY_ID, R2_SECRET_ACCESS_KEY, R2_BUCKET
#   GPG_KEY_ID      the signing key, already imported (the workflow imports it)
#   GPG_PASSPHRASE  its passphrase (optional)
#   APT_KEEP        versions kept per architecture (default 5)
#
# Layout (one suite, "stable", one component, "main"):
#   kinara.gpg                      the public key, for /usr/share/keyrings
#   kinara.sources                  a ready-made /etc/apt/sources.list.d file
#   pool/main/k/kinara-server/*.deb
#   dists/stable/{InRelease,Release,Release.gpg}
#   dists/stable/main/binary-{amd64,arm64}/Packages{,.gz}
#
# The pool is uploaded before the index, so the index never names a file
# that isn't there yet.
set -euo pipefail

[ $# -gt 0 ] || { echo "usage: $0 <.deb>..." >&2; exit 2; }
for v in R2_ACCOUNT_ID R2_ACCESS_KEY_ID R2_SECRET_ACCESS_KEY R2_BUCKET GPG_KEY_ID; do
  [ -n "${!v:-}" ] || { echo "$v is not set" >&2; exit 2; }
done

keep="${APT_KEEP:-5}"
suite=stable
archs=(amd64 arm64)

export AWS_ACCESS_KEY_ID="$R2_ACCESS_KEY_ID" AWS_SECRET_ACCESS_KEY="$R2_SECRET_ACCESS_KEY"
export AWS_DEFAULT_REGION=auto
# R2 doesn't take the newer default checksums of recent aws CLI versions.
export AWS_REQUEST_CHECKSUM_CALCULATION=WHEN_REQUIRED AWS_RESPONSE_CHECKSUM_VALIDATION=WHEN_REQUIRED
endpoint="https://${R2_ACCOUNT_ID}.r2.cloudflarestorage.com"
s3() { aws s3 --endpoint-url "$endpoint" "$@"; }

work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
repo="$work/repo"
pool="$repo/pool/main/k/kinara-server"
mkdir -p "$pool"

echo "==> Fetching the current pool"
s3 sync "s3://$R2_BUCKET/pool/" "$repo/pool/" --only-show-errors

for deb in "$@"; do
  [ -f "$deb" ] || { echo "no such file: $deb" >&2; exit 2; }
  echo "==> Adding $(basename "$deb")"
  cp "$deb" "$pool/"
done

# Keep the newest few versions per architecture.
for arch in "${archs[@]}"; do
  mapfile -t old < <(
    for f in "$pool"/kinara-server_*_"$arch".deb; do
      [ -e "$f" ] && printf '%s %s\n' "$(dpkg-deb -f "$f" Version)" "$f"
    done | sort -V -r -k1,1 | tail -n +"$((keep + 1))" | cut -d' ' -f2-
  )
  for f in "${old[@]}"; do
    echo "==> Dropping $(basename "$f")"
    rm -f "$f"
  done
done

echo "==> Writing the index"
cd "$repo"
for arch in "${archs[@]}"; do
  d="dists/$suite/main/binary-$arch"
  mkdir -p "$d"
  apt-ftparchive --arch "$arch" packages pool > "$d/Packages"
  gzip -9nk "$d/Packages"
done
apt-ftparchive \
  -o APT::FTPArchive::Release::Origin=Kinara \
  -o APT::FTPArchive::Release::Label=Kinara \
  -o APT::FTPArchive::Release::Suite="$suite" \
  -o APT::FTPArchive::Release::Codename="$suite" \
  -o APT::FTPArchive::Release::Architectures="${archs[*]}" \
  -o APT::FTPArchive::Release::Components=main \
  -o APT::FTPArchive::Release::Description="Kinara media server" \
  release "dists/$suite" > "$work/Release"
mv "$work/Release" "dists/$suite/Release"

gpgargs=(--batch --yes --local-user "$GPG_KEY_ID")
if [ -n "${GPG_PASSPHRASE:-}" ]; then
  gpgargs+=(--pinentry-mode loopback --passphrase "$GPG_PASSPHRASE")
fi
gpg "${gpgargs[@]}" --clearsign -o "dists/$suite/InRelease" "dists/$suite/Release"
gpg "${gpgargs[@]}" --armor --detach-sign -o "dists/$suite/Release.gpg" "dists/$suite/Release"

gpg --export "$GPG_KEY_ID" > kinara.gpg
cat > kinara.sources <<'EOF'
Types: deb
URIs: https://apt.kinara.media
Suites: stable
Components: main
Signed-By: /usr/share/keyrings/kinara.gpg
EOF

echo "==> Uploading"
# New packages first, then the index that names them, then old packages
# away: at no point does the live index name a missing file.
s3 sync pool "s3://$R2_BUCKET/pool/" --only-show-errors
s3 cp kinara.gpg "s3://$R2_BUCKET/kinara.gpg" --content-type application/pgp-keys --only-show-errors
s3 cp kinara.sources "s3://$R2_BUCKET/kinara.sources" --content-type text/plain --only-show-errors
# The index changes every release: short caching so apt sees it soon.
s3 sync dists "s3://$R2_BUCKET/dists/" --delete --cache-control "public, max-age=300" --only-show-errors
s3 sync pool "s3://$R2_BUCKET/pool/" --delete --only-show-errors

echo "Published to https://apt.kinara.media ($(ls pool/main/k/kinara-server | wc -l) packages in the pool)"
