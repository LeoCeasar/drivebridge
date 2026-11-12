import importlib.util
import json
import argparse
import hashlib
import zipfile
import xml.etree.ElementTree as ET
import pathlib
import tempfile
import subprocess
import os
import unittest
from unittest import mock

SPEC = importlib.util.spec_from_file_location('package', pathlib.Path(__file__).parents[1] / 'package.py')
p = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(p)


class PolicyTests(unittest.TestCase):
    def test_native_arch_required(self):
        with self.assertRaisesRegex(ValueError, 'native'):
            p.check_host('darwin', 'amd64', system='Darwin', machine='arm64')
        p.check_host('linux', 'arm64', system='Linux', machine='aarch64')

    def test_missing_tool_fails(self):
        with mock.patch('shutil.which', return_value=None):
            with self.assertRaisesRegex(ValueError, 'missing tool: pkgbuild'):
                p.check_tools(['pkgbuild'])

    def test_cli_only_is_never_release(self):
        with self.assertRaisesRegex(ValueError, 'CLI-only'):
            p.check_mode(cli_only=True, unsigned=False)

    def test_missing_mount_command_fails(self):
        with mock.patch.object(p, 'run', return_value='Usage: bridge [command]\n  feishu Managed commands\n  version Version'):
            with self.assertRaisesRegex(ValueError, 'mount2'):
                p.check_commands('bridge', 'linux', False)

    def test_schema_matches_payload_manifest(self):
        schema = json.loads((pathlib.Path(__file__).parents[1] / 'manifest.schema.json').read_text())
        self.assertIn('files', schema['required'])
        self.assertIn('release_blockers', schema['required'])

    def test_payload_allowlist_licenses_and_private_paths(self):
        with tempfile.TemporaryDirectory() as d:
            root = pathlib.Path(d)
            for name in p.required_files('linux'):
                dest = root / name
                dest.parent.mkdir(parents=True, exist_ok=True)
                dest.write_bytes(b'fixture')
            p.validate_payload(root, 'linux')
            bad = root / 'account.json'
            bad.write_text('{}')
            with self.assertRaisesRegex(ValueError, 'unapproved'):
                p.validate_payload(root, 'linux')
            bad.unlink()
            notice = root / 'licenses/NOTICE.md'
            notice.write_text('built at /Users/private/developer')
            with self.assertRaisesRegex(ValueError, 'developer path'):
                p.validate_payload(root, 'linux')
            notice.write_text('notice')
            (root / 'licenses/lark-cli.LICENSE').unlink()
            with self.assertRaisesRegex(ValueError, 'missing payload'):
                p.validate_payload(root, 'linux')

    def test_symlink_and_duplicate_startup_fail(self):
        with tempfile.TemporaryDirectory() as d:
            root = pathlib.Path(d)
            for name in p.required_files('linux'):
                dest = root / name
                dest.parent.mkdir(parents=True, exist_ok=True)
                dest.write_text('fixture')
            (root / 'licenses/link').symlink_to('/etc/passwd')
            with self.assertRaisesRegex(ValueError, 'symlink'):
                p.validate_payload(root, 'linux')
            (root / 'licenses/link').unlink()
            (root / 'startup.service').write_text('fixture')
            with self.assertRaisesRegex(ValueError, 'startup'):
                p.validate_payload(root, 'linux')

    def test_windows_requires_node_license_and_package(self):
        files = p.required_files('windows')
        self.assertIn('libexec/node/node.exe', files)
        self.assertIn('licenses/node.LICENSE', files)
        self.assertIn('libexec/lark/package/package.json', files)

    def test_binary_arch_validated_without_executing_input(self):
        with tempfile.TemporaryDirectory() as d:
            binary = pathlib.Path(d) / 'exe'
            binary.write_bytes(b'\x7fELF\x02\x01' + b'\0' * 12 + b'\xb7\x00' + b'\0' * 50)
            p.check_binary(binary, 'linux', 'arm64')
            with self.assertRaisesRegex(ValueError, 'architecture'):
                p.check_binary(binary, 'linux', 'amd64')

    def test_release_requires_bound_native_evidence(self):
        manifest = {'platform': 'linux', 'architecture': 'amd64', 'mount': True,
                    'signature': 'unsigned_test', 'files': [], 'upgrade_guard': 'lifecycle_lease'}
        with self.assertRaisesRegex(ValueError, 'unsigned'):
            p.check_release(manifest, None)
        manifest['signature'] = 'signed'
        with self.assertRaisesRegex(ValueError, 'evidence'):
            p.check_release(manifest, None)
        evidence = {'platform': 'linux', 'architecture': 'amd64', 'payload_sha256': p.payload_digest([]),
                    'checks': {name: 'passed' for name in p.NATIVE_GATES}, 'runner': 'fixture-native'}
        p.check_release(manifest, evidence)
        evidence['checks']['upgrade_preserves_pending'] = 'not_verified'
        with self.assertRaisesRegex(ValueError, 'upgrade_preserves_pending'):
            p.check_release(manifest, evidence)

    def test_manifest_rejects_tamper_and_unlisted_extra(self):
        with tempfile.TemporaryDirectory() as d:
            root = pathlib.Path(d)
            for name in p.required_files('linux'):
                dest = root / name
                dest.parent.mkdir(parents=True, exist_ok=True)
                dest.write_text('fixture')
            manifest = {'platform': 'linux', 'files': p.inventory(root)}
            p.verify_files(root, manifest)
            (root / 'README.md').write_text('changed')
            with self.assertRaisesRegex(ValueError, 'inventory'):
                p.verify_files(root, manifest)

    def test_archive_hash_is_checked_before_extraction(self):
        with tempfile.TemporaryDirectory() as d:
            root = pathlib.Path(d)
            archive = root / 'runtime.zip'
            with zipfile.ZipFile(archive, 'w') as z:
                z.writestr('lark-cli.exe', b'expected native bytes')
                z.writestr('../escape', b'never extracted')
            with self.assertRaisesRegex(ValueError, 'checksum'):
                p.archive_file(archive, '0' * 64, 'lark-cli.exe', root / 'out')
            self.assertFalse((root / 'out').exists())
            p.archive_file(archive, p.sha(archive), 'lark-cli.exe', root / 'out')
            self.assertEqual((root / 'out').read_bytes(), b'expected native bytes')
            self.assertFalse((root.parent / 'escape').exists())

    def test_native_evidence_cannot_cross_arch_or_payload(self):
        m = {'platform': 'darwin', 'architecture': 'arm64', 'mount': True, 'files': []}
        evidence = {'platform': 'darwin', 'architecture': 'amd64', 'runner': 'native',
                    'payload_sha256': p.payload_digest([]), 'checks': {n: 'passed' for n in p.NATIVE_GATES}}
        with self.assertRaisesRegex(ValueError, 'architecture mismatch'):
            p.check_native_evidence(m, evidence)
        evidence['architecture'] = 'arm64'
        evidence['payload_sha256'] = '0' * 64
        with self.assertRaisesRegex(ValueError, 'exact payload'):
            p.check_native_evidence(m, evidence)

    def test_windows_render_stays_per_user_with_registry_keypaths(self):
        with tempfile.TemporaryDirectory() as d:
            root = pathlib.Path(d) / 'payload'
            (root / 'bin').mkdir(parents=True)
            (root / 'bin/drivebridge.exe').write_bytes(b'fixture')
            output = pathlib.Path(d) / 'out.wxs'
            p.render(argparse.Namespace(root=str(root), output=str(output), template='windows.wxs', version='1.2.3', arch='amd64'))
            tree = ET.parse(output)
            ns = {'w': 'http://wixtoolset.org/schemas/v4/wxs'}
            self.assertEqual(tree.find('w:Package', ns).get('Scope'), 'perUser')
            key = tree.find('.//w:RegistryValue', ns)
            self.assertEqual((key.get('Root'), key.get('KeyPath')), ('HKCU', 'yes'))
            component = tree.find('.//w:Component', ns)
            self.assertRegex(component.get('Guid'), r'^[a-f0-9-]{36}$')
            p.render(argparse.Namespace(root=str(root), output=str(output), template='windows.wxs', version='1.2.4', arch='amd64'))
            self.assertEqual(ET.parse(output).find('.//w:Component', ns).get('Guid'), component.get('Guid'))
            self.assertIsNotNone(tree.find('.//w:RemoveFolder', ns))
            guard = tree.find('.//w:CustomAction', ns)
            self.assertEqual(guard.get('Impersonate'), 'yes')
            self.assertNotIn('SYSTEM', output.read_text())

    def test_lifecycle_wrapper_is_whitelisted_and_native_guards_are_embedded(self):
        self.assertIn('upgrade.sh', p.required_files('darwin'))
        self.assertIn('upgrade.ps1', p.required_files('windows'))
        for name in ('process_check_only', 'lifecycle_lease_unverified'):
            with self.assertRaisesRegex(ValueError, 'lifecycle lease'):
                p.check_release({'signature': 'signed', 'upgrade_guard': name}, None)
        template = (p.HERE / 'templates/windows.wxs').read_text()
        self.assertIn('BinaryRef="LeaseVerifier"', template)
        self.assertNotIn('powershell', template.lower())
        self.assertIn('feishu maintenance verify', (p.HERE / 'templates/macos-preinstall').read_text())
        self.assertIn('dontcopy noencryption', (p.HERE / 'templates/windows.iss').read_text())
        self.assertIn('exit 1', (p.HERE / 'templates/linux-preinstall').read_text())

    def test_wrapper_copies_helper_and_preserves_literal_argv(self):
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory)
            candidate = root / 'candidate'
            record = root / 'record'
            candidate.write_text('#!/bin/sh\nprintf "%s\\n" "$0" "$@" > "$DB_WRAPPER_RECORD"\n')
            candidate.chmod(0o700)
            literal = '$(never execute); one argument'
            env = dict(os.environ, DB_WRAPPER_RECORD=str(record))
            subprocess.run(['sh', str(p.HERE / 'upgrade.sh'), str(candidate), '--', '/synthetic/installer', literal], check=True, env=env)
            values = record.read_text().splitlines()
            self.assertNotEqual(values[0], str(candidate))
            self.assertFalse(pathlib.Path(values[0]).exists())
            self.assertEqual(values[1:], ['feishu', 'maintenance', 'exec', '--', '/synthetic/installer', literal])

    def test_macos_home_domain_and_architecture(self):
        with tempfile.TemporaryDirectory() as d:
            output = pathlib.Path(d) / 'dist.xml'
            p.render(argparse.Namespace(root=d, output=str(output), template='macos-distribution.xml', version='1.2.3', arch='amd64'))
            tree = ET.parse(output)
            domains = tree.find('domains').attrib
            self.assertEqual(domains['enable_currentUserHome'], 'true')
            self.assertEqual(domains['enable_localSystem'], 'false')
            self.assertEqual(tree.find('options').get('hostArchitectures'), 'x86_64')

    def test_vendor_ci_allowlist_does_not_allow_our_build_or_private_paths(self):
        with tempfile.TemporaryDirectory() as d:
            root = pathlib.Path(d)
            for name in p.required_files('linux'):
                dest = root / name
                dest.parent.mkdir(parents=True, exist_ok=True)
                dest.write_text('fixture')
            vendor = root / 'libexec/lark/lark-cli'
            vendor.write_bytes(b'/home/runner/go/pkg/mod/github.com/test/file.go')
            p.validate_payload(root, 'linux')
            (root / 'bin/drivebridge').write_bytes(vendor.read_bytes())
            with self.assertRaisesRegex(ValueError, 'developer path'):
                p.validate_payload(root, 'linux')
            (root / 'bin/drivebridge').write_text('fixture')
            vendor.write_bytes(b'/Users/private-developer/project/main.go')
            with self.assertRaisesRegex(ValueError, 'developer path'):
                p.validate_payload(root, 'linux')

    def test_process_check_only_cannot_qualify_for_release(self):
        manifest = {'platform': 'linux', 'architecture': 'amd64', 'mount': True,
                    'signature': 'signed', 'files': [], 'upgrade_guard': 'process_check_only'}
        evidence = {'platform': 'linux', 'architecture': 'amd64', 'payload_sha256': p.payload_digest([]),
                    'checks': {name: 'passed' for name in p.NATIVE_GATES}, 'runner': 'fixture-native'}
        with self.assertRaisesRegex(ValueError, 'lifecycle lease'):
            p.check_release(manifest, evidence)

    def test_public_help_sample_is_exact_not_a_developer_prefix(self):
        with tempfile.TemporaryDirectory() as d:
            root = pathlib.Path(d)
            for name in p.required_files('linux'):
                dest = root / name
                dest.parent.mkdir(parents=True, exist_ok=True)
                dest.write_text('fixture')
            binary = root / 'bin/drivebridge'
            binary.write_bytes(b'Example: /home/<user>/mountPoint\n')
            p.validate_payload(root, 'linux')
            binary.write_bytes(b'/home/<user>/mountPoint/private-source/main.go')
            with self.assertRaisesRegex(ValueError, 'developer path'):
                p.validate_payload(root, 'linux')

    def test_linux_cli_only_package_does_not_require_driver(self):
        with tempfile.TemporaryDirectory() as d:
            out = pathlib.Path(d) / 'control'
            args = argparse.Namespace(root=d, output=str(out), template='deb-control', version='1.2.3', arch='arm64', cli_only=True)
            p.render(args)
            self.assertIn('Depends: procps', out.read_text())
            self.assertNotIn('fuse3', out.read_text())
            args.cli_only = False
            p.render(args)
            self.assertIn('Depends: fuse3, procps', out.read_text())

    def test_pre_sign_source_binding_checks_native_hash_not_only_archive(self):
        with tempfile.TemporaryDirectory() as d:
            root = pathlib.Path(d)
            binary = b'\x7fELF\x02\x01' + b'\0' * 12 + b'\x3e\x00' + b'\0' * 50
            archive = root / 'native.zip'
            with zipfile.ZipFile(archive, 'w') as z:
                z.writestr('lark-cli', binary)
            asset = {'sha256': p.sha(archive), 'native_sha256': '0' * 64}
            lock = {'cli': {'version': '1.0.99', 'assets': {'lark-cli-1.0.99-linux-amd64.tar.gz': asset}}}
            (root / 'runtime-lock.json').write_text(json.dumps(lock))
            with mock.patch.object(p, 'HERE', root):
                with self.assertRaisesRegex(ValueError, 'official upstream hash'):
                    p.prepare_cli(archive, 'linux', 'amd64', root / 'out')
                asset['native_sha256'] = hashlib.sha256(binary).hexdigest()
                (root / 'runtime-lock.json').write_text(json.dumps(lock))
                actual = p.prepare_cli(archive, 'linux', 'amd64', root / 'out')
                self.assertEqual(actual, asset['native_sha256'])


if __name__ == '__main__':
    unittest.main()
