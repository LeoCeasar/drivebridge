#!/usr/bin/env bash
set -euo pipefail
source "$(cd "$(dirname "$0")" && pwd)/common.sh"
DB_PLATFORM=darwin
parse_args "$@"
python3 "$DB_HERE/package.py" preflight --platform darwin --arch "$DB_ARCH" ${DB_MODE[@]+"${DB_MODE[@]}"} \
  --tools go pkgbuild productbuild hdiutil codesign xcrun tar
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
build_stage "$DB_CGO" "$DB_TAGS"
python3 - "$DB_WORK/tools.json" <<'PY'
import json, subprocess, sys
commands={'pkgbuild':['pkgbuild','--version'],'productbuild':['productbuild','--version'],
          'hdiutil':['hdiutil','info'],'codesign':['codesign','--version'],
          'sdk':['xcrun','--show-sdk-version']}
result={}
for k,v in commands.items():
    p=subprocess.run(v,stdout=subprocess.PIPE,stderr=subprocess.STDOUT,text=True)
    # codesign --version is not supported by all SDKs; record OS build as its tool version.
    if k=='codesign' and p.returncode: result[k]=subprocess.check_output(['sw_vers','-buildVersion'],text=True).strip()
    elif p.returncode: raise SystemExit('cannot determine tool version: '+k)
    else: result[k]=p.stdout.strip().splitlines()[0]
open(sys.argv[1],'w').write(json.dumps(result))
PY
if ! $DB_UNSIGNED; then
  for binary in "$DB_PAYLOAD/bin/drivebridge" "$DB_PAYLOAD/libexec/lark/lark-cli"; do
    codesign --force --options runtime --timestamp --sign "$DRIVEBRIDGE_CODESIGN_IDENTITY" "$binary"
    codesign --verify --strict --verbose=2 "$binary"
  done
fi
write_manifest
mkdir -p "$DB_WORK/pkgroot/Applications/DriveBridge" "$DB_WORK/pkg-scripts"
cp -R "$DB_PAYLOAD/." "$DB_WORK/pkgroot/Applications/DriveBridge/"
cp "$DB_HERE/templates/macos-preinstall" "$DB_WORK/pkg-scripts/preinstall"
chmod 755 "$DB_WORK/pkg-scripts/preinstall"
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
mkdir "$DB_WORK/dmg"
cp "$DB_ARTIFACTS/$DB_NAME.pkg" "$DB_WORK/dmg/"
hdiutil create -quiet -volname DriveBridge -srcfolder "$DB_WORK/dmg" -format UDZO "$DB_ARTIFACTS/$DB_NAME.dmg"
if ! $DB_UNSIGNED; then
  codesign --timestamp --sign "$DRIVEBRIDGE_CODESIGN_IDENTITY" "$DB_ARTIFACTS/$DB_NAME.dmg"
  xcrun notarytool submit "$DB_ARTIFACTS/$DB_NAME.dmg" --keychain-profile "$DRIVEBRIDGE_NOTARY_PROFILE" --wait
  xcrun stapler staple "$DB_ARTIFACTS/$DB_NAME.dmg"
  xcrun stapler validate "$DB_ARTIFACTS/$DB_NAME.dmg"
fi
COPYFILE_DISABLE=1 tar -czf "$DB_ARTIFACTS/$DB_NAME.tar.gz" -C "$DB_PAYLOAD" .
finish_build
