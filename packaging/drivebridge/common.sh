#!/usr/bin/env bash
# Shared by native Unix builds. Never discovers an account or a developer npm prefix.
set -euo pipefail
DB_HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
DB_REPO="$(cd "$DB_HERE/../.." && pwd)"
fail() { echo "packaging error: $*" >&2; exit 1; }
need() { command -v "$1" >/dev/null 2>&1 || fail "missing tool: $1"; }
parse_args() {
  DB_EDITION=feishu DB_GIT_RUNTIME="" DB_ARCH="" DB_VERSION="" DB_LARK="" DB_LARK_ARCHIVE="" DB_OUTPUT="" DB_UNSIGNED=false DB_CLI_ONLY=false DB_DESKTOP=false
  while [ "$#" -gt 0 ]; do
    case "$1" in
      --arch|--version|--lark-package|--lark-native-archive|--output|--git-runtime|--edition)
        [ "$#" -ge 2 ] || fail "missing value for $1"
        case "$1" in
          --edition) DB_EDITION="$2" ;;
          --git-runtime) DB_GIT_RUNTIME="$2" ;; --arch) DB_ARCH="$2" ;; --version) DB_VERSION="$2" ;;
          --lark-package) DB_LARK="$2" ;; --lark-native-archive) DB_LARK_ARCHIVE="$2" ;; --output) DB_OUTPUT="$2" ;;
        esac
        shift 2 ;;
      --unsigned) DB_UNSIGNED=true; shift ;;
      --cli-only) DB_CLI_ONLY=true; shift ;;
      --desktop) DB_DESKTOP=true; shift ;;
      *) fail "unknown argument: $1" ;;
    esac
  done
  [ -n "$DB_ARCH" ] && [ -n "$DB_VERSION" ] && [ -n "$DB_LARK" ] && [ -n "$DB_LARK_ARCHIVE" ] || fail "required: --arch amd64|arm64 --version X.Y.Z --lark-package DIRECTORY --lark-native-archive FILE [--unsigned] [--cli-only | --desktop] [--output DIRECTORY]"
  [[ "$DB_VERSION" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]] || fail "version must be numeric X.Y.Z"
  DB_MODE=()
  case "$DB_EDITION" in feishu|full) ;; *) fail '--edition must be feishu or full' ;; esac
  DB_STAGE_EXTRA_ARGS=()
  if [ -n "$DB_GIT_RUNTIME" ]; then DB_STAGE_EXTRA_ARGS+=(--git-runtime "$DB_GIT_RUNTIME"); fi
  $DB_UNSIGNED && DB_MODE+=(--unsigned)
  $DB_CLI_ONLY && DB_MODE+=(--cli-only)
  $DB_DESKTOP && DB_MODE+=(--desktop)
  if $DB_DESKTOP && $DB_CLI_ONLY; then fail '--desktop requires native mount support; cannot combine with --cli-only'; fi
  DB_NAME="drivebridge-$DB_VERSION-$DB_PLATFORM-$DB_ARCH"
  if [ "$DB_EDITION" = feishu ]; then DB_NAME="$DB_NAME-feishu"; fi
  if $DB_DESKTOP; then DB_NAME="$DB_NAME-desktop"; fi
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
  if [ "$DB_EDITION" = feishu ]; then tags="${tags:+$tags,}drivebridge_feishu"; fi
  (cd "$DB_REPO" && GOOS="$DB_PLATFORM" GOARCH="$DB_ARCH" CGO_ENABLED="$cgo" \
    go build -trimpath -tags "$tags" -ldflags "-s -w ${DB_EXTRA_LDFLAGS:-}" -o "$DB_WORK/drivebridge" .)
  if [ "$DB_PLATFORM" = darwin ]; then
    local swift_arch="$DB_ARCH"
    if [ "$swift_arch" = amd64 ]; then swift_arch=x86_64; fi
    (cd "$DB_REPO" && xcrun swiftc -swift-version 6 -O -target "$swift_arch-apple-macos14.0" \
      -framework Cocoa -framework WebKit native/session-host/main.swift -o "$DB_WORK/drivebridge-session-host")
    DB_STAGE_EXTRA_ARGS+=(--session-host-binary "$DB_WORK/drivebridge-session-host" --version "$DB_VERSION")
  fi
  if $DB_DESKTOP; then
    local desktop_cgo=1
    local desktop_tags="${tags:+$tags,}desktop"
    (cd "$DB_REPO" && GOOS="$DB_PLATFORM" GOARCH="$DB_ARCH" CGO_ENABLED="$desktop_cgo" \
      go build -trimpath -tags "$desktop_tags" -ldflags "-s -w ${DB_EXTRA_LDFLAGS:-}" -o "$DB_WORK/drivebridge-desktop" .)
    DB_STAGE_EXTRA_ARGS+=(--desktop-binary "$DB_WORK/drivebridge-desktop" --version "$DB_VERSION")
  fi
  python3 "$DB_HERE/package.py" stage --platform "$DB_PLATFORM" --arch "$DB_ARCH" \
    --root "$DB_PAYLOAD" --repo "$DB_REPO" --binary "$DB_WORK/drivebridge" --consume-binary --edition "$DB_EDITION" \
    --lark-package "$DB_LARK" --lark-native-archive "$DB_LARK_ARCHIVE" ${DB_MODE[@]+"${DB_MODE[@]}"} ${DB_STAGE_EXTRA_ARGS[@]+"${DB_STAGE_EXTRA_ARGS[@]}"}
}
write_manifest() {
  local signature=unsigned_test
  if ! $DB_UNSIGNED; then signature=signed; fi
  python3 "$DB_HERE/package.py" manifest --platform "$DB_PLATFORM" --arch "$DB_ARCH" \
    --root "$DB_PAYLOAD" --version "$DB_VERSION" --tools "$DB_WORK/tools.json" \
    --signature "$signature" --edition "$DB_EDITION" ${DB_MODE[@]+"${DB_MODE[@]}"}
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
