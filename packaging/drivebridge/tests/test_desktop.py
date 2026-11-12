import argparse
import importlib.util
import json
import os
import pathlib
import plistlib
import struct
import subprocess
import tempfile
import unittest
import xml.etree.ElementTree as ET
from unittest import mock

spec = importlib.util.spec_from_file_location('desktop_package', pathlib.Path(__file__).resolve().parents[1] / 'package.py')
p = importlib.util.module_from_spec(spec)
spec.loader.exec_module(p)


class DesktopPackagingTests(unittest.TestCase):
    def binary(self, target):
        if target == 'darwin':
            return b'\xcf\xfa\xed\xfe' + struct.pack('<I', 0x0100000c) + b'\0' * 120
        if target == 'linux':
            return b'\x7fELF\x02\x01' + b'\0' * 12 + struct.pack('<H', 183) + b'\0' * 100
        data = bytearray(128)
        data[:2] = b'MZ'
        struct.pack_into('<I', data, 60, 64)
        data[64:68] = b'PE\0\0'
        struct.pack_into('<H', data, 68, 0x8664)
        return data

    def test_desktop_never_uses_cli_only_mount_bypass(self):
        for unsigned in (True, False):
            with self.assertRaisesRegex(ValueError, 'native mount'):
                p.check_mode(True, unsigned, True)
        p.check_mode(False, True, True)

    def test_mac_app_is_menu_bar_bundle_with_inventory_and_sibling_cli(self):
        with tempfile.TemporaryDirectory() as d:
            root = pathlib.Path(d) / 'payload'
            for name in p.required_files('darwin'):
                dest = root / name
                dest.parent.mkdir(parents=True, exist_ok=True)
                dest.write_bytes(b'fixture')
            binary = pathlib.Path(d) / 'desktop'
            binary.write_bytes(self.binary('darwin'))
            p.stage_session_host(root, 'darwin', 'arm64', binary, '1.2.3')
            with mock.patch.object(p, 'run', side_effect=['enable-autostart', json.dumps({'native_tray_compiled': True, 'platform': 'darwin', 'architecture': 'arm64'})]) as run:
                p.stage_desktop(root, 'darwin', 'arm64', binary, '1.2.3')
            self.assertEqual(run.call_args_list, [mock.call([binary, 'desktop', '--help']), mock.call([binary, 'desktop', 'capabilities'])])
            info = plistlib.loads((root / 'DriveBridge.app/Contents/Info.plist').read_bytes())
            self.assertIs(info['LSUIElement'], True)
            self.assertEqual(info['CFBundleExecutable'], 'drivebridge-desktop')
            self.assertEqual(info['CFBundleVersion'], '1.2.3')
            self.assertEqual(info['LSMinimumSystemVersion'], '14.0')
            icon = (root / 'DriveBridge.app/Contents/Resources/drivebridge.icns').read_bytes()
            self.assertEqual(icon[:4], b'icns')
            self.assertEqual(struct.unpack('>I', icon[4:8])[0], len(icon))
            self.assertTrue((root / 'bin/drivebridge').is_file())
            self.assertFalse((root / 'DriveBridge.app/Contents/MacOS/drivebridge').exists())
            p.validate_payload(root, 'darwin', True)
            with self.assertRaisesRegex(ValueError, 'unapproved|startup'):
                p.validate_payload(root, 'darwin', False)
            inventory = p.inventory(root)
            manifest = {'platform': 'darwin', 'desktop': True, 'files': inventory}
            p.verify_files(root, manifest)
            (root / 'DriveBridge.app/Contents/MacOS/drivebridge-desktop').write_bytes(b'changed')
            with self.assertRaisesRegex(ValueError, 'inventory'):
                p.verify_files(root, manifest)

    def test_windows_console_binary_cannot_be_staged_as_desktop(self):
        with tempfile.TemporaryDirectory() as d:
            binary = pathlib.Path(d) / 'desktop.exe'
            data = bytearray(self.binary('windows'))
            data.extend(b'\0' * 64)
            struct.pack_into('<H', data, 64 + 92, 3)  # Console subsystem.
            binary.write_bytes(data)
            with self.assertRaisesRegex(ValueError, 'windowsgui'):
                p.stage_desktop(pathlib.Path(d) / 'payload', 'windows', 'amd64', binary, '1.2.3')
            struct.pack_into('<H', data, 64 + 92, 2)
            binary.write_bytes(data)
            with mock.patch.object(p, 'run', side_effect=['enable-autostart', json.dumps({'native_tray_compiled': True, 'platform': 'windows', 'architecture': 'amd64'})]):
                p.stage_desktop(pathlib.Path(d) / 'payload', 'windows', 'amd64', binary, '1.2.3')
            self.assertEqual((pathlib.Path(d) / 'payload/bin/drivebridge-desktop.exe').read_bytes(), data)

    def test_native_desktop_capability_required_before_copying_any_files(self):
        with tempfile.TemporaryDirectory() as d:
            root = pathlib.Path(d) / 'payload'
            binary = pathlib.Path(d) / 'desktop'
            binary.write_bytes(self.binary('darwin'))
            for capabilities in ({}, {'native_tray_compiled': False, 'platform': 'darwin', 'architecture': 'arm64'},
                                 {'native_tray_compiled': True, 'platform': 'linux', 'architecture': 'arm64'},
                                 {'native_tray_compiled': True, 'platform': 'darwin', 'architecture': 'amd64'}):
                with mock.patch.object(p, 'run', side_effect=['enable-autostart', json.dumps(capabilities)]):
                    with self.assertRaisesRegex(ValueError, 'native tray support'):
                        p.stage_desktop(root, 'darwin', 'arm64', binary, '1.2.3')
                self.assertFalse(root.exists())

    def test_expected_desktop_files_are_required_and_no_other_plist_is_allowed(self):
        with tempfile.TemporaryDirectory() as d:
            root = pathlib.Path(d)
            for name in p.required_files('darwin', True):
                dest = root / name
                dest.parent.mkdir(parents=True, exist_ok=True)
                dest.write_text('fixture')
            extra = root / 'DriveBridge.app/Contents/evil.plist'
            extra.write_text('fixture')
            with self.assertRaisesRegex(ValueError, 'startup'):
                p.validate_payload(root, 'darwin', True)
            extra.unlink()
            (root / 'DriveBridge.app/Contents/MacOS/drivebridge-desktop').unlink()
            with self.assertRaisesRegex(ValueError, 'missing'):
                p.validate_payload(root, 'darwin', True)

    def test_windows_shortcut_only_for_separate_desktop_exe(self):
        with tempfile.TemporaryDirectory() as d:
            root = pathlib.Path(d) / 'payload'
            (root / 'bin').mkdir(parents=True)
            (root / 'bin/drivebridge.exe').write_bytes(b'cli')
            for desktop in (False, True):
                if desktop:
                    (root / 'bin/drivebridge-desktop.exe').write_bytes(b'desktop')
                for template in ('windows.wxs', 'windows.iss'):
                    out = pathlib.Path(d) / template
                    p.render(argparse.Namespace(root=str(root), output=str(out), template=template, version='1.2.3', arch='amd64'))
                    if template.endswith('.wxs'):
                        tree = ET.parse(out)
                        ns = {'w': 'http://wixtoolset.org/schemas/v4/wxs'}
                        shortcut = tree.find('.//w:Shortcut', ns)
                        self.assertEqual(shortcut is not None, desktop)
                        if desktop:
                            self.assertEqual(shortcut.get('Target'), '[INSTALLFOLDER]bin\\drivebridge-desktop.exe')
                            self.assertIsNotNone(tree.find('.//w:StandardDirectory[@Id="DesktopFolder"]', ns))
                    else:
                        self.assertEqual('[Icons]' in out.read_text(), desktop)

    def test_linux_desktop_runtime_dependencies_and_system_launcher(self):
        with tempfile.TemporaryDirectory() as d:
            for template in ('deb-control', 'linux.spec'):
                out = pathlib.Path(d) / template
                p.render(argparse.Namespace(root=d, output=str(out), template=template, version='1.2.3', arch='arm64', cli_only=False, desktop=True))
                text = out.read_text()
                self.assertIn('ayatana', text)
                if template == 'linux.spec':
                    self.assertIn('/usr/share/applications/org.drivebridge.desktop.desktop', text)
                    self.assertIn('exit 1', text)  # Existing multi-user lifecycle gate remains.
            entry = (p.HERE / 'templates/drivebridge.desktop').read_text()
            self.assertIn('Exec=/opt/drivebridge/bin/drivebridge-desktop', entry)
            self.assertIn('Terminal=false', entry)
            self.assertNotIn('autostart', entry)

    def test_portable_launcher_preserves_literal_argv_and_registers_only_application(self):
        with tempfile.TemporaryDirectory() as d:
            root = pathlib.Path(d) / 'portable $ with space %'
            (root / 'bin').mkdir(parents=True)
            record = pathlib.Path(d) / 'record'
            binary = root / 'bin/drivebridge-desktop'
            binary.write_text('#!/bin/sh\nprintf "%s\\n" "$@" > "$DB_RECORD"\n')
            binary.chmod(0o700)
            for name in ('launch-desktop.sh', 'install-desktop.sh'):
                p.copy_file(p.HERE / name, root / name)
            env = dict(os.environ, DB_RECORD=str(record), XDG_DATA_HOME=str(pathlib.Path(d) / 'data'))
            subprocess.run(['sh', str(root / 'launch-desktop.sh'), 'desktop', 'literal $(never execute)'], check=True, env=env)
            self.assertEqual(record.read_text().splitlines(), ['desktop', 'literal $(never execute)'])
            subprocess.run(['sh', str(root / 'install-desktop.sh')], check=True, env=env, capture_output=True)
            entry = pathlib.Path(env['XDG_DATA_HOME']) / 'applications/org.drivebridge.desktop.desktop'
            self.assertIn('%%', entry.read_text())
            self.assertIn('\\\\$', entry.read_text())
            self.assertFalse((pathlib.Path(d) / 'data/autostart').exists())

    def test_desktop_native_gates_bind_additional_real_tray_login_and_upgrade_evidence(self):
        manifest = {'platform': 'darwin', 'architecture': 'arm64', 'mount': True, 'desktop': True, 'files': []}
        evidence = {'platform': 'darwin', 'architecture': 'arm64', 'runner': 'fixture', 'payload_sha256': p.payload_digest([]), 'checks': {gate: 'passed' for gate in p.NATIVE_GATES}}
        with self.assertRaisesRegex(ValueError, 'desktop_tray'):
            p.check_native_evidence(manifest, evidence)
        for gate in ('desktop_tray', 'desktop_login', 'desktop_upgrade_exit'):
            evidence['checks'][gate] = 'passed'
        with self.assertRaisesRegex(ValueError, 'native_login'):
            p.check_native_evidence(manifest, evidence)
        evidence['checks']['native_login'] = 'passed'
        p.check_native_evidence(manifest, evidence)

    def test_build_scripts_request_native_tray_and_windows_gui_separately(self):
        common = (p.HERE / 'common.sh').read_text()
        self.assertIn('desktop_cgo=1', common)
        self.assertIn('--desktop-binary', common)
        windows = (p.HERE / 'build-windows.ps1').read_text()
        self.assertIn('$tags = \'cmount\'', windows)
        self.assertIn('"$tags,desktop"', windows)
        self.assertIn("'drivebridge_feishu'", windows)
        self.assertIn('-H=windowsgui', windows)
        self.assertIn('drivebridge-desktop.exe', windows)
        self.assertIn('codesign', (p.HERE / 'build-macos.sh').read_text())


if __name__ == '__main__':
    unittest.main()
