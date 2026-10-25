#!/bin/sh
# Removes Kinara from this Mac:
#   sudo /usr/local/kinara/uninstall.sh
#
# Stops the server and the menu bar item, removes their login items,
# /usr/local/kinara and /Applications/Kinara.app. Your
# library database, settings and cache are kept (listed at the end), so a
# reinstall picks up where you left off; delete those folders yourself to
# remove everything.
set -e
[ "$(id -u)" = 0 ] || { echo "Run it with sudo: sudo $0"; exit 1; }

user="${SUDO_USER:-$(stat -f %Su /dev/console)}"
uid="$(id -u "$user")"
home="$(dscl . -read "/Users/$user" NFSHomeDirectory | awk '{print $2}')"

for label in media.kinara.server media.kinara.menu; do
  launchctl bootout "gui/$uid/$label" 2>/dev/null || true
  rm -f "$home/Library/LaunchAgents/$label.plist"
done
pkill -x Kinara 2>/dev/null || true
rm -rf /usr/local/kinara /Applications/Kinara.app
pkgutil --forget media.kinara.server >/dev/null 2>&1 || true

echo "Kinara is removed. Kept (delete them to remove everything):"
echo "  $home/Library/Application Support/Kinara   database and settings"
echo "  $home/Library/Caches/Kinara                 artwork and converted video"
echo "  $home/Library/Logs/Kinara                   logs"
