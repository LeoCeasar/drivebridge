#!/usr/bin/env python3
"""Assemble the graphical DMG entry; no installed user state is read here."""
import argparse
import hashlib
import json
from pathlib import Path
import plistlib
import shutil


def bundle(root, executable, identifier, name, version, filename, agent=False):
    contents = root / 'Contents'
    (contents / 'MacOS').mkdir(parents=True)
    (contents / 'Resources').mkdir()
    shutil.copy2(executable, contents / 'MacOS' / filename)
    (contents / 'MacOS' / filename).chmod(0o755)
    info = dict(CFBundleExecutable=filename, CFBundleIdentifier=identifier,
                CFBundleName=name, CFBundleDisplayName=name, CFBundlePackageType='APPL',
                CFBundleShortVersionString=version, CFBundleVersion=version,
                LSMinimumSystemVersion='14.0', LSUIElement=agent, CFBundleIconFile='drivebridge.icns')
    (contents / 'Info.plist').write_bytes(plistlib.dumps(info))


def assemble(payload, launcher, installer, output, version):
    payload, launcher, installer, output = map(Path, (payload, launcher, installer, output))
    output.mkdir()
    entry = output / '安装 DriveBridge.app'
    bundle(entry, installer, 'org.drivebridge.installer', '安装 DriveBridge', version, 'drivebridge-installer')
    app = entry / 'Contents/Resources/DriveBridge.app'
    bundle(app, launcher, 'org.drivebridge.launcher', 'DriveBridge', version, 'drivebridge-launcher', agent=True)
    shutil.copytree(payload, app / 'Contents/Resources/payload')
    icon = payload / 'DriveBridge.app/Contents/Resources/drivebridge.icns'
    for target in (app, entry):
        shutil.copy2(icon, target / 'Contents/Resources/drivebridge.icns')
    write_inventory(app, entry / 'Contents/Resources/application-files.json')


def write_inventory(app, output):
    app, output = Path(app), Path(output)
    # Kept outside the installed app, so signing the app cannot introduce a
    # circular hash dependency. Signed builds refresh this before sealing the
    # containing installer application.
    files = [dict(path=p.relative_to(app).as_posix(), sha256=hashlib.sha256(p.read_bytes()).hexdigest())
             for p in sorted(app.rglob('*')) if p.is_file()]
    output.write_text(json.dumps(files, indent=2) + '\n')


if __name__ == '__main__':
    parser = argparse.ArgumentParser()
    commands = parser.add_subparsers(dest='command', required=True)
    assembly = commands.add_parser('assemble')
    for name in ('payload', 'launcher', 'installer', 'output', 'version'):
        assembly.add_argument('--' + name, required=True)
    inventory = commands.add_parser('inventory')
    inventory.add_argument('--app', required=True)
    inventory.add_argument('--output', required=True)
    args = parser.parse_args()
    if args.command == 'assemble':
        assemble(args.payload, args.launcher, args.installer, args.output, args.version)
    else:
        write_inventory(args.app, args.output)
