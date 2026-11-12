import hashlib
import importlib.util
import json
import pathlib
import tempfile
import shutil
import types
import unittest
from unittest import mock
import zipfile

SPEC = importlib.util.spec_from_file_location('git_runtime', pathlib.Path(__file__).parents[1] / 'git_runtime.py')
runtime = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(runtime)


class GitRuntimeTests(unittest.TestCase):
    def test_windows_build_accepts_ucrt64_layout_and_keeps_licenses(self):
        with tempfile.TemporaryDirectory() as temp:
            root = pathlib.Path(temp)
            archive = root / 'official.zip'
            with zipfile.ZipFile(archive, 'w') as z:
                z.writestr('ucrt64/bin/git.exe', b'Git binary')
                z.writestr('ucrt64/bin/libiconv-2.dll', b'Native dependency')
                for name in ('libintl-8.dll', 'libpcre2-8-0.dll', 'zlib1.dll'):
                    z.writestr('ucrt64/bin/' + name, b'Native dependency')
                z.writestr('ucrt64/bin/MicroCom.Runtime.dll', b'Unused credential-manager dependency')
                z.writestr('ucrt64/share/licenses/libiconv/COPYING', 'dependency license')
                z.writestr('LICENSE.txt', 'Git for Windows license')
                z.writestr('cmd/git.exe', b'Wrapper that must not replace the real binary')
            args = types.SimpleNamespace(platform='windows', architecture='amd64', output=str(root / 'runtime'))
            with mock.patch.object(runtime, 'download', side_effect=lambda pin, dest: shutil.copyfile(archive, dest)):
                runtime.build(args)
            out = root / 'runtime'
            runtime.verify(out, 'windows', 'amd64')
            self.assertEqual((out / 'bin/git.exe').read_bytes(), b'Git binary')
            self.assertEqual((out / 'bin/libiconv-2.dll').read_bytes(), b'Native dependency')
            self.assertEqual((out / 'licenses/git-for-windows.LICENSE.txt').read_text(), 'Git for Windows license')
            self.assertEqual((out / 'licenses/libiconv/COPYING').read_text(), 'dependency license')
            self.assertFalse((out / 'bin/MicroCom.Runtime.dll').exists(), 'unrelated credential-manager libraries must not ship')

    def fixture(self, root):
        (root / 'bin').mkdir()
        (root / 'licenses').mkdir()
        (root / 'bin/git').write_bytes(b'runtime bytes')
        (root / 'licenses/COPYING').write_text('license')
        (root / 'SOURCE.json').write_text('{}')
        (root / 'runtime.json').write_text(json.dumps({'schema_version': 1, 'version': '2.56.1',
            'platform': 'linux', 'architecture': 'amd64', 'files': runtime.inventory(root)}))

    def test_stage_checks_target_and_every_byte(self):
        with tempfile.TemporaryDirectory() as temp:
            root = pathlib.Path(temp) / 'runtime'
            root.mkdir()
            self.fixture(root)
            dest = pathlib.Path(temp) / 'payload/libexec/git'
            runtime.stage(root, dest, 'linux', 'amd64')
            self.assertEqual((dest / 'bin/git').read_bytes(), b'runtime bytes')
            with self.assertRaisesRegex(ValueError, 'target'):
                runtime.verify(dest, 'windows', 'amd64')
            (dest / 'bin/git').write_bytes(b'changed runtime')
            with self.assertRaisesRegex(ValueError, 'inventory'):
                runtime.verify(dest, 'linux', 'amd64')

    def test_runtime_rejects_extra_files_and_path_escape(self):
        with tempfile.TemporaryDirectory() as temp:
            root = pathlib.Path(temp)
            self.fixture(root)
            (root / 'credentials.json').write_text('must not package')
            with self.assertRaisesRegex(ValueError, 'inventory'):
                runtime.verify(root)
            with self.assertRaisesRegex(ValueError, 'unsafe'):
                runtime.safe_name('../outside')


if __name__ == '__main__':
    unittest.main()
