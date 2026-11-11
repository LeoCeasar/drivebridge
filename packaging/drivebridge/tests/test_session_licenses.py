import importlib.util
import json
from pathlib import Path
import tempfile
import unittest

spec = importlib.util.spec_from_file_location('session_licenses', Path(__file__).resolve().parents[1] / 'session-licenses.py')
p = importlib.util.module_from_spec(spec)
spec.loader.exec_module(p)


class SessionLicenseTests(unittest.TestCase):
    def fixture(self, root):
        packages = root / 'nuget'
        for name, notice in p.PACKAGES.items():
            path = packages / name / '10.0.1'
            path.mkdir(parents=True)
            (path / 'LICENSE.txt').write_text('license ' + name)
            if notice:
                (path / 'THIRD-PARTY-NOTICES.TXT').write_text('notice ' + name)
        assets = {'packageFolders': {str(packages): {}},
                  'libraries': {'Microsoft.Web.WebView2/10.0.1': {}},
                  'project': {'frameworks': {'net10.0-windows7.0': {'downloadDependencies': [
                      {'name': name, 'version': '[10.0.1, 10.0.1]'}
                      for name in p.PACKAGES if name != 'microsoft.web.webview2']}}}}
        source = root / 'project.assets.json'
        source.write_text(json.dumps(assets))
        return source, assets

    def test_collects_exact_restored_dependency_license_and_runtime_notices(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            source, assets = self.fixture(root)
            output = root / 'out' / 'native-login.LICENSES'
            p.collect(source, output)
            text = output.read_text()
            for package in p.PACKAGES:
                self.assertIn('license ' + package, text)
            self.assertIn('notice microsoft.netcore.app.runtime.win-x64', text)
            self.assertIn('notice microsoft.web.webview2', text)
            self.assertNotIn(str(root), text)  # Build paths stay private.

    def test_missing_runtime_notice_and_nonexact_pack_version_fail_closed(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            source, assets = self.fixture(root)
            output = root / 'native-login.LICENSES'
            notice = root / 'nuget/microsoft.netcore.app.runtime.win-x64/10.0.1/THIRD-PARTY-NOTICES.TXT'
            notice.unlink()
            with self.assertRaisesRegex(ValueError, 'license/notice is missing'):
                p.collect(source, output)
            self.assertFalse(output.exists())
            assets['project']['frameworks']['net10.0-windows7.0']['downloadDependencies'][0]['version'] = '[10.0.1, 11.0.0)'
            with self.assertRaisesRegex(ValueError, 'must be exact'):
                p.package_versions(assets)

    def test_missing_webview_package_cannot_silently_omit_license(self):
        with self.assertRaisesRegex(ValueError, 'missing exact native login dependency'):
            p.package_versions({'libraries': {}, 'project': {}})


if __name__ == '__main__':
    unittest.main()
