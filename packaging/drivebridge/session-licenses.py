#!/usr/bin/env python3
"""Collect license texts from the exact restored Windows session dependencies."""
import argparse
import json
from pathlib import Path
import re

PACKAGES = {
    'microsoft.netcore.app.runtime.win-x64': True,
    # WindowsDesktop runtime pack ships a single MIT LICENSE; WebView2 also
    # ships a NOTICE.txt which must accompany its redistributable SDK binaries.
    'microsoft.windowsdesktop.app.runtime.win-x64': False,
    'microsoft.web.webview2': True,
}


def package_versions(assets):
    versions = {}
    for key in assets.get('libraries', {}):
        name, sep, version = key.rpartition('/')
        if sep and name.lower() in PACKAGES:
            versions[name.lower()] = version

    def visit(value):
        if isinstance(value, dict):
            for dependency in value.get('downloadDependencies', []):
                name = dependency.get('name', '').lower()
                if name in PACKAGES:
                    version = dependency.get('version', '')
                    match = re.fullmatch(r'\[([^,]+),\s*\1\]', version)
                    if not match:
                        raise ValueError('dependency version must be exact: ' + name)
                    versions[name] = match[1]
            for child in value.values():
                visit(child)
        elif isinstance(value, list):
            for child in value:
                visit(child)
    visit(assets.get('project', {}))
    if versions.keys() != PACKAGES.keys():
        raise ValueError('missing exact native login dependency: ' + ', '.join(sorted(PACKAGES.keys() - versions.keys())))
    return versions


def collect(assets_path, output):
    assets = json.loads(Path(assets_path).read_text(encoding='utf-8-sig'))
    folders = [Path(folder) for folder in assets.get('packageFolders', {})]
    if not folders:
        raise ValueError('restored NuGet package folders are missing')
    sections = ['DriveBridge native login — redistributed dependency licenses\n']
    for name, version in sorted(package_versions(assets).items()):
        candidates = [folder / name / version for folder in folders]
        package = next((path for path in candidates if path.is_dir() and not path.is_symlink()), None)
        if package is None:
            raise ValueError('restored package is missing: ' + name + '/' + version)
        files = {file.name.lower(): file for file in package.iterdir() if file.is_file() and not file.is_symlink()}
        license_file = next((files[key] for key in ('license.txt', 'license', 'license.md') if key in files), None)
        notice_file = next((files[key] for key in ('third-party-notices.txt', 'thirdpartynotices.txt', 'thirdpartynotices', 'notice.txt') if key in files), None)
        if license_file is None or PACKAGES[name] and notice_file is None:
            raise ValueError('dependency license/notice is missing: ' + name)
        for file in (license_file, notice_file):
            if file is None:
                continue
            text = file.read_text(encoding='utf-8-sig')
            if not text.strip():
                raise ValueError('dependency license/notice is empty: ' + name)
            sections.append('\n=== ' + name + '/' + version + ': ' + file.name + ' ===\n\n' + text + '\n')
    output = Path(output)
    output.parent.mkdir(parents=True, exist_ok=True)
    output.write_text(''.join(sections), encoding='utf-8')


if __name__ == '__main__':
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--assets', required=True)
    parser.add_argument('--output', required=True)
    args = parser.parse_args()
    collect(args.assets, args.output)
