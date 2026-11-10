#!/usr/bin/env bash
# Shared by native Unix builds. Never discovers an account or a developer npm prefix.
set -euo pipefail
DB_HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
DB_REPO="$(cd "$DB_HERE/../.." && pwd)"
fail() { echo "packaging error: $*" >&2; exit 1; }
need() { command -v "$1" >/dev/null 2>&1 || fail "missing tool: $1"; }
parse_args() {
  DB_ARCH="" DB_VERSION="" DB_LARK="" DB_LARK_ARCHIVE="" DB_OUTPUT="" DB_UNSIGNED=false DB_CLI_ONLY=false
  while [ "$#" -gt 0 ]; do
    case "$1" in
      --arch|--version|--lark-package|--lark-native-archive|--output)
        [ "$#" -ge 2 ] || fail "missing value for $1"
        case "$1" in
          --arch) DB_ARCH="$2" ;; --version) DB_VERSION="$2" ;;
          --lark-package) DB_LARK="$2" ;; --lark-native-archive) DB_LARK_ARCHIVE="$2" ;; --output) DB_OUTPUT="$2" ;;
        esac
        shift 2 ;;
      --unsigned) DB_UNSIGNED=true; shift ;;
      --cli-only) DB_CLI_ONLY=true; shift ;;
      *) fail "unknown argument: $1" ;;
    esac
  done
  [ -n "$DB_ARCH" ] && [ -n "$DB_VERSION" ] && [ -n "$DB_LARK" ] && [ -n "$DB_LARK_ARCHIVE" ] || fail "required: --arch amd64|arm64 --version X.Y.Z --lark-package DIRECTORY --lark-native-archive FILE [--unsigned] [--cli-only] [--output DIRECTORY]"
  [[ "$DB_VERSION" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]] || fail "version must be numeric X.Y.Z"
  DB_MODE=()
  $DB_UNSIGNED && DB_MODE+=(--unsigned)
  $DB_CLI_ONLY && DB_MODE+=(--cli-only)
  DB_NAME="drivebridge-$DB_VERSION-$DB_PLATFORM-$DB_ARCH"
  if $DB_UNSIGNED; then DB_NAME="$DB_NAME-unsigned-test"; fi
  DB_OUTPUT="${DB_OUTPUT:-$DB_REPO/dist/$DB_NAME}"
  [ ! -e "$DB_OUTPUT" ] || fail "output exists; refusing overwrite: $DB_OUTPUT"
  need python3
}
prepare_work() {
  DB_WORK="$(mktemp -d "${TMPDIR:-/tmp}/drivebridge-package.XXXXXXXX")"
  trap 'rm -rf "$DB_WORK"' EXIT
  DB_PAYLOAD="$DB_WORK/payload"
  DB_ARTIFACTS="$DB_WORK/artifacts"
  mkdir "$DB_ARTIFACTS"
}
build_stage() {
  local cgo="$1" tags="$2"
  (cd "$DB_REPO" && GOOS="$DB_PLATFORM" GOARCH="$DB_ARCH" CGO_ENABLED="$cgo" \
    go build -trimpath -tags "$tags" -ldflags '-s -w' -o "$DB_WORK/drivebridge" .)
  python3 "$DB_HERE/package.py" stage --platform "$DB_PLATFORM" --arch "$DB_ARCH" \
    --root "$DB_PAYLOAD" --repo "$DB_REPO" --binary "$DB_WORK/drivebridge" \
    --lark-package "$DB_LARK" --lark-native-archive "$DB_LARK_ARCHIVE" ${DB_MODE[@]+"${DB_MODE[@]}"}
}
write_manifest() {
  local signature=unsigned_test
  if ! $DB_UNSIGNED; then signature=signed; fi
  python3 "$DB_HERE/package.py" manifest --platform "$DB_PLATFORM" --arch "$DB_ARCH" \
    --root "$DB_PAYLOAD" --version "$DB_VERSION" --tools "$DB_WORK/tools.json" \
    --signature "$signature" ${DB_MODE[@]+"${DB_MODE[@]}"}
  python3 "$DB_HERE/package.py" verify --root "$DB_PAYLOAD"
}
finish_build() {
  python3 "$DB_HERE/package.py" artifacts --root "$DB_PAYLOAD" --dir "$DB_ARTIFACTS"
  cp "$DB_PAYLOAD/manifest.json" "$DB_ARTIFACTS/payload-manifest.json"
  mkdir -p "$(dirname "$DB_OUTPUT")"
  # Publish only after every build/signing command succeeded; existing output is never removed.
  [ ! -e "$DB_OUTPUT" ] || fail "output appeared during build"
  mv "$DB_ARTIFACTS" "$DB_OUTPUT"
  echo "Artifacts: $DB_OUTPUT"
  echo "Native installation/mount/upgrade verification: NOT VERIFIED. Release requires smoke evidence."
}
