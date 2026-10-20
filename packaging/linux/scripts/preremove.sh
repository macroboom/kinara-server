#!/bin/sh
# kinara-server .deb: before removing. $1 is "remove", "upgrade" or "deconfigure".
set -e

# On upgrade the new version restarts it (postinstall); only stop for good here.
if [ "$1" = remove ] && [ -d /run/systemd/system ]; then
  systemctl disable --now kinara-server.service >/dev/null 2>&1 || true
fi
exit 0
