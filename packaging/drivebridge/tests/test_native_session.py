import importlib.util
import json
import pathlib
import plistlib
import re
import struct
import tempfile
import unittest
import xml.etree.ElementTree as ET
from unittest import mock

spec = importlib.util.spec_from_file_location('native_session_package', pathlib.Path(__file__).resolve().parents[1] / 'package.py')
p = importlib.util.module_from_spec(spec)
spec.loader.exec_module(p)


class NativeSessionPackagingTests(unittest.TestCase):
    def helper(self, root):
        path = root / 'session-helper'
        path.write_bytes(b'\xcf\xfa\xed\xfe' + struct.pack('<I', 0x0100000c) + b'\0' * 120)
        return path

    def complete_payload(self, root):
        for name in p.required_files('darwin'):
            dest = root / name
            dest.parent.mkdir(parents=True, exist_ok=True)
            dest.write_bytes(b'fixture')
        helper = self.helper(root.parent)
        p.stage_session_host(root, 'darwin', 'arm64', helper, '1.2.3')
        return helper

    def test_owned_login_payload_is_required_on_mac_and_windows(self):
        for target in ('darwin', 'linux', 'windows'):
            for desktop in (False, True):
                files = p.required_files(target, desktop)
                self.assertEqual(p.SESSION_BINARY in files, target == 'darwin')
                self.assertEqual(p.SESSION_PLIST in files, target == 'darwin')
                self.assertEqual(p.WINDOWS_SESSION_BINARY in files, target == 'windows')
                self.assertEqual(p.WINDOWS_SESSION_LICENSE in files, target == 'windows')
                self.assertFalse(any('browser-host' in name or 'browser-extension' in name for name in files))

    def test_stage_keeps_prototype_bundle_identity_and_minimum_os(self):
        with tempfile.TemporaryDirectory() as d:
            root = pathlib.Path(d)
            helper = self.helper(root)
            payload = root / 'payload'
            p.stage_session_host(payload, 'darwin', 'arm64', helper, '1.2.3')
            self.assertEqual((payload / p.SESSION_BINARY).read_bytes(), helper.read_bytes())
            info = plistlib.loads((payload / p.SESSION_PLIST).read_bytes())
            self.assertEqual(info['CFBundleIdentifier'], 'org.drivebridge.sessionhost')
            self.assertEqual(info['CFBundleExecutable'], 'drivebridge-session-host')
            self.assertEqual(info['LSMinimumSystemVersion'], '14.0')
            self.assertEqual(info['CFBundleVersion'], '1.2.3')
            self.assertIs(info['NSHighResolutionCapable'], True)
            self.assertIs(info['LSUIElement'], True)
            self.assertTrue((payload / p.SESSION_BINARY).stat().st_mode & 0o100)
            self.assertEqual({item['path'] for item in p.inventory(payload)}, {p.SESSION_BINARY, p.SESSION_PLIST})

    def test_helper_missing_wrong_arch_or_wrong_platform_fails(self):
        with tempfile.TemporaryDirectory() as d:
            root = pathlib.Path(d)
            helper = self.helper(root)
            with self.assertRaisesRegex(ValueError, 'requires native login helper'):
                p.stage_session_host(root / 'payload', 'darwin', 'arm64', None, '1.0.0')
            with self.assertRaisesRegex(ValueError, 'architecture'):
                p.stage_session_host(root / 'payload', 'darwin', 'amd64', helper, '1.0.0')
            with self.assertRaisesRegex(ValueError, 'requires native login helper'):
                p.stage_session_host(root / 'windows', 'windows', 'amd64', None, '1.0.0')
            for target in ('linux',):
                p.stage_session_host(root / target, target, 'amd64', None, '1.0.0')
                self.assertFalse((root / target).exists())
                with self.assertRaisesRegex(ValueError, 'unavailable'):
                    p.stage_session_host(root / target, target, 'amd64', helper, '1.0.0')

    def windows_helper(self, root, subsystem=2):
        data = bytearray(192)
        data[:2] = b'MZ'
        struct.pack_into('<I', data, 60, 64)
        data[64:68] = b'PE\0\0'
        struct.pack_into('<H', data, 68, 0x8664)
        struct.pack_into('<H', data, 64 + 92, subsystem)
        helper = root / 'drivebridge-session-host.exe'
        helper.write_bytes(data)
        return helper

    def test_windows_requires_gui_helper_bound_protocol_and_licenses(self):
        capabilities = {'protocol': 1, 'bundle_id': p.SESSION_BUNDLE_ID, 'persistent_per_account': True,
                        'operations': ['show', 'hide', 'fetch', 'complete', 'shutdown']}
        with tempfile.TemporaryDirectory() as d:
            root = pathlib.Path(d)
            helper = self.windows_helper(root)
            license_file = root / 'licenses'
            license_file.write_text('native login dependency license fixture')
            payload = root / 'payload'
            for name in p.required_files('windows'):
                dest = payload / name
                dest.parent.mkdir(parents=True, exist_ok=True)
                dest.write_bytes(b'fixture')
            with mock.patch.object(p, 'run', return_value=json.dumps(capabilities)) as run:
                p.stage_session_host(payload, 'windows', 'amd64', helper, '1.2.3', license_file)
                run.assert_called_once_with([helper, '--capabilities'])
            self.assertEqual((payload / p.WINDOWS_SESSION_BINARY).read_bytes(), helper.read_bytes())
            self.assertEqual((payload / p.WINDOWS_SESSION_LICENSE).read_text(), license_file.read_text())
            p.validate_payload(payload, 'windows', arch='amd64')
            # A console-subsystem login helper would flash a second application.
            self.windows_helper(root, subsystem=3)
            with self.assertRaisesRegex(ValueError, 'windowsgui'):
                p.stage_session_host(payload, 'windows', 'amd64', helper, '1.2.3', license_file)
            self.windows_helper(root)
            with mock.patch.object(p, 'run', return_value=json.dumps(dict(capabilities, bundle_id='foreign'))):
                with self.assertRaisesRegex(ValueError, 'identity/protocol'):
                    p.stage_session_host(payload, 'windows', 'amd64', helper, '1.2.3', license_file)
            (payload / 'libexec/profile.json').write_text('{}')
            with self.assertRaisesRegex(ValueError, 'unapproved'):
                p.validate_payload(payload, 'windows', arch='amd64')

    def test_verifier_rejects_bundle_identity_os_or_activation_tampering(self):
        with tempfile.TemporaryDirectory() as d:
            root = pathlib.Path(d) / 'payload'
            self.complete_payload(root)
            p.validate_payload(root, 'darwin', arch='arm64')
            original = plistlib.loads((root / p.SESSION_PLIST).read_bytes())
            for change in ({'CFBundleIdentifier': 'foreign.bundle'}, {'LSMinimumSystemVersion': '13.0'},
                           {'CFBundleExecutable': 'foreign'}, {'LSUIElement': False}, {'LSUIElement': 1}, {'LSBackgroundOnly': True}):
                info = dict(original, **change)
                (root / p.SESSION_PLIST).write_bytes(plistlib.dumps(info))
                with self.assertRaisesRegex(ValueError, 'identity/minimum OS/activation'):
                    p.validate_payload(root, 'darwin')
            (root / p.SESSION_PLIST).write_bytes(plistlib.dumps(original))
            signature = root / p.SESSION_APP / 'Contents/_CodeSignature/CodeResources'
            signature.parent.mkdir()
            signature.write_bytes(b'signature fixture')
            p.validate_payload(root, 'darwin')
            secret = root / p.SESSION_APP / 'Contents/Cookies'
            secret.write_bytes(b'not permitted in application payload')
            with self.assertRaisesRegex(ValueError, 'unapproved'):
                p.validate_payload(root, 'darwin')

    def test_session_path_spaces_are_supported_without_allowing_traversal(self):
        schema = json.loads((p.HERE / 'manifest.schema.json').read_text())
        pattern = schema['properties']['files']['items']['properties']['path']['pattern']
        self.assertRegex(p.SESSION_BINARY, pattern)
        self.assertIsNone(re.fullmatch(pattern, '../' + p.SESSION_BINARY))
        self.assertIn('native_login', schema['required'])
        tree = ET.parse(p.HERE / 'templates/macos-distribution.xml')
        self.assertEqual(tree.find('volume-check/allowed-os-versions/os-version').get('min'), '14.0')
        self.assertIn('WebView2', schema['properties']['native_login']['properties']['provider']['enum'])
        self.assertIn(p.WINDOWS_SESSION_MINIMUM_OS, schema['properties']['native_login']['properties']['minimum_os']['enum'])
        self.assertIn('MinVersion=' + p.WINDOWS_SESSION_MINIMUM_OS, (p.HERE / 'templates/windows.iss').read_text())
        tree = ET.parse(p.HERE / 'templates/windows.wxs')
        ns = {'w': 'http://wixtoolset.org/schemas/v4/wxs'}
        self.assertIn('17763', tree.find('w:Package/w:Launch', ns).get('Condition'))

    def test_mac_and_windows_release_need_real_product_login_evidence(self):
        for platform, arch in (('darwin', 'arm64'), ('windows', 'amd64')):
            manifest = {'platform': platform, 'architecture': arch, 'mount': True, 'files': []}
            evidence = {'platform': platform, 'architecture': arch, 'runner': 'fixture',
                        'payload_sha256': p.payload_digest([]), 'checks': {name: 'passed' for name in p.NATIVE_GATES}}
            with self.assertRaisesRegex(ValueError, 'native_login'):
                p.check_native_evidence(manifest, evidence)
            evidence['checks']['native_login'] = 'passed'
            p.check_native_evidence(manifest, evidence)

    def test_builders_compile_swift_only_for_mac_and_sign_its_app(self):
        common = (p.HERE / 'common.sh').read_text()
        windows = (p.HERE / 'build-windows.ps1').read_text()
        self.assertIn('if [ "$DB_PLATFORM" = darwin ]', common)
        self.assertIn('xcrun swiftc -swift-version 6', common)
        self.assertIn('-apple-macos14.0', common)
        self.assertIn('native/session-host/main.swift', common)
        self.assertIn('--session-host-binary', common)
        self.assertNotIn('browser-host', common + windows)
        self.assertIn('DriveBridge.SessionHost.csproj', windows)
        self.assertIn('session-licenses.py', windows)
        self.assertIn('libexec/drivebridge-session-host.exe', windows)
        self.assertIn("'verify', '/pa', \"$payload/libexec/drivebridge-session-host.exe\"", windows)
        self.assertIn('"$DB_PAYLOAD/libexec/DriveBridge Session.app"', (p.HERE / 'build-macos.sh').read_text())
        self.assertIn('codesign --verify --strict "$DB_PAYLOAD/libexec/DriveBridge Session.app"', (p.HERE / 'smoke.sh').read_text())
        self.assertNotIn('browser-host', (p.HERE / 'smoke.ps1').read_text())


if __name__ == '__main__':
    unittest.main()
