#!/usr/bin/env bash
set -euo pipefail
source "$(cd "$(dirname "$0")" && pwd)/common.sh"
DB_PLATFORM=darwin
parse_args "$@"
python3 "$DB_HERE/package.py" preflight --platform darwin --arch "$DB_ARCH" ${DB_MODE[@]+"${DB_MODE[@]}"} \
  --tools go swiftc pkgbuild productbuild hdiutil codesign xcrun tar
DB_CGO=0 DB_TAGS=''
if ! $DB_CLI_ONLY; then
  DB_FUSE="${DRIVEBRIDGE_FUSE_INCLUDE:-}"
  if [ -z "$DB_FUSE" ]; then
    for candidate in /usr/local/include/fuse /opt/homebrew/include/fuse /usr/local/include/osxfuse/fuse /opt/homebrew/include/osxfuse/fuse /Library/Filesystems/macfuse.fs/Contents/Resources/include/fuse; do
      if [ -f "$candidate/fuse.h" ]; then DB_FUSE="$candidate"; break; fi
    done
  fi
  [ -n "$DB_FUSE" ] && [ -f "$DB_FUSE/fuse.h" ] || fail 'macFUSE fuse.h missing; full writable build blocked. Explicit --cli-only --unsigned creates only a test artifact.'
  export CGO_CFLAGS="${CGO_CFLAGS:-} -I$DB_FUSE"
  DB_CGO=1 DB_TAGS=cmount
fi
if ! $DB_UNSIGNED; then
  [ -n "${DRIVEBRIDGE_CODESIGN_IDENTITY:-}" ] || fail 'Developer ID Application identity required (or explicit --unsigned)'
  [ -n "${DRIVEBRIDGE_INSTALLER_IDENTITY:-}" ] || fail 'Developer ID Installer identity required'
  [ -n "${DRIVEBRIDGE_NOTARY_PROFILE:-}" ] || fail 'notarytool keychain profile required'
fi
prepare_work
if ! $DB_UNSIGNED; then
  # Hash the official bytes before any mutation, sign that exact file, then bind both
  # hashes into our executable. Runtime never reads capability grants from a manifest.
  DB_CLI_UPSTREAM="$(python3 "$DB_HERE/package.py" prepare-cli --platform darwin --arch "$DB_ARCH" --archive "$DB_LARK_ARCHIVE" --output "$DB_WORK/signed-lark-cli")"
  codesign --force --options runtime --timestamp --sign "$DRIVEBRIDGE_CODESIGN_IDENTITY" "$DB_WORK/signed-lark-cli"
  codesign --verify --strict "$DB_WORK/signed-lark-cli"
  DB_CLI_SIGNED="$(python3 "$DB_HERE/package.py" hash-file --path "$DB_WORK/signed-lark-cli")"
  DB_EXTRA_LDFLAGS="-X github.com/rclone/rclone/internal/bridge/manager.ReleaseCLIArtifactSHA256=$DB_CLI_SIGNED -X github.com/rclone/rclone/internal/bridge/manager.ReleaseCLIUpstreamSHA256=$DB_CLI_UPSTREAM"
  DB_STAGE_EXTRA_ARGS+=(--signed-cli "$DB_WORK/signed-lark-cli" --cli-upstream-sha "$DB_CLI_UPSTREAM")
fi
build_stage "$DB_CGO" "$DB_TAGS"
python3 - "$DB_WORK/tools.json" <<'PY'
import hashlib, json, shutil, subprocess, sys
os_version = subprocess.check_output(['sw_vers', '-productVersion'], text=True).strip()
os_build = subprocess.check_output(['sw_vers', '-buildVersion'], text=True).strip()
result = {}
# Apple system packaging tools do not expose a standalone --version. Record their
# owning OS version/build and exact executable hash rather than inventing a version.
for name in ('pkgbuild', 'productbuild', 'codesign'):
    with open(shutil.which(name), 'rb') as f:
        digest = hashlib.sha256(f.read()).hexdigest()
    result[name] = 'Apple system tool, macOS %s (%s); standalone version not exposed; sha256:%s' % (os_version, os_build, digest)
for name, command in {'hdiutil': ['hdiutil','info'], 'sdk': ['xcrun','--show-sdk-version'], 'swift': ['xcrun','swiftc','--version']}.items():
    result[name] = subprocess.check_output(command, stderr=subprocess.STDOUT, text=True).strip().splitlines()[0]
open(sys.argv[1],'w').write(json.dumps(result))
PY
if ! $DB_UNSIGNED; then
  codesign --force --options runtime --timestamp --sign "$DRIVEBRIDGE_CODESIGN_IDENTITY" "$DB_PAYLOAD/bin/drivebridge"
  codesign --verify --strict --verbose=2 "$DB_PAYLOAD/bin/drivebridge"
  codesign --force --options runtime --timestamp --sign "$DRIVEBRIDGE_CODESIGN_IDENTITY" "$DB_PAYLOAD/libexec/DriveBridge Session.app"
  codesign --verify --strict --verbose=2 "$DB_PAYLOAD/libexec/DriveBridge Session.app"
  if $DB_DESKTOP; then
    codesign --force --options runtime --timestamp --sign "$DRIVEBRIDGE_CODESIGN_IDENTITY" "$DB_PAYLOAD/DriveBridge.app"
    codesign --verify --strict --verbose=2 "$DB_PAYLOAD/DriveBridge.app"
  fi
fi
write_manifest
mkdir -p "$DB_WORK/pkgroot/Applications" "$DB_WORK/pkg-scripts"
# All payload mutation/signing is complete; hardlinks avoid a second large scratch copy.
python3 - "$DB_PAYLOAD" "$DB_WORK/pkgroot/Applications/DriveBridge" <<'PYLINK'
import os, shutil, sys
shutil.copytree(sys.argv[1], sys.argv[2], copy_function=os.link)
PYLINK
cp "$DB_HERE/templates/macos-preinstall" "$DB_WORK/pkg-scripts/preinstall"
cp "$DB_PAYLOAD/bin/drivebridge" "$DB_WORK/pkg-scripts/drivebridge-install-verifier"
chmod 755 "$DB_WORK/pkg-scripts/preinstall" "$DB_WORK/pkg-scripts/drivebridge-install-verifier"
pkgbuild --root "$DB_WORK/pkgroot" --identifier org.drivebridge.user --version "$DB_VERSION" \
  --scripts "$DB_WORK/pkg-scripts" --install-location / "$DB_WORK/component.pkg"
python3 "$DB_HERE/package.py" render --template macos-distribution.xml --version "$DB_VERSION" \
  --arch "$DB_ARCH" --root "$DB_PAYLOAD" --output "$DB_WORK/distribution.xml"
DB_PKG_SIGN=()
if ! $DB_UNSIGNED; then DB_PKG_SIGN=(--sign "$DRIVEBRIDGE_INSTALLER_IDENTITY" --timestamp); fi
productbuild --distribution "$DB_WORK/distribution.xml" --package-path "$DB_WORK" \
  ${DB_PKG_SIGN[@]+"${DB_PKG_SIGN[@]}"} "$DB_ARTIFACTS/$DB_NAME.pkg"
if ! $DB_UNSIGNED; then
  xcrun notarytool submit "$DB_ARTIFACTS/$DB_NAME.pkg" --keychain-profile "$DRIVEBRIDGE_NOTARY_PROFILE" --wait
  xcrun stapler staple "$DB_ARTIFACTS/$DB_NAME.pkg"
  xcrun stapler validate "$DB_ARTIFACTS/$DB_NAME.pkg"
fi
if $DB_DESKTOP; then
  # The graphical entry owns the installation lease through the complete copy.
  # Do not expose a raw pkg as the default double-click path: its preinstall
  # verifier deliberately rejects detached, uncoordinated replacement.
  DB_SWIFT_ARCH="$DB_ARCH"
  if [ "$DB_SWIFT_ARCH" = amd64 ]; then DB_SWIFT_ARCH=x86_64; fi
  xcrun swiftc -swift-version 6 -O -target "$DB_SWIFT_ARCH-apple-macos14.0" \
    "$DB_REPO/native/macos-installer/InstallerCore.swift" "$DB_REPO/native/macos-installer/Launcher.swift" -o "$DB_WORK/drivebridge-launcher"
  xcrun swiftc -swift-version 6 -O -target "$DB_SWIFT_ARCH-apple-macos14.0" \
    "$DB_REPO/native/macos-installer/InstallerCore.swift" "$DB_REPO/native/macos-installer/Installer.swift" -o "$DB_WORK/drivebridge-installer"
  python3 "$DB_HERE/macos_app.py" assemble --payload "$DB_PAYLOAD" --launcher "$DB_WORK/drivebridge-launcher" \
    --installer "$DB_WORK/drivebridge-installer" --version "$DB_VERSION" --output "$DB_WORK/dmg"
  if ! $DB_UNSIGNED; then
    codesign --force --options runtime --timestamp --sign "$DRIVEBRIDGE_CODESIGN_IDENTITY" "$DB_WORK/dmg/安装 DriveBridge.app/Contents/Resources/DriveBridge.app"
    codesign --verify --deep --strict "$DB_WORK/dmg/安装 DriveBridge.app/Contents/Resources/DriveBridge.app"
    python3 "$DB_HERE/macos_app.py" inventory --app "$DB_WORK/dmg/安装 DriveBridge.app/Contents/Resources/DriveBridge.app" \
      --output "$DB_WORK/dmg/安装 DriveBridge.app/Contents/Resources/application-files.json"
    codesign --force --options runtime --timestamp --sign "$DRIVEBRIDGE_CODESIGN_IDENTITY" "$DB_WORK/dmg/安装 DriveBridge.app"
    codesign --verify --deep --strict "$DB_WORK/dmg/安装 DriveBridge.app"
  fi
else
  mkdir "$DB_WORK/dmg"
  ln "$DB_ARTIFACTS/$DB_NAME.pkg" "$DB_WORK/dmg/"
fi
# hdiutil's automatic estimate can create an image too small for the package
# (the copy fails with ENOSPC inside /Volumes/DriveBridge despite host space).
# Reserve logical package size plus filesystem metadata and do not hide errors.
DB_DMG_MIB="$(python3 - "$DB_WORK/dmg" <<'PYDMG'
from pathlib import Path
import sys
print((sum(p.stat().st_size for p in Path(sys.argv[1]).rglob('*') if p.is_file()) + 1048575) // 1048576 + 64)
PYDMG
)"
hdiutil create -size "${DB_DMG_MIB}m" -fs HFS+ -volname "DriveBridge $DB_VERSION" -srcfolder "$DB_WORK/dmg" -format UDZO "$DB_ARTIFACTS/$DB_NAME.dmg"
if ! $DB_UNSIGNED; then
  codesign --timestamp --sign "$DRIVEBRIDGE_CODESIGN_IDENTITY" "$DB_ARTIFACTS/$DB_NAME.dmg"
  xcrun notarytool submit "$DB_ARTIFACTS/$DB_NAME.dmg" --keychain-profile "$DRIVEBRIDGE_NOTARY_PROFILE" --wait
  xcrun stapler staple "$DB_ARTIFACTS/$DB_NAME.dmg"
  xcrun stapler validate "$DB_ARTIFACTS/$DB_NAME.dmg"
fi
COPYFILE_DISABLE=1 tar -czf "$DB_ARTIFACTS/$DB_NAME.tar.gz" -C "$DB_PAYLOAD" .
finish_build
