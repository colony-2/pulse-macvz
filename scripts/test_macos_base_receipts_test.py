"""Run the builder service with fake guest paths; never touch host VM state."""
import os
from pathlib import Path
import subprocess
from tempfile import TemporaryDirectory
import unittest

ROOT = Path(__file__).resolve().parent.parent


class MacBaseReceipts(unittest.TestCase):
    def run_service(self, fail):
        with TemporaryDirectory() as tmp:
            root = Path(tmp)
            state = root / 'state'
            bundle = state / 'setup'
            bundle.mkdir(parents=True)
            (state / 'failed').touch()
            (state / 'ready').write_text('old receipt\n')
            tools = root / 'tools/bin'
            tools.mkdir(parents=True)
            for tool in ['c2j', 'pulse-tart-guest', 'git', 'node', 'pnpm', 'uv', 'python3']:
                p = tools / tool
                p.write_text('#!/bin/bash\nexit 0\n')
                p.chmod(0o755)
            image = root / 'image'
            image.mkdir()
            for name in ['bootstrap-guest.sh', 'prepare-template.sh']:
                (image / name).touch()
            (bundle / 'prepare-image.sh').write_text(
                '#!/bin/bash\nset -eu\n'
                'test ! -e "$1/../ready"\ntest ! -e "$1/../failed"\n'
                + ('echo newer-attempt > \"$1/../attempt\"\nexit 17\n' if fail == 'superseded' else 'exit 17\n' if fail else 'exit 0\n'))
            script = (ROOT / 'scripts/prepare-base.sh').read_text()
            for old, new in [('/var/db/pulse-tart-base', str(state)),
                             ('/opt/pulse-tart-image', str(image)),
                             ('/opt/pulse-tart', str(root / 'tools')),
                             ('/Library/LaunchDaemons', str(root / 'daemons')),
                             ('/var/db/pulse-tart-template', str(root / 'template')),
                             ('/usr/bin/sw_vers', 'sw_vers'), ('/usr/bin/uuidgen', 'uuidgen_test'),
                             ('/sbin/shutdown', 'shutdown_test'), ('$EUID == 0', '1 == 1')]:
                script = script.replace(old, new)
            script = script.replace('\ngrow_macos_storage\n', '\n: # storage tested separately\n').replace('\nmacos_free_space\n', '\n: # storage tested separately\n')
            if fail == 'storage':
                script = script.replace(': # storage tested separately', 'storage_failure() { false; }; storage_failure', 1)
            prelude = '''
uname() { case "$1" in -s) echo Darwin;; -m) echo arm64;; esac; }
sw_vers() { echo 15.7.7; }
sync() { :; }
shutdown_test() { echo SHUTDOWN; }
uuidgen_test() { echo attempt-test; }
sleep() { if [[ $1 != 5 ]]; then command sleep "$1"; fi; }
'''
            file = root / 'prepare.sh'
            file.write_text(prelude + script)
            # Files avoid pipes retained by the background timeout's sleep.
            with (root / 'output').open('w') as output:
                result = subprocess.run(['bash', str(file), str(bundle), '60', '15.7.7', 'install'],
                                        env=dict(os.environ, PULSE_BASE_SERVICE='1'),
                                        stdout=output, stderr=output, timeout=10)
            return result.returncode, (state / 'ready').exists(), (state / 'failed').exists(), (root / 'output').read_text()

    def test_successful_reboot_retry_replaces_stale_failure(self):
        code, ready, failed, output = self.run_service(False)
        self.assertEqual(code, 0, output)
        self.assertTrue(ready)
        self.assertFalse(failed)

    def test_superseded_attempt_cannot_fail_or_shutdown_new_attempt(self):
        code, ready, failed, output = self.run_service('superseded')
        self.assertNotEqual(code, 0, output)
        self.assertFalse(ready)
        self.assertFalse(failed)
        self.assertNotIn('SHUTDOWN', output)

    def test_failed_retry_invalidates_old_success(self):
        code, ready, failed, output = self.run_service(True)
        self.assertNotEqual(code, 0, output)
        self.assertFalse(ready)
        self.assertTrue(failed)

    def test_storage_failure_marks_builder_failed(self):
        code, ready, failed, output = self.run_service('storage')
        self.assertNotEqual(code, 0, output)
        self.assertFalse(ready)
        self.assertTrue(failed)


if __name__ == '__main__':
    unittest.main()
