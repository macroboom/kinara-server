#!/bin/sh
# Kinara's Docker entrypoint. Started as root, it makes sure the config and
# cache folders belong to PUID:PGID, joins the groups that own the graphics
# devices, then runs the server as that user. Started with --user, it runs
# the server as is.
set -eu

if [ "$(id -u)" != 0 ]; then
  exec /usr/bin/kinara-server "$@"
fi

uid="${PUID:-1000}"
gid="${PGID:-1000}"
case "$uid$gid" in *[!0-9]*) echo "PUID and PGID must be numbers" >&2; exit 1 ;; esac

for dir in "${KINARA_DATA:-/config}" "${KINARA_CACHE:-/cache}"; do
  mkdir -p "$dir"
  # Only when the owner is wrong (first run, or PUID changed): a cache
  # with years of artwork takes a while to walk.
  if [ "$(stat -c '%u:%g' "$dir")" != "$uid:$gid" ]; then
    echo "Giving $dir to $uid:$gid"
    chown -R "$uid:$gid" "$dir"
  fi
done

# Groups owning the passed-in devices (Intel/AMD render nodes, NVIDIA), so
# the server can use them for converting whatever their group IDs are here.
groups="$gid"
for dev in /dev/dri/renderD* /dev/dri/card* /dev/nvidia*; do
  [ -e "$dev" ] || continue
  g="$(stat -c '%g' "$dev")"
  # root's group (NVIDIA's devices are usually root:root, and open to all
  # anyway) would give the server far more than the device.
  [ "$g" = 0 ] && continue
  case ",$groups," in *",$g,"*) ;; *) groups="$groups,$g" ;; esac
done

exec setpriv --reuid="$uid" --regid="$gid" --groups="$groups" \
  --inh-caps=-all --bounding-set=-all --no-new-privs \
  /usr/bin/kinara-server "$@"
