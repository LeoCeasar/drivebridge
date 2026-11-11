import importlib.util
import json
import pathlib
import plistlib
import re
import struct
import tempfile
import unittest
import xml.etree.ElementTree as ET

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

    def test_only_mac_payloads_require_owned_login_app(self):
        for target in ('darwin', 'linux', 'windows'):
            for desktop in (False, True):
                files = p.required_files(target, desktop)
                self.assertEqual(p.SESSION_BINARY in files, target == 'darwin')
                self.assertEqual(p.SESSION_PLIST in files, target == 'darwin')
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
            self.assertNotIn('LSUIElement', info)
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
            for target in ('windows', 'linux'):
                p.stage_session_host(root / target, target, 'amd64', None, '1.0.0')
                self.assertFalse((root / target).exists())
                with self.assertRaisesRegex(ValueError, 'macOS-only'):
                    p.stage_session_host(root / target, target, 'amd64', helper, '1.0.0')

    def test_verifier_rejects_bundle_identity_os_or_activation_tampering(self):
        with tempfile.TemporaryDirectory() as d:
            root = pathlib.Path(d) / 'payload'
            self.complete_payload(root)
            p.validate_payload(root, 'darwin', arch='arm64')
            original = plistlib.loads((root / p.SESSION_PLIST).read_bytes())
            for change in ({'CFBundleIdentifier': 'foreign.bundle'}, {'LSMinimumSystemVersion': '13.0'},
                           {'CFBundleExecutable': 'foreign'}, {'LSUIElement': True}, {'LSBackgroundOnly': True}):
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

    def test_mac_release_needs_real_product_login_evidence(self):
        manifest = {'platform': 'darwin', 'architecture': 'arm64', 'mount': True, 'files': []}
        evidence = {'platform': 'darwin', 'architecture': 'arm64', 'runner': 'fixture',
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
        self.assertNotIn('session-host', windows)
        self.assertIn('"$DB_PAYLOAD/libexec/DriveBridge Session.app"', (p.HERE / 'build-macos.sh').read_text())
        self.assertIn('codesign --verify --strict "$DB_PAYLOAD/libexec/DriveBridge Session.app"', (p.HERE / 'smoke.sh').read_text())
        self.assertNotIn('browser-host', (p.HERE / 'smoke.ps1').read_text())


if __name__ == '__main__':
    unittest.main()
