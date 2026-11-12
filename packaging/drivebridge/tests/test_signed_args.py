import os
import pathlib
import shutil
import subprocess
import tempfile
import unittest


@unittest.skipUnless(shutil.which('bash'), 'Bash is required for the macOS build script')
class SignedBuildArgumentsTests(unittest.TestCase):
    def test_signed_macos_stage_preserves_git_runtime_for_both_editions(self):
        script = pathlib.Path(__file__).resolve().parents[1] / 'build-macos.sh'
        for edition in ('feishu', 'full'):
            with self.subTest(edition=edition), tempfile.TemporaryDirectory() as directory:
                root = pathlib.Path(directory)
                include = root / 'fuse'
                include.mkdir()
                (include / 'fuse.h').touch()
                capture = root / 'stage-argv'
                hook = root / 'build-boundary.sh'
                # Run the real argument parsing, signed branch and build_stage.
                # Replace only external build/sign tools, stopping at package staging.
                hook.write_text('''
python3() {
  case "$2" in
    preflight) ;;
    prepare-cli) printf '%s\\n' upstream-digest ;;
    hash-file) printf '%s\\n' signed-digest ;;
    stage) printf '%s\\0' "$@" > "$DB_STAGE_CAPTURE"; exit 0 ;;
    *) printf 'Unexpected Python action: %s\\n' "$2" >&2; exit 99 ;;
  esac
}
codesign() { :; }
go() { :; }
xcrun() { :; }
''')
                git_runtime = str(root / 'Git runtime $(literal)')
                env = dict(os.environ, BASH_ENV=str(hook), TMPDIR=str(root),
                           DB_STAGE_CAPTURE=str(capture), DRIVEBRIDGE_FUSE_INCLUDE=str(include),
                           DRIVEBRIDGE_CODESIGN_IDENTITY='fixture application identity',
                           DRIVEBRIDGE_INSTALLER_IDENTITY='fixture installer identity',
                           DRIVEBRIDGE_NOTARY_PROFILE='fixture notary profile')
                result = subprocess.run([
                    'bash', str(script), '--arch', 'arm64', '--version', '1.2.3',
                    '--lark-package', str(root / 'lark package'),
                    '--lark-native-archive', str(root / 'lark archive.tar.gz'),
                    '--git-runtime', git_runtime, '--edition', edition,
                    '--output', str(root / 'output'),
                ], env=env, capture_output=True, text=True)
                self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
                self.assertTrue(capture.is_file(), 'build did not reach package staging')
                args = capture.read_bytes().decode().rstrip('\0').split('\0')
                self.assertIn('--git-runtime', args)
                self.assertEqual(args[args.index('--git-runtime') + 1], git_runtime)
                self.assertEqual(args.count('--git-runtime'), 1)
                self.assertIn('--signed-cli', args)
                self.assertTrue(args[args.index('--signed-cli') + 1].endswith('/signed-lark-cli'))
                self.assertEqual(args[args.index('--cli-upstream-sha') + 1], 'upstream-digest')
                self.assertEqual(args[args.index('--edition') + 1], edition)
