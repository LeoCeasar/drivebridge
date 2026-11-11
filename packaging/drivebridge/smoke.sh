#!/usr/bin/env bash
# Requires an explicitly provided disposable real mount and independent installer/upgrade evidence.
set -euo pipefail
DB_HERE="$(cd "$(dirname "$0")" && pwd)"
DB_PAYLOAD='' DB_MOUNT='' DB_EVIDENCE='' DB_ARTIFACTS='' DB_RELEASE=false
while [ "$#" -gt 0 ]; do
  case "$1" in
    --payload|--mountpoint|--evidence|--artifacts)
      [ "$#" -ge 2 ] || { echo 'missing argument' >&2; exit 1; }
      case "$1" in
        --payload) DB_PAYLOAD="$2" ;; --mountpoint) DB_MOUNT="$2" ;;
        --evidence) DB_EVIDENCE="$2" ;; --artifacts) DB_ARTIFACTS="$2" ;;
      esac; shift 2 ;;
    --release) DB_RELEASE=true; shift ;;
    *) echo "unknown argument: $1" >&2; exit 1 ;;
  esac
done
[ -n "$DB_PAYLOAD" ] && [ -n "$DB_MOUNT" ] && [ -n "$DB_EVIDENCE" ] && [ -n "$DB_ARTIFACTS" ] || {
  echo 'NOT VERIFIED: require --payload DIR --mountpoint DISPOSABLE_REAL_MOUNT --evidence FILE --artifacts DIR [--release]' >&2; exit 1;
}
python3 "$DB_HERE/package.py" verify --root "$DB_PAYLOAD"
python3 - "$DB_HERE" "$DB_PAYLOAD" "$DB_MOUNT" "$DB_ARTIFACTS" <<'PY'
import importlib.util,json,os,pathlib,shutil,sys,tempfile
spec=importlib.util.spec_from_file_location('pack',pathlib.Path(sys.argv[1])/'package.py');p=importlib.util.module_from_spec(spec);spec.loader.exec_module(p)
root=pathlib.Path(sys.argv[2]); mount=pathlib.Path(sys.argv[3]); artifacts=pathlib.Path(sys.argv[4])
m=json.loads((root/'manifest.json').read_text());p.check_host(m['platform'],m['architecture'])
if not m['mount']: raise SystemExit('NOT VERIFIED: CLI-only bundle cannot pass native mount gate')
p.check_commands(root/'bin/drivebridge',m['platform'],False)
index=json.loads((artifacts/'artifacts.json').read_text())
if index['payload_manifest_sha256']!=p.sha(root/'manifest.json'): raise SystemExit('artifact/payload manifest mismatch')
for item in index['artifacts']:
    name=item['path']
    if pathlib.Path(name).name!=name or p.sha(artifacts/name)!=item['sha256']: raise SystemExit('artifact checksum mismatch')
if m['platform']=='linux':
    p.check_tools(['fusermount3', 'findmnt'])
    if not pathlib.Path('/dev/fuse').exists(): raise SystemExit('NOT VERIFIED: /dev/fuse absent')
elif m['platform']=='darwin':
    if not pathlib.Path('/Library/Filesystems/macfuse.fs').exists(): raise SystemExit('NOT VERIFIED: macFUSE absent')
else: raise SystemExit('Use smoke.ps1 on Windows')
# This gate intentionally cannot pass on an ordinary directory fixture.
if mount.is_symlink() or not os.path.ismount(mount): raise SystemExit('NOT VERIFIED: real mounted filesystem required')
kind=p.run(['findmnt','--mountpoint',str(mount),'-n','-o','FSTYPE']) if m['platform']=='linux' else p.run(['stat','-f','%T',str(mount)])
if kind not in ('fuse.rclone','macfuse','osxfuse'): raise SystemExit('NOT VERIFIED: unsupported mounted filesystem type '+kind)
work=pathlib.Path(tempfile.mkdtemp(prefix='.drivebridge-smoke-',dir=mount))
try:
    data='DriveBridge native write / 原子保存\\n'.encode()
    with (work/'save.tmp').open('wb') as f: f.write(data);f.flush();os.fsync(f.fileno())
    os.replace(work/'save.tmp',work/'saved.txt')
    if (work/'saved.txt').read_bytes()!=data: raise SystemExit('native readback mismatch')
    (work/'saved.txt').rename(work/'renamed.txt')
    if (work/'renamed.txt').read_bytes()!=data: raise SystemExit('native rename mismatch')
finally:
    shutil.rmtree(work)
print('native create/fsync/read/rename passed; remaining gates require bound evidence')
PY
DB_GATE=(--native)
if $DB_RELEASE; then
  DB_GATE=(--release)
  case "$(uname -s)" in
    Darwin)
      codesign --verify --strict "$DB_PAYLOAD/bin/drivebridge"
      codesign --verify --strict "$DB_PAYLOAD/libexec/DriveBridge Session.app"
      codesign --verify --strict "$DB_PAYLOAD/libexec/lark/lark-cli"
      if [ -d "$DB_PAYLOAD/DriveBridge.app" ]; then codesign --verify --strict "$DB_PAYLOAD/DriveBridge.app"; fi
      for pkg in "$DB_ARTIFACTS"/*.pkg; do pkgutil --check-signature "$pkg"; xcrun stapler validate "$pkg"; done
      for dmg in "$DB_ARTIFACTS"/*.dmg; do codesign --verify --strict "$dmg"; xcrun stapler validate "$dmg"; done ;;
    Linux)
      for artifact in "$DB_ARTIFACTS"/*.deb "$DB_ARTIFACTS"/*.rpm "$DB_ARTIFACTS"/*.tar.gz; do
        gpg --verify "$artifact.asc" "$artifact"
      done ;;
  esac
fi
python3 "$DB_HERE/package.py" verify --root "$DB_PAYLOAD" "${DB_GATE[@]}" --evidence "$DB_EVIDENCE"
echo 'Native smoke and supplied install/upgrade/stop evidence passed for this exact payload.'
