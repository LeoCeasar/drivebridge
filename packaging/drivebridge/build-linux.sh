#!/usr/bin/env bash
set -euo pipefail
source "$(cd "$(dirname "$0")" && pwd)/common.sh"
DB_PLATFORM=linux
parse_args "$@"
python3 "$DB_HERE/package.py" preflight --platform linux --arch "$DB_ARCH" ${DB_MODE[@]+"${DB_MODE[@]}"} \
  --tools go dpkg-deb rpmbuild tar
if ! $DB_CLI_ONLY; then
  need fusermount3
  [ -c /dev/fuse ] || fail '/dev/fuse is required for a full native build; explicit --cli-only --unsigned is test-only'
fi
if ! $DB_UNSIGNED; then
  need gpg; need dpkg-sig; need rpmsign
  [ -n "${DRIVEBRIDGE_GPG_KEY:-}" ] || fail 'release signing requires DRIVEBRIDGE_GPG_KEY (or explicit --unsigned)'
fi
prepare_work
build_stage 0 ''
python3 - "$DB_WORK/tools.json" <<'PY'
import json,subprocess,sys
names=['dpkg-deb','rpmbuild','tar']
open(sys.argv[1],'w').write(json.dumps({n:subprocess.check_output([n,'--version'],text=True).splitlines()[0] for n in names}))
PY
write_manifest
DB_RENDER_MODE=(); if $DB_CLI_ONLY; then DB_RENDER_MODE=(--cli-only); fi
mkdir -p "$DB_WORK/deb/opt/drivebridge" "$DB_WORK/deb/DEBIAN" "$DB_WORK/rpm/SOURCES" "$DB_WORK/rpm/SPECS" "$DB_WORK/rpm/BUILD" "$DB_WORK/rpm/RPMS" "$DB_WORK/rpm/SRPMS" "$DB_WORK/rpm/BUILDROOT"
cp -R "$DB_PAYLOAD/." "$DB_WORK/deb/opt/drivebridge/"
python3 "$DB_HERE/package.py" render --template deb-control ${DB_RENDER_MODE[@]+"${DB_RENDER_MODE[@]}"} --version "$DB_VERSION" --arch "$DB_ARCH" --root "$DB_PAYLOAD" --output "$DB_WORK/deb/DEBIAN/control"
cp "$DB_HERE/templates/linux-preinstall" "$DB_WORK/deb/DEBIAN/preinst"
cp "$DB_HERE/templates/linux-preinstall" "$DB_WORK/deb/DEBIAN/prerm"
chmod 755 "$DB_WORK/deb/DEBIAN/preinst" "$DB_WORK/deb/DEBIAN/prerm"
dpkg-deb --root-owner-group --build "$DB_WORK/deb" "$DB_ARTIFACTS/$DB_NAME.deb"
tar -czf "$DB_WORK/rpm/SOURCES/payload.tar.gz" -C "$DB_PAYLOAD" .
DB_RPM_ARCH=x86_64
[ "$DB_ARCH" != arm64 ] || DB_RPM_ARCH=aarch64
python3 "$DB_HERE/package.py" render --template linux.spec ${DB_RENDER_MODE[@]+"${DB_RENDER_MODE[@]}"} --version "$DB_VERSION" --arch "$DB_RPM_ARCH" --root "$DB_PAYLOAD" --output "$DB_WORK/rpm/SPECS/drivebridge.spec"
rpmbuild -bb --define "_topdir $DB_WORK/rpm" --target "$DB_RPM_ARCH" "$DB_WORK/rpm/SPECS/drivebridge.spec"
cp "$DB_WORK/rpm/RPMS/$DB_RPM_ARCH/drivebridge-$DB_VERSION-1.$DB_RPM_ARCH.rpm" "$DB_ARTIFACTS/$DB_NAME.rpm"
tar -czf "$DB_ARTIFACTS/$DB_NAME.tar.gz" -C "$DB_PAYLOAD" .
if ! $DB_UNSIGNED; then
  dpkg-sig --sign builder -k "$DRIVEBRIDGE_GPG_KEY" "$DB_ARTIFACTS/$DB_NAME.deb"
  rpmsign --define "_gpg_name $DRIVEBRIDGE_GPG_KEY" --addsign "$DB_ARTIFACTS/$DB_NAME.rpm"
  for artifact in "$DB_ARTIFACTS"/*; do gpg --batch --local-user "$DRIVEBRIDGE_GPG_KEY" --armor --detach-sign "$artifact"; done
fi
finish_build
