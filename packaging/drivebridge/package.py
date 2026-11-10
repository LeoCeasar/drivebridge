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
import zipfile
import xml.etree.ElementTree as ET

HERE = Path(__file__).resolve().parent
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
    return subprocess.check_output([str(a) for a in args], stderr=subprocess.STDOUT, text=True).strip()


def check_host(target, arch, system=None, machine=None):
    host = {'Darwin': 'darwin', 'Linux': 'linux', 'Windows': 'windows'}.get(system or platform.system())
    cpu = {'arm64': 'arm64', 'aarch64': 'arm64', 'x86_64': 'amd64', 'AMD64': 'amd64'}.get(machine or platform.machine())
    if host != target or cpu != arch or (target == 'windows' and arch != 'amd64'):
        raise ValueError('native OS/architecture runner required: %s/%s (host %s/%s)' % (target, arch, host, cpu))


def check_tools(names):
    for name in names:
        if not shutil.which(name):
            raise ValueError('missing tool: ' + name)


def check_mode(cli_only, unsigned):
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


def required_files(target):
    suffix = '.exe' if target == 'windows' else ''
    files = {'bin/drivebridge' + suffix, 'libexec/lark/lark-cli' + suffix,
             'licenses/rclone.COPYING', 'licenses/lark-cli.LICENSE',
             'licenses/NOTICE.md', 'licenses/THIRD_PARTY_NOTICES.md', 'README.md'}
    if target == 'windows':
        files |= {'libexec/node/node.exe', 'licenses/node.LICENSE',
                  'libexec/lark/package/package.json', 'libexec/lark/package/LICENSE',
                  'libexec/lark/package/checksums.txt', 'libexec/lark/package/scripts/run.js',
                  'libexec/lark/package/scripts/install.js', 'libexec/lark/package/scripts/install-wizard.js'}
    return files


def validate_payload(root, target):
    root = Path(root)
    required = required_files(target)
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
        if path.suffix in ('.plist', '.service', '.task') or 'startup' in path.name.lower():
            raise ValueError('duplicate/unmanaged startup item: ' + rel)
        allowed_runtime = target == 'windows' and rel == 'libexec/lark/package/bin/lark-cli.exe'
        if rel not in required and not allowed_runtime:
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
                    example = rel in ('bin/drivebridge', 'bin/drivebridge.exe') and any(
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


def sha(path):
    h = hashlib.sha256()
    with Path(path).open('rb') as f:
        for part in iter(lambda: f.read(1024 * 1024), b''):
            h.update(part)
    return h.hexdigest()


def inventory(root):
    return [{'path': p.relative_to(root).as_posix(), 'size': p.stat().st_size, 'sha256': sha(p)}
            for p in sorted(Path(root).rglob('*')) if p.is_file() and p.name != 'manifest.json']


def payload_digest(files):
    return hashlib.sha256(json.dumps(files, sort_keys=True, separators=(',', ':')).encode()).hexdigest()


def verify_files(root, manifest):
    validate_payload(root, manifest['platform'])
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
    for gate in NATIVE_GATES:
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


def stage(args):
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
    ext = '.zip' if args.platform == 'windows' else '.tar.gz'
    asset = 'lark-cli-%s-%s-%s%s' % (pin['cli']['version'], args.platform, args.arch, ext)
    check_binary(args.binary, args.platform, args.arch)
    check_commands(args.binary, args.platform, args.cli_only)
    native = root / ('libexec/lark/lark-cli' + suffix)
    archive_file(args.lark_native_archive, pin['cli']['assets'][asset]['sha256'], 'lark-cli' + suffix, native)
    check_binary(native, args.platform, args.arch)
    copy_file(args.binary, root / ('bin/drivebridge' + suffix))
    copy_file(package / 'LICENSE', root / 'licenses/lark-cli.LICENSE')
    for src, dst in [('COPYING', 'rclone.COPYING'), ('NOTICE.md', 'NOTICE.md'),
                     ('THIRD_PARTY_NOTICES.md', 'THIRD_PARTY_NOTICES.md')]:
        copy_file(repo / src, root / 'licenses' / dst)
    copy_file(HERE / 'BUNDLE-README.md', root / 'README.md')
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
    validate_payload(root, args.platform)


def manifest(args):
    root = Path(args.root)
    validate_payload(root, args.platform)
    tools = json.loads(Path(args.tools).read_text())
    if not tools or any(not value for value in tools.values()):
        raise ValueError('tool versions must be recorded')
    lock = json.loads((HERE / 'runtime-lock.json').read_text())
    manifest = {'schema_version': 1, 'product': 'DriveBridge', 'version': args.version,
                'platform': args.platform, 'architecture': args.arch,
                'go_version': run(['go', 'version']), 'tools': tools,
                'runtime': lock, 'vendor_metadata': {'embedded_ci_prefixes': [p.decode() for p in VENDOR_CI_PREFIXES], 'policy': 'Only checksum-verified vendor executables may retain upstream CI source paths; bytes are never rewritten.', 'product_help_examples': [p.decode() for p in PRODUCT_HELP_EXAMPLES]}, 'mount': not args.cli_only,
                'mount_engine': 'none' if args.cli_only else ('mount2' if args.platform == 'linux' else 'cmount'),
                'signature': args.signature, 'upgrade_guard': 'process_check_only', 'native_verification': 'not_verified',
                'release_blockers': ['native installation, upgrade, driver and mount evidence required', 'installer lifecycle lease not integrated; process check is insufficient'],
                'licenses': sorted(p.relative_to(root).as_posix() for p in (root / 'licenses').iterdir()),
                'files': inventory(root)}
    if args.signature == 'unsigned_test':
        manifest['release_blockers'].append('unsigned test artifact')
    if args.cli_only:
        manifest['release_blockers'].append('CLI-only; writable mount unavailable')
    (root / 'manifest.json').write_text(json.dumps(manifest, indent=2) + '\n')


def render(args):
    # Generate one component per whitelisted file. Stable UUIDs are generated by WiX from paths.
    root, out = Path(args.root).resolve(), Path(args.output)
    template = (HERE / 'templates' / args.template).read_text()
    replacements = {'@VERSION@': args.version, '@ARCH@': ('x86_64' if args.template == 'macos-distribution.xml' and args.arch == 'amd64' else args.arch), '@PAYLOAD@': str(root), '@OUTPUT@': str(out.parent.resolve())}
    if args.template == 'windows.wxs':
        ns = 'http://wixtoolset.org/schemas/v4/wxs'
        ET.register_namespace('', ns)
        tree = ET.fromstring(template.replace('@VERSION@', args.version))
        directory = tree.find('.//{%s}Directory[@Id="INSTALLFOLDER"]' % ns)
        group = tree.find('.//{%s}ComponentGroup' % ns)
        dirs = {'': directory}
        for i, path in enumerate(sorted(p for p in root.rglob('*') if p.is_file())):
            rel = path.relative_to(root)
            current = ''
            for part in rel.parts[:-1]:
                parent = current
                current = current + '/' + part
                if current not in dirs:
                    dirs[current] = ET.SubElement(dirs[parent], '{%s}Directory' % ns, Id='D' + hashlib.sha256(current.encode()).hexdigest()[:16], Name=part)
            suffix = hashlib.sha256(rel.as_posix().encode()).hexdigest()[:16]
            component = ET.SubElement(dirs[current], '{%s}Component' % ns, Id='C' + suffix, Guid='*')
            ET.SubElement(component, '{%s}File' % ns, Id='F' + suffix, Source=str(path))
            ET.SubElement(component, '{%s}RegistryValue' % ns, Root='HKCU', Key='Software\\DriveBridge\\Installer\\Files', Name='F' + suffix, Type='string', Value='[#F%s]' % suffix, KeyPath='yes')
            ET.SubElement(group, '{%s}ComponentRef' % ns, Id='C' + suffix)
        first = tree.find('.//{%s}Component' % ns)
        if first is None:
            raise ValueError('empty installer payload')
        for i, directory_id in enumerate(['ProgramsFolder'] + [d.get('Id') for d in dirs.values()]):
            ET.SubElement(first, '{%s}RemoveFolder' % ns, Id='R%d' % i, Directory=directory_id, On='uninstall')
        out.write_text(ET.tostring(tree, encoding='unicode'))
    else:
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
        p.add_argument('--unsigned', action='store_true')
        if name == 'stage':
            for key in ('root', 'repo', 'binary', 'lark-package', 'lark-native-archive'):
                p.add_argument('--' + key, required=True)
            p.add_argument('--node-archive')
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
    art = sub.add_parser('artifacts')
    art.add_argument('--dir', required=True)
    art.add_argument('--root', required=True)
    args = parser.parse_args()
    if args.action == 'preflight':
        check_host(args.platform, args.arch)
        check_mode(args.cli_only, args.unsigned)
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
