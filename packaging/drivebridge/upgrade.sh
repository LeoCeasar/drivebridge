#!/bin/sh
# Explicit current-user wrapper. INSTALLER must remain foreground until complete.
set -eu
if [ "$#" -lt 3 ] || [ "$2" != -- ]; then
  echo 'Usage: upgrade.sh /absolute/candidate/bin/drivebridge -- /absolute/installer [argv...]' >&2
  exit 2
fi
candidate=$1
shift 2
case "$candidate" in /*) ;; *) echo 'Candidate must be absolute' >&2; exit 2;; esac
case "$1" in /*) ;; *) echo 'Installer must be absolute and synchronous' >&2; exit 2;; esac
[ -f "$candidate" ] && [ ! -L "$candidate" ] || exit 2
work=$(mktemp -d "${TMPDIR:-/tmp}/drivebridge-install.XXXXXXXX")
trap 'rm -f "$work/drivebridge-install-helper"; rmdir "$work"' EXIT
# Keep helper outside the replaced installation. Do not launch it elevated.
cp "$candidate" "$work/drivebridge-install-helper"
chmod 700 "$work/drivebridge-install-helper"
"$work/drivebridge-install-helper" feishu maintenance exec -- "$@"
