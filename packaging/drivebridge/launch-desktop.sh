#!/bin/sh
set -eu
bundle=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
exec "$bundle/bin/drivebridge-desktop" "$@"
