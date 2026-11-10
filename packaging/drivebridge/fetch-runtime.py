#!/usr/bin/env python3
"""Fetch only pinned public release inputs; never run npm hooks or install a runtime."""
import argparse
import base64
import hashlib
import json
from pathlib import Path
import sys
import tarfile
import urllib.request

HERE = Path(__file__).resolve().parent


def fetch(url, path, digest, algorithm='sha256'):
    path = Path(path)
    if not url.startswith('https://'):
        raise ValueError('HTTPS source required')
    h = hashlib.new(algorithm)
    with urllib.request.urlopen(url, timeout=120) as response, path.open('xb') as out:
        while True:
            block = response.read(1024 * 1024)
            if not block:
                break
            h.update(block)
            out.write(block)
    actual = h.hexdigest() if algorithm == 'sha256' else base64.b64encode(h.digest()).decode()
    if actual != digest:
        path.unlink()
        raise ValueError('runtime checksum mismatch: ' + path.name)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--platform', choices=['darwin', 'linux', 'windows'], required=True)
    parser.add_argument('--arch', choices=['amd64', 'arm64'], required=True)
    parser.add_argument('--output', required=True)
    args = parser.parse_args()
    lock = json.loads((HERE / 'runtime-lock.json').read_text())
    output = Path(args.output)
    output.mkdir(parents=True, exist_ok=False)
    cli = lock['cli']
    algorithm, digest = cli['integrity'].split('-', 1)
    fetch(cli['source'], output / 'cli-package.tgz', digest, algorithm)
    package = output / 'lark-package'
    package.mkdir()
    with tarfile.open(output / 'cli-package.tgz') as archive:
        for name, digest in cli['package_files'].items():
            member = archive.getmember('package/' + name)
            if not member.isfile() or Path(name).is_absolute() or '..' in Path(name).parts:
                raise ValueError('invalid package member')
            data = archive.extractfile(member).read()
            if hashlib.sha256(data).hexdigest() != digest:
                raise ValueError('package member checksum mismatch')
            target = package / name
            target.parent.mkdir(parents=True, exist_ok=True)
            target.write_bytes(data)
    ext = '.zip' if args.platform == 'windows' else '.tar.gz'
    name = 'lark-cli-%s-%s-%s%s' % (cli['version'], args.platform, args.arch, ext)
    asset = cli['assets'][name]
    fetch(asset['source'], output / ('cli-native' + ext), asset['sha256'])
    if args.platform == 'windows':
        fetch(lock['node']['source'], output / 'node.zip', lock['node']['sha256'])
    print('Pinned runtime archives verified; no install hooks or account commands executed.')


if __name__ == '__main__':
    try:
        main()
    except Exception as exc:
        print('runtime fetch failed: ' + str(exc), file=sys.stderr)
        sys.exit(1)
