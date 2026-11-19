import copy
import fcntl
import importlib.util
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest

ROOT = Path(__file__).resolve().parent.parent
spec = importlib.util.spec_from_file_location('verify_publication', ROOT / 'scripts/verify-image-publication.py')
verify = importlib.util.module_from_spec(spec)
spec.loader.exec_module(verify)


class PublicationVerification(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.home = Path(self.temp.name)
        self.source, self.target = 'pulse-build-test-base', 'pulse-test-base'
        self.paths = [self.home / 'vms' / name for name in (self.source, self.target)]
        self.config = {'cpuCount': 4, 'memorySize': 8192, 'macAddress': '02:00:00:00:00:01',
                       'ecid': 'identity', 'hardwareModel': 'hardware'}
        for path in self.paths:
            path.mkdir(parents=True)
            (path / 'config.json').write_text(json.dumps(self.config))
            (path / 'disk.img').write_bytes(b'disk bytes')
            (path / 'nvram.bin').write_bytes(b'nvram bytes')

    def check(self, state='stopped'):
        return verify.verify(self.home, self.source, self.target, state)

    def test_stopped_clone_allows_only_mac_difference(self):
        cfg = copy.deepcopy(self.config)
        cfg['macAddress'] = '02:00:00:00:00:02'
        (self.paths[1] / 'config.json').write_text(json.dumps(cfg))
        self.assertEqual(self.check()['outcome'], 'verified')
        cfg['ecid'] = 'different machine'
        (self.paths[1] / 'config.json').write_text(json.dumps(cfg))
        with self.assertRaisesRegex(verify.Unverified, 'config_mismatch'):
            self.check()

    def test_snapshot_requires_exact_mac_and_saved_state(self):
        for path in self.paths:
            (path / 'state.vzvmsave').write_bytes(b'saved state')
        self.assertIn('state.vzvmsave_bytes_match', self.check('suspended')['checks'])
        cfg = copy.deepcopy(self.config)
        cfg['macAddress'] = '02:00:00:00:00:02'
        (self.paths[1] / 'config.json').write_text(json.dumps(cfg))
        with self.assertRaisesRegex(verify.Unverified, 'config_mismatch'):
            self.check('suspended')
        (self.paths[1] / 'state.vzvmsave').unlink()
        with self.assertRaisesRegex(verify.Unverified, 'saved_state_mismatch'):
            self.check('suspended')

    def test_same_size_content_mismatch_for_every_vm_payload(self):
        for filename in ('disk.img', 'nvram.bin', 'state.vzvmsave'):
            with self.subTest(filename=filename):
                for path in self.paths:
                    (path / filename).write_bytes(b'original')
                (self.paths[1] / filename).write_bytes(b'modified')
                with self.assertRaisesRegex(verify.Unverified, filename + '_content_mismatch'):
                    self.check('suspended' if filename == 'state.vzvmsave' else 'stopped')
                (self.paths[1] / filename).write_bytes(b'original')

    def test_full_file_comparison_checks_past_first_chunk_without_mutation(self):
        for path in self.paths:
            (path / 'disk.img').write_bytes(b'x' * (4 << 20) + b'last')
        before = {p: p.read_bytes() for directory in self.paths for p in directory.iterdir()}
        self.assertEqual(self.check()['outcome'], 'verified')
        self.assertEqual(before, {p: p.read_bytes() for directory in self.paths for p in directory.iterdir()})
        with (self.paths[1] / 'disk.img').open('r+b') as stream:
            stream.seek(4 << 20)
            stream.write(b'fail')
        with self.assertRaisesRegex(verify.Unverified, 'disk.img_content_mismatch'):
            self.check()

    def test_symlinks_and_stacked_layout_are_not_verified(self):
        disk = self.paths[1] / 'disk.img'
        disk.unlink()
        disk.symlink_to(self.paths[0] / 'disk.img')
        with self.assertRaises(OSError):
            self.check()
        (self.paths[1] / 'manifest.json').write_text('{}')
        with self.assertRaisesRegex(verify.Unverified, 'unsupported_vm_layout'):
            self.check()

    def test_tart_pid_lock_prevents_verifying_a_running_vm(self):
        script = '''import fcntl,sys,time
with open(sys.argv[1], 'r+b') as f:
 fcntl.lockf(f, fcntl.LOCK_EX)
 print('locked', flush=True)
 time.sleep(60)
'''
        process = subprocess.Popen([sys.executable, '-c', script, str(self.paths[1] / 'config.json')], stdout=subprocess.PIPE, text=True)
        try:
            self.assertEqual(process.stdout.readline().strip(), 'locked')
            with self.assertRaises(BlockingIOError):
                self.check()
        finally:
            process.terminate()
            process.wait(timeout=5)
            process.stdout.close()

    def test_tart_global_lock_prevents_concurrent_verification(self):
        fd = os.open(self.home, os.O_RDONLY)
        try:
            fcntl.flock(fd, fcntl.LOCK_EX)
            with self.assertRaises(BlockingIOError):
                self.check()
        finally:
            os.close(fd)

    def test_cli_errors_do_not_export_private_config(self):
        (self.paths[1] / 'config.json').write_text('SECRET_PRIVATE_CONFIGURATION')
        result = subprocess.run([sys.executable, str(ROOT / 'scripts/verify-image-publication.py'), self.source, self.target,
                                 'stopped', '--tart-home', str(self.home)], capture_output=True, text=True)
        self.assertEqual(result.returncode, 1)
        self.assertEqual(json.loads(result.stdout)['outcome'], 'unverified')
        self.assertNotIn('SECRET_', result.stdout + result.stderr)


if __name__ == '__main__':
    unittest.main()
