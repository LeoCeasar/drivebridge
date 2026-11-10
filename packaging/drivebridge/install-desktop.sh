#!/bin/sh
# Opt-in current-user application launcher, not login startup.
set -eu
bundle=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
case "$bundle" in *'
'*|*"$(printf '\r')"*) echo 'Unsupported newline in bundle path' >&2; exit 1;; esac
applications="${XDG_DATA_HOME:-$HOME/.local/share}/applications"
mkdir -p "$applications"
# Desktop Entry string escaping is applied after Exec argument quoting.
exec_path=$(printf '%s' "$bundle/bin/drivebridge-desktop" | sed 's/%/%%/g; s/[\\"$`]/\\&/g; s/\\/\\\\/g')
icon_path=$(printf '%s' "$bundle/share/icons/hicolor/256x256/apps/drivebridge.png" | sed 's/\\/\\\\/g')
umask 077
cat > "$applications/org.drivebridge.desktop.desktop" <<ENTRY
[Desktop Entry]
Type=Application
Name=DriveBridge
Exec="$exec_path"
Icon=$icon_path
Terminal=false
Categories=Network;FileTransfer;
StartupNotify=false
ENTRY
printf 'Application launcher: %s\n' "$applications/org.drivebridge.desktop.desktop"
