import hashlib
import importlib.util
import json
import pathlib
import tempfile
import unittest

SPEC = importlib.util.spec_from_file_location('git_runtime', pathlib.Path(__file__).parents[1] / 'git_runtime.py')
runtime = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(runtime)


class GitRuntimeTests(unittest.TestCase):
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
