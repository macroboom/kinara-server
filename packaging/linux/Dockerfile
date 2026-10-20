# The Kinara server image, for linux/amd64 and linux/arm64.
#
# Made from what build.sh leaves in dist/, so the image and the .deb carry
# the same binaries. The build context is the dist folder:
#
#   packaging/linux/build.sh                     # dist/linux_<arch>/ and dist/web/
#   docker build -f packaging/linux/Dockerfile -t kinara-server dist
#
# The release workflow builds both architectures and pushes them to GHCR.
#
# Run it (host networking lets apps find the server and remote access open
# a router port; see docker-compose.yml):
#
#   docker run -d --name kinara --network host \
#     -e PUID=1000 -e PGID=1000 \
#     -v ./config:/config -v ./cache:/cache -v /srv/media:/media:ro \
#     --device /dev/dri:/dev/dri \
#     ghcr.io/macroboom/kinara-server

FROM debian:bookworm-slim

ARG TARGETARCH

# FFmpeg from Jellyfin: HDR tone mapping (libplacebo, OpenCL), Dolby Vision,
# and the VA-API / QSV / NVENC drivers, for both architectures. tini reaps
# the FFmpeg processes; curl is for the health check.
RUN set -eux; \
    apt-get update; \
    apt-get install -y --no-install-recommends ca-certificates curl gnupg tini; \
    install -d -m 0755 /etc/apt/keyrings; \
    curl -fsSL https://repo.jellyfin.org/jellyfin_team.gpg.key | gpg --dearmor -o /etc/apt/keyrings/jellyfin.gpg; \
    printf 'Types: deb\nURIs: https://repo.jellyfin.org/debian\nSuites: bookworm\nComponents: main\nArchitectures: %s\nSigned-By: /etc/apt/keyrings/jellyfin.gpg\n' "$TARGETARCH" \
      > /etc/apt/sources.list.d/jellyfin.sources; \
    apt-get update; \
    apt-get install -y --no-install-recommends jellyfin-ffmpeg7; \
    apt-get purge -y --auto-remove gnupg; \
    rm -rf /var/lib/apt/lists/*; \
    /usr/lib/jellyfin-ffmpeg/ffmpeg -hide_banner -version | head -n1

COPY --chmod=0755 linux_${TARGETARCH}/kinara-server linux_${TARGETARCH}/kinara-iroh /usr/bin/
COPY web/ /usr/share/kinara/web/
COPY --chmod=0755 docker-entrypoint.sh /usr/local/bin/docker-entrypoint.sh

ENV KINARA_DATA=/config \
    KINARA_CACHE=/cache \
    KINARA_LISTEN=:8420 \
    KINARA_WEB=/usr/share/kinara/web \
    KINARA_FFPROBE=/usr/lib/jellyfin-ffmpeg/ffprobe \
    KINARA_FFMPEG=/usr/lib/jellyfin-ffmpeg/ffmpeg \
    KINARA_ENV_FILE=/config/kinara.env \
    NVIDIA_DRIVER_CAPABILITIES=compute,video,utility \
    PUID=1000 \
    PGID=1000

VOLUME ["/config", "/cache"]
# 8420: apps and the web app. 32443: direct HTTPS from outside your home,
# once a Kinara account is linked.
EXPOSE 8420/tcp 32443/tcp

HEALTHCHECK --interval=30s --timeout=5s --start-period=30s --retries=3 \
  CMD curl -fsS "http://127.0.0.1:${KINARA_LISTEN##*:}/api/v1/health" >/dev/null || exit 1

ENTRYPOINT ["/usr/bin/tini", "--", "/usr/local/bin/docker-entrypoint.sh"]
CMD ["serve"]

LABEL org.opencontainers.image.title="Kinara server" \
      org.opencontainers.image.description="Kinara media server for your movies and shows" \
      org.opencontainers.image.vendor="Macroboom LLC" \
      org.opencontainers.image.url="https://kinara.media" \
      org.opencontainers.image.licenses="AGPL-3.0-or-later"
