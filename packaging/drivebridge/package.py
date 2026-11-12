#!/usr/bin/env python3
"""Offline packaging policy, inventory, and installer rendering (stdlib only)."""
import argparse
import hashlib
import json
import os
from pathlib import Path
import platform
import re
import shutil
import struct
import subprocess
import sys
import tarfile
import uuid
import zipfile
import zlib
import plistlib
import xml.etree.ElementTree as ET

HERE = Path(__file__).resolve().parent
SESSION_APP = 'libexec/DriveBridge Session.app'
SESSION_BINARY = SESSION_APP + '/Contents/MacOS/drivebridge-session-host'
SESSION_PLIST = SESSION_APP + '/Contents/Info.plist'
SESSION_BUNDLE_ID = 'org.drivebridge.sessionhost'
SESSION_MINIMUM_OS = '14.0'
WINDOWS_SESSION_BINARY = 'libexec/drivebridge-session-host.exe'
WINDOWS_SESSION_LICENSE = 'licenses/native-login.LICENSES'
WINDOWS_SESSION_MINIMUM_OS = '10.0.17763'

NATIVE_GATES = ('driver', 'install', 'mount_read_write_rename', 'flush_error_feedback',
                'stop', 'upgrade_preserves_pending', 'uninstall_preserves_userdata', 'user_startup')
# Public upstream help literals, not build/source-directory prefixes.
# Sources: cmd/mountlib/rc.go, fs/config/rc.go, vfs/rc.go, backend/sftp/sftp.go, cmd/bisync/rc.md.
PRODUCT_HELP_EXAMPLES = (b'/home/<user>/mountPoint', b'/home/USER/.cache/rclone',
    b'/home/USER/.rclone.conf', b'/home/user/.cache/rclone/vfs/local/mnt/a',
    b'/home/user/.cache/rclone/vfsMeta/local/mnt/a', b'/home/local/directory',
    b'/home/ncw/.cache/rclone/bisync')
VENDOR_CI_PREFIXES = (b'/home/runner/go/pkg/mod/', b'/home/runner/work/', b'/Users/.../', b'/home/.../')
PRIVATE_PATH = re.compile(rb'(?:/Users/[^/\s\x00]+/|/home/[^/\s\x00]+/|[A-Za-z]:[\\/]Users[\\/][^\\/\s\x00]+[\\/])')


def run(args):
    return subprocess.check_output([str(a) for a in args], stderr=subprocess.STDOUT, encoding='utf-8', errors='replace').strip()


def check_host(target, arch, system=None, machine=None):
    host = {'Darwin': 'darwin', 'Linux': 'linux', 'Windows': 'windows'}.get(system or platform.system())
    cpu = {'arm64': 'arm64', 'aarch64': 'arm64', 'x86_64': 'amd64', 'AMD64': 'amd64'}.get(machine or platform.machine())
    if host != target or cpu != arch or (target == 'windows' and arch != 'amd64'):
        raise ValueError('native OS/architecture runner required: %s/%s (host %s/%s)' % (target, arch, host, cpu))


def check_tools(names):
    for name in names:
        if not shutil.which(name):
            raise ValueError('missing tool: ' + name)


def check_mode(cli_only, unsigned, desktop=False):
    if desktop and cli_only:
        raise ValueError('desktop requires native mount support; --desktop and --cli-only cannot be combined')
    if cli_only and not unsigned:
        raise ValueError('CLI-only packages are unsigned tests; release is blocked')


def check_binary(path, target, arch):
    data = Path(path).read_bytes()
    cpu = None
    if target == 'linux' and data[:6] == b'\x7fELF\x02\x01':
        cpu = {62: 'amd64', 183: 'arm64'}.get(struct.unpack_from('<H', data, 18)[0])
    elif target == 'darwin' and data[:4] == b'\xcf\xfa\xed\xfe':
        cpu = {0x01000007: 'amd64', 0x0100000c: 'arm64'}.get(struct.unpack_from('<I', data, 4)[0])
    elif target == 'windows' and data[:2] == b'MZ' and len(data) >= 64:
        off = struct.unpack_from('<I', data, 60)[0]
        if data[off:off + 4] == b'PE\0\0' and len(data) >= off + 6:
            cpu = {0x8664: 'amd64'}.get(struct.unpack_from('<H', data, off + 4)[0])
    if cpu != arch:
        raise ValueError('binary format/architecture mismatch: ' + str(path))


def check_commands(binary, target, cli_only):
    # Help is offline. Inspect root help too: Cobra can exit zero for unknown help topics.
    root = run([binary, '--help'])
    if not re.search(r'(?m)^\s+feishu\s', root):
        raise ValueError('binary lacks managed feishu command')
    run([binary, 'feishu', '--help'])
    if not cli_only:
        command = 'mount2' if target == 'linux' else 'mount'
        if not re.search(r'(?m)^\s+' + command + r'\s', root):
            raise ValueError('binary lacks required mount command: ' + command)
        run([binary, command, '--help'])


def desktop_files(target):
    if target == 'darwin':
        return {'licenses/desktop.LICENSES', 'DriveBridge.app/Contents/MacOS/drivebridge-desktop',
                'DriveBridge.app/Contents/Info.plist', 'DriveBridge.app/Contents/Resources/drivebridge.icns'}
    if target == 'windows':
        return {'licenses/desktop.LICENSES', 'bin/drivebridge-desktop.exe'}
    return {'licenses/desktop.LICENSES', 'bin/drivebridge-desktop', 'share/applications/org.drivebridge.desktop.desktop',
            'share/icons/hicolor/256x256/apps/drivebridge.png', 'launch-desktop.sh', 'install-desktop.sh'}


def required_files(target, desktop=False):
    suffix = '.exe' if target == 'windows' else ''
    files = {'bin/drivebridge' + suffix, 'libexec/lark/lark-cli' + suffix,
             'licenses/rclone.COPYING', 'licenses/lark-cli.LICENSE',
             'licenses/NOTICE.md', 'licenses/THIRD_PARTY_NOTICES.md', 'README.md',
             'upgrade.ps1' if target == 'windows' else 'upgrade.sh'}
    if target == 'windows':
        files |= {WINDOWS_SESSION_BINARY, WINDOWS_SESSION_LICENSE, 'libexec/node/node.exe', 'licenses/node.LICENSE',
                  'libexec/lark/package/package.json', 'libexec/lark/package/LICENSE',
                  'libexec/lark/package/checksums.txt', 'libexec/lark/package/scripts/run.js',
                  'libexec/lark/package/scripts/install.js', 'libexec/lark/package/scripts/install-wizard.js'}
    if target == 'darwin':
        files |= {SESSION_BINARY, SESSION_PLIST}
    return files | (desktop_files(target) if desktop else set())


def validate_payload(root, target, desktop=False, arch=None):
    root = Path(root)
    required = required_files(target, desktop)
    found = set()
    for path in sorted(root.rglob('*')):
        rel = path.relative_to(root).as_posix()
        if path.is_symlink():
            raise ValueError('symlink forbidden in payload: ' + rel)
        if path.is_dir():
            continue
        if not path.is_file():
            raise ValueError('nonregular payload: ' + rel)
        if rel == 'manifest.json':
            continue
        allowed_plist = target == 'darwin' and (rel == SESSION_PLIST or desktop and rel == 'DriveBridge.app/Contents/Info.plist')
        if (path.suffix in ('.plist', '.service', '.task') or 'startup' in path.name.lower()) and not allowed_plist:
            raise ValueError('duplicate/unmanaged startup item: ' + rel)
        allowed_runtime = target == 'windows' and rel == 'libexec/lark/package/bin/lark-cli.exe'
        allowed_signature = target == 'darwin' and (rel == SESSION_APP + '/Contents/_CodeSignature/CodeResources' or
                                                    desktop and rel == 'DriveBridge.app/Contents/_CodeSignature/CodeResources')
        if rel not in required and not allowed_runtime and not allowed_signature:
            raise ValueError('unapproved payload file (accounts/config are never copied): ' + rel)
        if path.stat().st_size == 0:
            raise ValueError('empty payload file: ' + rel)
        # Check all payload bytes, including binaries. Do not rewrite vendor binaries to hide a failure.
        with path.open('rb') as f:
            previous = b''
            while True:
                part = f.read(1024 * 1024)
                combined = previous + part
                for match in PRIVATE_PATH.finditer(combined):
                    if part and match.start() >= len(combined) - 4096:
                        continue  # inspect the complete prefix with the next chunk
                    tail = combined[match.start():]
                    # stage() admits these vendor paths only from checksum-verified archives;
                    # verify_files() additionally binds every byte to the manifest inventory.
                    vendor = rel in ('libexec/lark/lark-cli', 'libexec/lark/lark-cli.exe', 'libexec/lark/package/bin/lark-cli.exe', 'libexec/node/node.exe')
                    example = rel in ('bin/drivebridge', 'bin/drivebridge.exe', 'bin/drivebridge-desktop', 'bin/drivebridge-desktop.exe', 'DriveBridge.app/Contents/MacOS/drivebridge-desktop') and any(
                        tail.startswith(sample) and len(tail) > len(sample) and tail[len(sample):len(sample)+1] in (b' ', b'\n', b'\"', b')')
                        for sample in PRODUCT_HELP_EXAMPLES)
                    if not example and not (vendor and tail.startswith(VENDOR_CI_PREFIXES)):
                        raise ValueError('developer path in payload: ' + rel)
                if not part:
                    break
                previous = combined[-4096:]
        found.add(rel)
    missing = required - found
    if missing:
        raise ValueError('missing payload/license: ' + ', '.join(sorted(missing)))
    if target == 'darwin':
        validate_session_bundle(root, arch)
    elif target == 'windows' and arch:
        check_windows_gui_binary(root / WINDOWS_SESSION_BINARY, arch)


def sha(path):
    h = hashlib.sha256()
    with Path(path).open('rb') as f:
        for part in iter(lambda: f.read(1024 * 1024), b''):
            h.update(part)
    return h.hexdigest()


def inventory(root):
    root = Path(root)
    return [{'path': p.relative_to(root).as_posix(), 'size': p.stat().st_size, 'sha256': sha(p)}
            for p in sorted(root.rglob('*')) if p.is_file() and p != root / 'manifest.json']


def payload_digest(files):
    return hashlib.sha256(json.dumps(files, sort_keys=True, separators=(',', ':')).encode()).hexdigest()


def verify_files(root, manifest):
    validate_payload(root, manifest['platform'], manifest.get('desktop', False), manifest.get('architecture'))
    if inventory(root) != manifest['files']:
        raise ValueError('payload inventory mismatch; missing, extra, or changed file')


def check_release(manifest, evidence):
    if manifest['signature'] != 'signed':
        raise ValueError('release blocked: unsigned test package')
    if manifest.get('upgrade_guard') != 'lifecycle_lease':
        raise ValueError('release blocked: installer lifecycle lease not integrated')
    check_native_evidence(manifest, evidence)


def check_native_evidence(manifest, evidence):
    if not manifest['mount']:
        raise ValueError('release blocked: mount capability missing')
    if not evidence or not evidence.get('runner'):
        raise ValueError('release blocked: native evidence is required')
    for key in ('platform', 'architecture'):
        if evidence.get(key) != manifest[key]:
            raise ValueError('release blocked: evidence ' + key + ' mismatch')
    if evidence.get('payload_sha256') != payload_digest(manifest['files']):
        raise ValueError('release blocked: evidence must bind exact payload hashes')
    gates = NATIVE_GATES + (('desktop_tray', 'desktop_login', 'desktop_upgrade_exit') if manifest.get('desktop') else ())
    if manifest['platform'] in ('darwin', 'windows'):
        gates += ('native_login',)
    for gate in gates:
        if evidence.get('checks', {}).get(gate) != 'passed':
            raise ValueError('release blocked: native evidence not verified: ' + gate)


def copy_file(source, dest):
    source, dest = Path(source), Path(dest)
    if source.is_symlink() or not source.is_file():
        raise ValueError('required regular input file: ' + str(source))
    dest.parent.mkdir(parents=True, exist_ok=True)
    shutil.copy2(source, dest)


def archive_file(archive, expected_hash, name, dest):
    if sha(archive) != expected_hash:
        raise ValueError('runtime archive checksum mismatch: ' + str(archive))
    dest = Path(dest)
    dest.parent.mkdir(parents=True, exist_ok=True)
    # Extract only a named regular member; never extractall or execute npm install hooks.
    if zipfile.is_zipfile(archive):
        with zipfile.ZipFile(archive) as source:
            item = source.getinfo(name)
            if item.is_dir() or (item.external_attr >> 16) & 0o170000 == 0o120000:
                raise ValueError('runtime archive member is not regular')
            with source.open(item) as src, dest.open('wb') as dst:
                shutil.copyfileobj(src, dst)
    else:
        with tarfile.open(archive) as source:
            item = source.getmember(name)
            if not item.isfile():
                raise ValueError('runtime archive member is not regular')
            with source.extractfile(item) as src, dest.open('wb') as dst:
                shutil.copyfileobj(src, dst)
    dest.chmod(0o755 if dest.suffix != '.LICENSE' else 0o644)


def cli_asset(pin, target, arch):
    ext = '.zip' if target == 'windows' else '.tar.gz'
    return pin['cli']['assets']['lark-cli-%s-%s-%s%s' % (pin['cli']['version'], target, arch, ext)]


def prepare_cli(archive, target, arch, dest):
    pin = json.loads((HERE / 'runtime-lock.json').read_text())
    asset = cli_asset(pin, target, arch)
    archive_file(archive, asset['sha256'], 'lark-cli.exe' if target == 'windows' else 'lark-cli', dest)
    check_binary(dest, target, arch)
    actual = sha(dest)
    if actual != asset['native_sha256']:
        raise ValueError('native executable differs from pinned official upstream hash')
    return actual


def stage(args):
    check_mode(args.cli_only, args.unsigned, getattr(args, 'desktop', False))
    root, repo, package = Path(args.root), Path(args.repo), Path(args.lark_package)
    if root.exists():
        raise ValueError('stage must be new; refusing overwrite: ' + str(root))
    pin = json.loads((HERE / 'runtime-lock.json').read_text())
    pkg = json.loads((package / 'package.json').read_text())
    if pkg.get('name') != pin['cli']['package'] or pkg.get('version') != pin['cli']['version']:
        raise ValueError('official CLI package/version differs from runtime-lock.json')
    for name in ('package.json', 'LICENSE', 'checksums.txt', 'scripts/run.js', 'scripts/install.js', 'scripts/install-wizard.js'):
        source = package / name
        if source.is_symlink() or sha(source) != pin['cli']['package_files'][name]:
            raise ValueError('official CLI package file checksum mismatch: ' + name)
    suffix = '.exe' if args.platform == 'windows' else ''
    check_binary(args.binary, args.platform, args.arch)
    check_commands(args.binary, args.platform, args.cli_only)
    stage_session_host(root, args.platform, args.arch, args.session_host_binary, args.version,
                       getattr(args, 'session_host_licenses', None))
    native = root / ('libexec/lark/lark-cli' + suffix)
    upstream_sha = prepare_cli(args.lark_native_archive, args.platform, args.arch, native)
    if args.signed_cli:
        if args.platform != 'darwin' or args.unsigned or args.cli_only or args.cli_upstream_sha != upstream_sha:
            raise ValueError('signed CLI binding requires a verified upstream macOS release input')
        check_binary(args.signed_cli, args.platform, args.arch)
        run(['codesign', '--verify', '--strict', args.signed_cli])
        copy_file(args.signed_cli, native)
    elif args.cli_upstream_sha:
        raise ValueError('upstream binding without signed CLI')
    if args.consume_binary:
        target = root / ('bin/drivebridge' + suffix)
        target.parent.mkdir(parents=True, exist_ok=True)
        os.replace(args.binary, target)  # caller explicitly transfers its disposable build output
    else:
        copy_file(args.binary, root / ('bin/drivebridge' + suffix))
    copy_file(package / 'LICENSE', root / 'licenses/lark-cli.LICENSE')
    for src, dst in [('COPYING', 'rclone.COPYING'), ('NOTICE.md', 'NOTICE.md'),
                     ('THIRD_PARTY_NOTICES.md', 'THIRD_PARTY_NOTICES.md')]:
        copy_file(repo / src, root / 'licenses' / dst)
    copy_file(HERE / 'BUNDLE-README.md', root / 'README.md')
    wrapper = 'upgrade.ps1' if args.platform == 'windows' else 'upgrade.sh'
    copy_file(HERE / wrapper, root / wrapper)
    if args.platform == 'windows':
        if not args.node_archive:
            raise ValueError('Windows requires official pinned Node ZIP with LICENSE')
        prefix = 'node-v%s-win-x64/' % pin['node']['version']
        for name, dest in [('node.exe', 'libexec/node/node.exe'), ('LICENSE', 'licenses/node.LICENSE')]:
            archive_file(args.node_archive, pin['node']['sha256'], prefix + name, root / dest)
        node = root / 'libexec/node/node.exe'
        check_binary(node, 'windows', 'amd64')
        if sha(node) != pin['node']['binary_sha256']:
            raise ValueError('Node binary differs from official checksum')
        for name in ('package.json', 'LICENSE', 'checksums.txt', 'scripts/run.js', 'scripts/install.js', 'scripts/install-wizard.js'):
            copy_file(package / name, root / 'libexec/lark/package' / name)
        copy_file(native, root / 'libexec/lark/package/bin/lark-cli.exe')
        bins = pkg.get('bin', {})
        if bins != {'lark-cli': 'scripts/run.js'}:
            raise ValueError('unexpected official CLI package launcher')
    if getattr(args, 'desktop', False):
        if not args.desktop_binary:
            raise ValueError('desktop mode requires a native desktop binary')
        stage_desktop(root, args.platform, args.arch, args.desktop_binary, args.version)
    validate_payload(root, args.platform, getattr(args, 'desktop', False), args.arch)


def session_info(version):
    return {'CFBundleExecutable': 'drivebridge-session-host', 'CFBundleIdentifier': SESSION_BUNDLE_ID,
            'CFBundleName': 'DriveBridge Session', 'CFBundleDisplayName': 'DriveBridge 飞书登录',
            'CFBundlePackageType': 'APPL', 'CFBundleShortVersionString': version, 'CFBundleVersion': version,
            'LSMinimumSystemVersion': SESSION_MINIMUM_OS, 'NSHighResolutionCapable': True, 'LSUIElement': True}


def validate_session_bundle(root, arch=None):
    try:
        info = plistlib.loads((Path(root) / SESSION_PLIST).read_bytes())
    except (ValueError, plistlib.InvalidFileException) as exc:
        raise ValueError('invalid native login helper Info.plist') from exc
    if (not isinstance(info, dict) or info.get('CFBundleIdentifier') != SESSION_BUNDLE_ID or
            info.get('CFBundleExecutable') != 'drivebridge-session-host' or info.get('CFBundlePackageType') != 'APPL' or
            info.get('LSMinimumSystemVersion') != SESSION_MINIMUM_OS or info.get('NSHighResolutionCapable') is not True or
            info.get('LSUIElement') is not True or info.get('LSBackgroundOnly')):
        raise ValueError('native login helper identity/minimum OS/activation mismatch')
    if arch:
        check_binary(Path(root) / SESSION_BINARY, 'darwin', arch)


def check_windows_gui_binary(binary, arch):
    check_binary(binary, 'windows', arch)
    data = Path(binary).read_bytes()
    pe = struct.unpack_from('<I', data, 60)[0]
    if len(data) < pe + 94 or struct.unpack_from('<H', data, pe + 92)[0] != 2:
        raise ValueError('Windows native window binary must use windowsgui subsystem')


def stage_session_host(root, target, arch, helper, version, licenses=None):
    if target == 'windows':
        if not helper or not licenses:
            raise ValueError('Windows package requires native login helper binary and licenses')
        check_windows_gui_binary(helper, arch)
        capabilities = json.loads(run([helper, '--capabilities']))
        if (capabilities.get('protocol') != 1 or capabilities.get('bundle_id') != SESSION_BUNDLE_ID or
                capabilities.get('persistent_per_account') is not True or
                capabilities.get('operations') != ['show', 'hide', 'fetch', 'complete', 'shutdown']):
            raise ValueError('Windows native login helper identity/protocol mismatch')
        copy_file(helper, Path(root) / WINDOWS_SESSION_BINARY)
        copy_file(licenses, Path(root) / WINDOWS_SESSION_LICENSE)
        return
    if target != 'darwin':
        if helper or licenses:
            raise ValueError('native login helper is unavailable for this platform')
        return
    if not helper:
        raise ValueError('macOS package requires native login helper binary')
    check_binary(helper, target, arch)
    copy_file(helper, Path(root) / SESSION_BINARY)
    (Path(root) / SESSION_BINARY).chmod(0o755)
    (Path(root) / SESSION_PLIST).write_bytes(plistlib.dumps(session_info(version)))
    validate_session_bundle(root, arch)


def icon_png():
    # Repository-native simple bridge glyph, opaque RGB PNG; no external asset tool.
    n = 256
    rows = bytearray()
    for y in range(n):
        rows.append(0)
        for x in range(n):
            ink = (45 <= x < 76 or 180 <= x < 211) and 55 <= y < 211 or 60 <= x < 196 and 55 <= y < 86 or 60 <= x < 196 and 132 <= y < 163
            rows.extend((255, 255, 255) if ink else (24, 96, 125))
    def chunk(kind, data):
        return struct.pack('>I', len(data)) + kind + data + struct.pack('>I', zlib.crc32(kind + data))
    return b'\x89PNG\r\n\x1a\n' + chunk(b'IHDR', struct.pack('>IIBBBBB', n, n, 8, 2, 0, 0, 0)) + chunk(b'IDAT', zlib.compress(bytes(rows))) + chunk(b'IEND', b'')


def stage_desktop(root, target, arch, binary, version):
    check_binary(binary, target, arch)
    if target == 'windows':
        check_windows_gui_binary(binary, arch)
    # Explicit help args avoid launching a real tray during a build.
    help_text = run([binary, 'desktop', '--help'])
    if 'enable-autostart' not in help_text:
        raise ValueError('desktop binary lacks desktop command')
    capabilities = json.loads(run([binary, 'desktop', 'capabilities']))
    if (capabilities.get('native_tray_compiled') is not True or
            capabilities.get('platform') != target or capabilities.get('architecture') != arch):
        raise ValueError('desktop binary lacks native tray support for requested OS/architecture')
    relative = 'DriveBridge.app/Contents/MacOS/drivebridge-desktop' if target == 'darwin' else 'bin/drivebridge-desktop' + ('.exe' if target == 'windows' else '')
    copy_file(binary, root / relative)
    copy_file(HERE / 'desktop.LICENSES', root / 'licenses/desktop.LICENSES')
    if target == 'darwin':
        contents = root / 'DriveBridge.app/Contents'
        info = {'CFBundleExecutable': 'drivebridge-desktop', 'CFBundleIdentifier': 'org.drivebridge.desktop',
                'CFBundleName': 'DriveBridge', 'CFBundleDisplayName': 'DriveBridge', 'CFBundlePackageType': 'APPL',
                'CFBundleShortVersionString': version, 'CFBundleVersion': version,
                'LSUIElement': True, 'CFBundleIconFile': 'drivebridge.icns', 'NSHighResolutionCapable': True,
                'LSMinimumSystemVersion': SESSION_MINIMUM_OS}
        (contents / 'Info.plist').write_bytes(plistlib.dumps(info))
        resource = contents / 'Resources/drivebridge.icns'
        resource.parent.mkdir(parents=True)
        png = icon_png()
        resource.write_bytes(b'icns' + struct.pack('>I', 16 + len(png)) + b'ic08' + struct.pack('>I', 8 + len(png)) + png)
    elif target == 'linux':
        icon = root / 'share/icons/hicolor/256x256/apps/drivebridge.png'
        icon.parent.mkdir(parents=True)
        icon.write_bytes(icon_png())
        copy_file(HERE / 'templates/drivebridge.desktop', root / 'share/applications/org.drivebridge.desktop.desktop')
        for name in ('launch-desktop.sh', 'install-desktop.sh'):
            copy_file(HERE / name, root / name)


def manifest(args):
    root = Path(args.root)
    check_mode(args.cli_only, args.unsigned, getattr(args, 'desktop', False))
    validate_payload(root, args.platform, getattr(args, 'desktop', False), args.arch)
    tools = json.loads(Path(args.tools).read_text())
    if not tools or any(not value for value in tools.values()):
        raise ValueError('tool versions must be recorded')
    lock = json.loads((HERE / 'runtime-lock.json').read_text())
    manifest = {'schema_version': 1, 'product': 'DriveBridge', 'version': args.version,
                'platform': args.platform, 'architecture': args.arch, 'desktop': getattr(args, 'desktop', False),
                'go_version': run(['go', 'version']), 'tools': tools,
                'runtime': lock, 'cli_binding': {'upstream_sha256': cli_asset(lock, args.platform, args.arch)['native_sha256'], 'artifact_sha256': sha(root / ('libexec/lark/lark-cli.exe' if args.platform == 'windows' else 'libexec/lark/lark-cli'))}, 'vendor_metadata': {'embedded_ci_prefixes': [p.decode() for p in VENDOR_CI_PREFIXES], 'policy': 'Only checksum-verified vendor executables may retain upstream CI source paths; bytes are never rewritten.', 'product_help_examples': [p.decode() for p in PRODUCT_HELP_EXAMPLES]}, 'mount': not args.cli_only,
                'mount_engine': 'none' if args.cli_only else ('mount2' if args.platform == 'linux' else 'cmount'),
                'native_login': {'available': args.platform in ('darwin', 'windows'),
                                 'provider': {'darwin': 'WKWebView', 'windows': 'WebView2'}.get(args.platform, 'unavailable'),
                                 'minimum_os': {'darwin': SESSION_MINIMUM_OS, 'windows': WINDOWS_SESSION_MINIMUM_OS}.get(args.platform),
                                 'verification': 'not_verified'},
                'signature': args.signature, 'upgrade_guard': 'lifecycle_lease_unverified', 'native_verification': 'not_verified',
                'release_blockers': ['native installation, upgrade, driver and mount evidence required', 'native installer lease environment/identity/lifetime propagation not verified'],
                'licenses': sorted(p.relative_to(root).as_posix() for p in (root / 'licenses').iterdir()),
                'files': inventory(root)}
    if args.signature == 'unsigned_test':
        manifest['release_blockers'].append('unsigned test artifact')
    if args.cli_only:
        manifest['release_blockers'].append('CLI-only; writable mount unavailable')
    (root / 'manifest.json').write_text(json.dumps(manifest, indent=2) + '\n')


def render(args):
    # Generate one component per whitelisted file.
    root, out = Path(args.root).resolve(), Path(args.output)
    template = (HERE / 'templates' / args.template).read_text()
    replacements = {'@VERSION@': args.version, '@ARCH@': ('x86_64' if args.template == 'macos-distribution.xml' and args.arch == 'amd64' else args.arch), '@PAYLOAD@': str(root), '@OUTPUT@': str(out.parent.resolve()), '@DEB_DEPENDS@': 'procps' if getattr(args, 'cli_only', False) else 'fuse3, procps', '@RPM_FUSE@': '' if getattr(args, 'cli_only', False) else 'Requires: fuse3'}
    if getattr(args, 'desktop', False) and args.template == 'deb-control':
        replacements['@DEB_DEPENDS@'] += ', libgtk-3-0, libayatana-appindicator3-1'
    if getattr(args, 'desktop', False) and args.template == 'linux.spec':
        replacements['@RPM_FUSE@'] += '\nRequires: gtk3, libayatana-appindicator-gtk3'
        template = template.replace('%pre\n', 'mkdir -p %{buildroot}/usr/share/applications %{buildroot}/usr/share/icons/hicolor/256x256/apps\ncp %{buildroot}/opt/drivebridge/share/applications/org.drivebridge.desktop.desktop %{buildroot}/usr/share/applications/\ncp %{buildroot}/opt/drivebridge/share/icons/hicolor/256x256/apps/drivebridge.png %{buildroot}/usr/share/icons/hicolor/256x256/apps/\n%pre\n')
        template += '\n/usr/share/applications/org.drivebridge.desktop.desktop\n/usr/share/icons/hicolor/256x256/apps/drivebridge.png\n'
    if args.template == 'windows.wxs':
        ns = 'http://wixtoolset.org/schemas/v4/wxs'
        ET.register_namespace('', ns)
        tree = ET.fromstring(template.replace('@VERSION@', args.version))
        package = tree.find('{%s}Package' % ns)
        component_namespace = uuid.UUID(package.get('UpgradeCode'))
        tree.find('.//{%s}Binary' % ns).set('SourceFile', str(root / 'bin' / 'drivebridge.exe'))
        directory = tree.find('.//{%s}Directory[@Id="INSTALLFOLDER"]' % ns)
        group = tree.find('.//{%s}ComponentGroup' % ns)
        dirs = {'': directory}
        for path in sorted(p for p in root.rglob('*') if p.is_file()):
            rel = path.relative_to(root)
            current = ''
            for part in rel.parts[:-1]:
                parent = current
                current = current + '/' + part
                if current not in dirs:
                    dirs[current] = ET.SubElement(dirs[parent], '{%s}Directory' % ns, Id='D' + hashlib.sha256(current.encode()).hexdigest()[:16], Name=part)
            suffix = hashlib.sha256(rel.as_posix().encode()).hexdigest()[:16]
            # WiX cannot generate a GUID for a file with an HKCU registry key path.
            guid = str(uuid.uuid5(component_namespace, rel.as_posix().casefold()))
            component = ET.SubElement(dirs[current], '{%s}Component' % ns, Id='C' + suffix, Guid=guid)
            ET.SubElement(component, '{%s}File' % ns, Id='F' + suffix, Source=str(path))
            ET.SubElement(component, '{%s}RegistryValue' % ns, Root='HKCU', Key='Software\\DriveBridge\\Installer\\Files', Name='F' + suffix, Type='string', Value='[#F%s]' % suffix, KeyPath='yes')
            ET.SubElement(group, '{%s}ComponentRef' % ns, Id='C' + suffix)
        if (root / 'bin/drivebridge-desktop.exe').is_file():
            desktop = ET.SubElement(package, '{%s}StandardDirectory' % ns, Id='DesktopFolder')
            component = ET.SubElement(desktop, '{%s}Component' % ns, Id='DesktopShortcut', Guid='*')
            ET.SubElement(component, '{%s}Shortcut' % ns, Id='DriveBridgeDesktopShortcut', Name='DriveBridge', Target='[INSTALLFOLDER]bin\\drivebridge-desktop.exe', WorkingDirectory='INSTALLFOLDER')
            ET.SubElement(component, '{%s}RegistryValue' % ns, Root='HKCU', Key='Software\\DriveBridge\\Installer', Name='DesktopShortcut', Type='integer', Value='1', KeyPath='yes')
            ET.SubElement(group, '{%s}ComponentRef' % ns, Id='DesktopShortcut')
        first = tree.find('.//{%s}Component' % ns)
        if first is None:
            raise ValueError('empty installer payload')
        for i, directory_id in enumerate(['ProgramsFolder'] + [d.get('Id') for d in dirs.values()]):
            ET.SubElement(first, '{%s}RemoveFolder' % ns, Id='R%d' % i, Directory=directory_id, On='uninstall')
        ET.ElementTree(tree).write(out, encoding='utf-8', xml_declaration=True)
    else:
        if args.template == 'windows.iss' and (root / 'bin/drivebridge-desktop.exe').is_file():
            template = template.replace('[Registry]', '[Icons]\nName: "{userdesktop}\\DriveBridge"; Filename: "{app}\\bin\\drivebridge-desktop.exe"; WorkingDir: "{app}"\n\n[Registry]')
        for key, value in replacements.items():
            # All platform template parameters are paths/version under build control, not shell commands.
            template = template.replace(key, value)
        out.write_text(template)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    sub = parser.add_subparsers(dest='action', required=True)
    pre = sub.add_parser('preflight')
    pre.add_argument('--tools', nargs='*', default=[])
    for name in ('preflight', 'stage', 'manifest'):
        p = pre if name == 'preflight' else sub.add_parser(name)
        p.add_argument('--platform', required=True, choices=['darwin', 'linux', 'windows'])
        p.add_argument('--arch', required=True, choices=['amd64', 'arm64'])
        p.add_argument('--cli-only', action='store_true')
        p.add_argument('--desktop', action='store_true')
        p.add_argument('--unsigned', action='store_true')
        if name == 'stage':
            for key in ('root', 'repo', 'binary', 'lark-package', 'lark-native-archive'):
                p.add_argument('--' + key, required=True)
            p.add_argument('--session-host-binary')
            p.add_argument('--session-host-licenses')
            p.add_argument('--desktop-binary')
            p.add_argument('--version', default='0.0.0')
            p.add_argument('--node-archive')
            p.add_argument('--consume-binary', action='store_true')
            p.add_argument('--signed-cli')
            p.add_argument('--cli-upstream-sha')
        if name == 'manifest':
            for key in ('root', 'version', 'tools'):
                p.add_argument('--' + key, required=True)
            p.add_argument('--signature', choices=['signed', 'unsigned_test'], required=True)
    ver = sub.add_parser('verify')
    ver.add_argument('--root', required=True)
    ver.add_argument('--release', action='store_true')
    ver.add_argument('--evidence')
    ver.add_argument('--native', action='store_true')
    rend = sub.add_parser('render')
    for key in ('root', 'output', 'template', 'version', 'arch'):
        rend.add_argument('--' + key, required=True)
    rend.add_argument('--cli-only', action='store_true')
    rend.add_argument('--desktop', action='store_true')
    prepare = sub.add_parser('prepare-cli')
    for key in ('archive', 'platform', 'arch', 'output'):
        prepare.add_argument('--' + key, required=True)
    digest = sub.add_parser('hash-file')
    digest.add_argument('--path', required=True)
    art = sub.add_parser('artifacts')
    art.add_argument('--dir', required=True)
    art.add_argument('--root', required=True)
    args = parser.parse_args()
    if args.action == 'preflight':
        check_host(args.platform, args.arch)
        check_mode(args.cli_only, args.unsigned, args.desktop)
        check_tools(args.tools)
    elif args.action == 'stage':
        stage(args)
    elif args.action == 'manifest':
        manifest(args)
    elif args.action == 'render':
        if not re.fullmatch(r'[0-9]+\.[0-9]+\.[0-9]+', args.version):
            raise ValueError('installer version must be numeric X.Y.Z')
        render(args)
    elif args.action == 'verify':
        root = Path(args.root)
        data = json.loads((root / 'manifest.json').read_text())
        verify_files(root, data)
        if args.release:
            check_release(data, json.loads(Path(args.evidence).read_text()) if args.evidence else None)
        elif args.native:
            check_native_evidence(data, json.loads(Path(args.evidence).read_text()) if args.evidence else None)
    elif args.action == 'prepare-cli':
        print(prepare_cli(args.archive, args.platform, args.arch, args.output))
    elif args.action == 'hash-file':
        print(sha(args.path))
    elif args.action == 'artifacts':
        # External checksum index avoids self-referential manifest/container hashes.
        out = Path(args.dir)
        data = {'schema_version': 1, 'payload_manifest_sha256': sha(Path(args.root) / 'manifest.json'),
                'artifacts': [{'path': p.name, 'sha256': sha(p), 'size': p.stat().st_size}
                              for p in sorted(out.iterdir()) if p.is_file() and p.name != 'artifacts.json']}
        (out / 'artifacts.json').write_text(json.dumps(data, indent=2) + '\n')


if __name__ == '__main__':
    try:
        main()
    except (ValueError, OSError, KeyError, subprocess.CalledProcessError, json.JSONDecodeError) as exc:
        print('packaging error: ' + str(exc), file=sys.stderr)
        sys.exit(1)
