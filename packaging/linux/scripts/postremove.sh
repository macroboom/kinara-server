#!/bin/sh
# kinara-server .deb: after removing. $1 is "remove", "purge", "upgrade", ...
set -e

if [ -d /run/systemd/system ]; then
  systemctl daemon-reload >/dev/null 2>&1 || true
fi

# Your library (database, settings, cache) is never deleted by the package,
# even on purge: a reinstall picks it up again. Say where it is.
if [ "$1" = purge ]; then
  echo "Kinara's library is kept in /var/lib/kinara (and its cache in /var/cache/kinara)."
  echo "To delete it as well: sudo rm -rf /var/lib/kinara /var/cache/kinara"
fi
exit 0
