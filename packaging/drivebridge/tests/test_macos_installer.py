import hashlib
import importlib.util
import pathlib
import plistlib
import tempfile
import unittest


HERE = pathlib.Path(__file__).resolve().parents[1]


class MacInstallerTests(unittest.TestCase):
    def test_dmg_has_one_graphical_entry_and_self_contained_application(self):
        path = HERE / 'macos_app.py'
        self.assertTrue(path.is_file(), 'missing graphical macOS installer builder')
        spec = importlib.util.spec_from_file_location('macos_app', path)
        module = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(module)
        with tempfile.TemporaryDirectory() as d:
            root = pathlib.Path(d)
            payload = root / 'payload'
            for name in ('bin/drivebridge', 'upgrade.sh', 'manifest.json',
                         'DriveBridge.app/Contents/Resources/drivebridge.icns'):
                file = payload / name
                file.parent.mkdir(parents=True, exist_ok=True)
                file.write_text('fixture')
            inner_info = payload / 'DriveBridge.app/Contents/Info.plist'
            inner_info.write_bytes(plistlib.dumps({'CFBundleIdentifier': 'org.drivebridge.desktop'}))
            helper_info = payload / 'libexec/DriveBridge Link Service.app/Contents/Info.plist'
            helper_info.parent.mkdir(parents=True)
            helper_info.write_bytes(plistlib.dumps({'CFBundleIdentifier': 'org.drivebridge.linkservice', 'NSServices': module.services()}))
            before = {p.relative_to(payload): hashlib.sha256(p.read_bytes()).hexdigest()
                      for p in payload.rglob('*') if p.is_file()}
            launcher, installer = root / 'launcher', root / 'installer'
            launcher.write_text('launcher')
            installer.write_text('installer')
            module.assemble(payload, launcher, installer, root / 'dmg', '0.1.2')
            entry = root / 'dmg/安装 DriveBridge.app'
            self.assertEqual(list((root / 'dmg').iterdir()), [entry])
            app = entry / 'Contents/Resources/DriveBridge.app'
            self.assertEqual((app / 'Contents/Resources/payload/bin/drivebridge').read_text(), 'fixture')
            self.assertEqual((app / 'Contents/MacOS/drivebridge-launcher').read_text(), 'launcher')
            info = plistlib.loads((app / 'Contents/Info.plist').read_bytes())
            self.assertEqual(info['CFBundleIdentifier'], 'org.drivebridge.launcher')
            self.assertTrue(info['LSUIElement'])
            providers = [(p, plistlib.loads(p.read_bytes()).get('NSServices', []))
                         for p in app.rglob('Info.plist')]
            services = [(p, service) for p, values in providers for service in values
                        if service.get('NSMessage') == 'copyFeishuLink']
            self.assertEqual(len(services), 1, 'one installed product must publish one Finder service')
            self.assertEqual(services[0][0], app / 'Contents/Resources/payload/libexec/DriveBridge Link Service.app/Contents/Info.plist')
            self.assertNotIn('NSServices', info)
            copied = app / 'Contents/Resources/payload'
            self.assertEqual(before, {p.relative_to(copied): hashlib.sha256(p.read_bytes()).hexdigest()
                                      for p in copied.rglob('*') if p.is_file()})
            service = services[0][1]
            self.assertEqual(service['NSMessage'], 'copyFeishuLink')
            self.assertEqual(service['NSMenuItem']['default'], '复制飞书分享链接')
            self.assertNotIn('NSReturnTypes', service)
            info = plistlib.loads((entry / 'Contents/Info.plist').read_bytes())
            self.assertFalse(info.get('LSUIElement', False))
            self.assertEqual(info['CFBundleIdentifier'], 'org.drivebridge.installer')
            self.assertNotIn('NSServices', info)
            self.assertFalse(any((root / 'dmg').glob('*.pkg')))


if __name__ == '__main__':
    unittest.main()
