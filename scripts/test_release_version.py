import importlib.util
import pathlib
import subprocess
import tempfile
import unittest

spec = importlib.util.spec_from_file_location('version', pathlib.Path(__file__).with_name('release-version.py'))
version = importlib.util.module_from_spec(spec)
spec.loader.exec_module(version)

class VersionTests(unittest.TestCase):
    def test_initial_patch_minor_major_and_rerun(self):
        with tempfile.TemporaryDirectory() as directory:
            def git(*args):
                return subprocess.check_output(['git', '-C', directory, *args], text=True).strip()
            original = version.git
            version.git = git
            try:
                git('init', '-q'); git('config', 'user.email', 'test@example.test'); git('config', 'user.name', 'Test'); git('config', 'tag.gpgsign', 'false'); git('config', 'commit.gpgsign', 'false')
                git('commit', '--allow-empty', '-qm', 'feat: initial platform')
                self.assertEqual(version.next_version(), 'v1.0.0')
                git('tag', 'v1.0.0')
                self.assertEqual(version.next_version(), 'v1.0.0')
                git('commit', '--allow-empty', '-qm', 'fix: retry policy')
                self.assertEqual(version.next_version(), 'v1.0.1')
                git('tag', 'v1.0.1'); git('commit', '--allow-empty', '-qm', 'feat(campaign): mapping')
                self.assertEqual(version.next_version(), 'v1.1.0')
                git('tag', 'v1.1.0'); git('commit', '--allow-empty', '-qm', 'feat!: change API')
                self.assertEqual(version.next_version(), 'v2.0.0')
            finally:
                version.git = original
