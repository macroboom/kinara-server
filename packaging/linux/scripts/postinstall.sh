#!/bin/sh
# kinara-server .deb: after unpacking. $1 is "configure"; $2 is the version
# being upgraded from, empty on a fresh install.
set -e

[ "$1" = configure ] || exit 0

# A system user to run as, with no login and no home of its own outside
# the data folder.
if ! getent group kinara >/dev/null; then
  addgroup --system --quiet kinara
fi
if ! getent passwd kinara >/dev/null; then
  adduser --system --quiet --ingroup kinara --home /var/lib/kinara \
    --no-create-home --shell /usr/sbin/nologin kinara
fi
# Graphics cards for converting: /dev/dri belongs to render (and video).
for g in render video; do
  if getent group "$g" >/dev/null; then
    adduser --quiet kinara "$g" >/dev/null 2>&1 || true
  fi
done

install -d -o kinara -g kinara -m 0750 /var/lib/kinara /var/cache/kinara
# The settings hold a TMDB key if you add one: readable by kinara, not everyone.
chown root:kinara /etc/kinara/kinara.env
chmod 0640 /etc/kinara/kinara.env

if [ -d /run/systemd/system ]; then
  systemctl daemon-reload >/dev/null || true
  if [ -z "$2" ]; then
    systemctl enable --now kinara-server.service >/dev/null || true
    port="$(sed -nE 's/^KINARA_LISTEN=.*:([0-9]+)$/\1/p' /etc/kinara/kinara.env | tail -n1)"
    echo
    echo "Kinara is running. Open http://$(hostname):${port:-8420} in a browser to set it up."
    echo "Settings: /etc/kinara/kinara.env   Logs: journalctl -u kinara-server"
    echo
  else
    systemctl try-restart kinara-server.service >/dev/null || true
  fi
fi
exit 0
