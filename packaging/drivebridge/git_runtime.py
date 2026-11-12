#!/usr/bin/env python3
"""Build/stage a private Git plumbing runtime; never install a system package."""
import argparse
import hashlib
import json
import os
from pathlib import Path, PurePosixPath
import platform
import shutil
import subprocess
import tarfile
import tempfile
import urllib.request
import zipfile

HERE = Path(__file__).resolve().parent


def digest(path):
    h = hashlib.sha256()
    with Path(path).open('rb') as source:
        for part in iter(lambda: source.read(1024 * 1024), b''):
            h.update(part)
    return h.hexdigest()


def safe_name(name):
    p = PurePosixPath(name)
    if not name or p.is_absolute() or '..' in p.parts or '\\' in name or p.as_posix() != name:
        raise ValueError('unsafe Git runtime path')
    return p


def download(pin, dest):
    with urllib.request.urlopen(pin['url'], timeout=60) as source, Path(dest).open('wb') as output:
        shutil.copyfileobj(source, output)
    if digest(dest) != pin['sha256']:
        raise ValueError('Git upstream checksum mismatch')


def inventory(root):
    result = {}
    for p in sorted(Path(root).rglob('*')):
        if p.is_symlink():
            raise ValueError('symlink in Git runtime')
        if p.is_file() and p.name != 'runtime.json':
            result[p.relative_to(root).as_posix()] = digest(p)
    return result


def verify(root, target=None, arch=None):
    root = Path(root)
    if root.is_symlink() or (root / 'runtime.json').is_symlink():
        raise ValueError('symlink in Git runtime')
    metadata = json.loads((root / 'runtime.json').read_text())
    if metadata.get('schema_version') != 1 or target and metadata.get('platform') != target or arch and metadata.get('architecture') != arch:
        raise ValueError('Git runtime target mismatch')
    if not isinstance(metadata.get('files'), dict) or len(metadata['files']) > 10000:
        raise ValueError('invalid Git runtime inventory')
    for name, h in metadata['files'].items():
        safe_name(name)
        if not isinstance(h, str) or len(h) != 64:
            raise ValueError('invalid Git runtime digest')
    lock = json.loads((HERE / 'git-runtime-lock.json').read_text())
    if metadata.get('version') != lock['version']:
        raise ValueError('Git runtime version differs from lock')
    suffix = '.exe' if metadata['platform'] == 'windows' else ''
    required = {'bin/git' + suffix, 'SOURCE.json'}
    if metadata['platform'] == 'windows':
        required |= {'bin/' + name for name in lock['windows'][metadata['architecture']].get('runtime_dlls', [])}
    if not required <= metadata['files'].keys() or not any(p.startswith('licenses/') for p in metadata['files']):
        raise ValueError('Git runtime or license missing')
    if inventory(root) != metadata['files']:
        raise ValueError('Git runtime inventory mismatch')
    return metadata


def stage(source, destination, target, arch):
    metadata = verify(source, target, arch)
    if Path(destination).exists():
        raise ValueError('Git staging destination exists')
    shutil.copytree(source, destination)
    verify(destination, target, arch)
    return metadata


def build(args):
    if Path(args.output).exists():
        raise ValueError('Git runtime output exists')
    lock = json.loads((HERE / 'git-runtime-lock.json').read_text())
    out = Path(args.output).resolve()
    out.parent.mkdir(parents=True, exist_ok=True)
    with tempfile.TemporaryDirectory(prefix='drivebridge-git-', dir=out.parent) as temp:
        work = Path(temp)
        result = work / 'runtime'
        (result / 'bin').mkdir(parents=True)
        (result / 'licenses').mkdir()
        if args.platform == 'windows':
            pin = lock['windows'][args.architecture]
            dlls = pin.get('runtime_dlls')
            archive = work / 'mingit.zip'
            download(pin, archive)
            with zipfile.ZipFile(archive) as z:
                # Pinned recent MinGit releases use UCRT; older archives used
                # mingw64. Use the real executable, never the cmd/ wrapper.
                roots = [p for p in ('ucrt64', 'mingw64', 'clangarm64') if p + '/bin/git.exe' in z.namelist()]
                if len(roots) != 1:
                    raise ValueError('ambiguous or missing native Git runtime directory')
                bindir = roots[0] + '/bin/'
                licenses = roots[0] + '/share/licenses/'
                for info in z.infolist():
                    safe_name(info.filename.rstrip('/'))
                    name = info.filename
                    if name == bindir + 'git.exe' or name.startswith(bindir) and name.endswith('.dll') and (dlls is None or PurePosixPath(name).name in dlls):
                        dest = result / 'bin' / PurePosixPath(name).name
                    elif name.startswith(licenses) and not info.is_dir():
                        dest = result / 'licenses' / name.removeprefix(licenses)
                    elif name == 'LICENSE.txt':
                        dest = result / 'licenses/git-for-windows.LICENSE.txt'
                    else:
                        continue
                    dest.parent.mkdir(parents=True, exist_ok=True)
                    dest.write_bytes(z.read(info))
            source = {'upstream': pin['url'], 'sha256': pin['sha256'], 'source': 'https://github.com/git-for-windows/git/tree/v' + lock['version'] + '.windows.1'}
        else:
            native = 'darwin' if platform.system() == 'Darwin' else 'linux'
            machine = 'arm64' if platform.machine().lower() in ('arm64', 'aarch64') else 'amd64'
            if native != args.platform or machine != args.architecture:
                raise ValueError('Build Git on a matching native runner')
            archive = work / 'git.tar.xz'
            download(lock['source'], archive)
            with tarfile.open(archive) as t:
                root = work / 'source'
                root.mkdir()
                for member in t.getmembers():
                    safe_name(member.name.rstrip('/'))
                    destination = (root / member.name).resolve()
                    if root.resolve() not in destination.parents:
                        raise ValueError('Git source archive escapes extraction root')
                    if not (member.isfile() or member.isdir() or member.issym() or member.islnk()):
                        raise ValueError('special file in Git source archive')
                    if member.issym() or member.islnk():
                        link = ((destination.parent if member.issym() else root) / member.linkname).resolve()
                        if root.resolve() not in link.parents:
                            raise ValueError('Git source link escapes extraction root')
                    t.extract(member, root)
            src = work / 'source' / ('git-' + lock['version'])
            flags = '-O2 -g0 -ffile-prefix-map=' + str(work) + '=/drivebridge/git-source'
            if args.platform == 'darwin':
                flags += ' -mmacosx-version-min=14.0'
            subprocess.run(['make', '-j2', 'git', 'NO_CURL=YesPlease', 'NO_EXPAT=YesPlease', 'NO_OPENSSL=YesPlease',
                            'NO_GETTEXT=YesPlease', 'NO_PERL=YesPlease', 'NO_PYTHON=YesPlease', 'NO_TCLTK=YesPlease',
                            'NO_RUST=YesPlease', 'prefix=/drivebridge/git', 'CFLAGS=' + flags], cwd=src, check=True)
            shutil.copy2(src / 'git', result / 'bin/git')
            shutil.copy2(src / 'COPYING', result / 'licenses/git.COPYING')
            (result / 'source').mkdir()
            shutil.copy2(archive, result / 'source' / ('git-' + lock['version'] + '.tar.xz'))
            source = lock['source']
            if args.codesign_identity:
                subprocess.run(['codesign', '--force', '--options', 'runtime', '--timestamp', '--sign', args.codesign_identity, str(result / 'bin/git')], check=True)
                subprocess.run(['codesign', '--verify', '--strict', str(result / 'bin/git')], check=True)
        (result / 'SOURCE.json').write_text(json.dumps(source, indent=2) + '\n')
        (result / 'runtime.json').write_text(json.dumps({'schema_version': 1, 'version': lock['version'], 'platform': args.platform,
                                                       'architecture': args.architecture, 'files': inventory(result)}, indent=2) + '\n')
        verify(result, args.platform, args.architecture)
        if args.platform != 'windows':
            subprocess.run([str(result / 'bin/git'), '--version'], check=True)
        shutil.move(str(result), out)
    print('Private Git runtime: ' + str(out))


def main():
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument('--platform', choices=['darwin', 'linux', 'windows'], required=True)
    p.add_argument('--architecture', choices=['amd64', 'arm64'], required=True)
    p.add_argument('--output', required=True)
    p.add_argument('--codesign-identity')
    build(p.parse_args())


if __name__ == '__main__':
    main()
