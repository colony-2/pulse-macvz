import importlib.util
import json
import os
from pathlib import Path
import sys
from tempfile import TemporaryDirectory
import unittest
from unittest.mock import patch

from test_update_images_test import FAKE

spec = importlib.util.spec_from_file_location('probe', Path(__file__).with_name('probe-tart-restore.py'))
probe = importlib.util.module_from_spec(spec)
spec.loader.exec_module(probe)


class RestoreProbe(unittest.TestCase):
    def setUp(self):
        self.temp = TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.state = self.root / 'state.json'
        vms = {name: dict(Name=name, OS='darwin', CPU=4, Memory=8192,
                          State='suspended', MACAddress=name)
               for name in ('one', 'two')}
        self.state.write_text(json.dumps(dict(vms=vms, calls=[], next=0)))
        fake = FAKE.replace("waiting=args[-1];vms[waiting]['State']='running'",
                            "waiting=args[-1];vms[waiting]['State']='running';"
                            "print('restoring VM state from a snapshot...\\nresuming VM...', flush=True)\n"
                            "   if os.environ.get('FAIL_RESTORE') or (os.environ.get('FAIL_EXISTING_PAIR') and '-existing-' in waiting and waiting.endswith('-2')): vms[waiting]['State']='suspended';waiting=None;code=1")
        binary = self.root / 'tart'
        binary.write_text('#!' + sys.executable + '\n' + fake)
        binary.chmod(0o755)
        output = self.root / 'output'
        output.mkdir()
        self.p = probe.Probe(output, self.root / 'home')
        self.p.settle_seconds = 0
        self.p.poll_seconds = .01
        self.p.env.update(PATH=str(self.root) + os.pathsep + os.environ['PATH'], FAKE_TART_STATE=str(self.state))
        self.original = vms

    def test_restore_and_cleanup_leave_sources_unchanged(self):
        self.p.preflight(['one', 'two'])
        name = self.p.restore('one', 'alone')
        self.p.remove(name)
        data = json.loads(self.state.read_text())
        self.assertEqual(data['vms'], self.original)
        self.assertEqual(sum(call[1] == 'run' for call in data['calls']), 1)
        self.assertFalse(self.p.clones)

    def test_failed_restore_is_reported_without_retry_and_cleaned(self):
        self.p.env['FAIL_RESTORE'] = '1'
        with self.assertRaisesRegex(RuntimeError, 'tart run exited 1'):
            self.p.restore('one', 'worker')
        for name in list(self.p.clones):
            self.p.remove(name)
        data = json.loads(self.state.read_text())
        self.assertEqual(data['vms'], self.original)
        self.assertEqual(sum(call[1] == 'run' for call in data['calls']), 1)

    def test_preflight_rejects_mac_workloads_but_allows_linux(self):
        data = json.loads(self.state.read_text())
        data['vms']['other'] = dict(Name='other', OS='darwin', State='running')
        self.state.write_text(json.dumps(data))
        with self.assertRaisesRegex(RuntimeError, 'Pause other macOS'):
            self.p.preflight(['one', 'two'])
        data['vms']['other']['OS'] = 'linux'
        self.state.write_text(json.dumps(data))
        self.p.preflight(['one', 'two'])

    def test_preflight_rejects_duplicate_source(self):
        with self.assertRaisesRegex(RuntimeError, 'distinct MAC'):
            self.p.preflight(['one', 'one'])

    def test_comparison_retains_failures_and_cleans_each_case(self):
        data = json.loads(self.state.read_text())
        data['vms']['base'] = dict(data['vms']['one'], Name='base', State='stopped')
        self.state.write_text(json.dumps(data))
        before = data['vms']
        self.p.env['FAIL_EXISTING_PAIR'] = '1'
        cases = []
        self.p.compare_preparation(['one', 'two'], 'base', 1, cases)
        self.assertEqual([case['result'] for case in cases], ['failed'] * 2 + ['passed'] * 7)
        data = json.loads(self.state.read_text())
        self.assertEqual(data['vms'], before)
        self.assertFalse(self.p.clones)
        runs = [call[-1] for call in data['calls'] if call[1] == 'run']
        self.assertEqual(len(runs), len(set(runs)), 'A failed VM must never be retried')
        # Simulate occupancy from the actual subprocess command trace. Concurrent
        # preparation must hold two builders, while all other preparation holds one.
        active = set()
        overlap = False
        serial_changes = 0
        for call in data['calls']:
            if call[1] == 'run':
                active.add(call[-1])
                # Synthetic failed restores have already terminated.
                if '-existing-' in call[-1] and call[-1].endswith('-2'):
                    active.remove(call[-1])
            elif call[1] in ('stop', 'suspend', 'delete'):
                active.discard(call[2])
            elif call[1] == 'set' and '--random-serial' in call:
                serial_changes += 1
                self.assertNotIn(call[2], active)
            self.assertLessEqual(len(active), 2)
            if sum('-concurrent-builder-' in name for name in active) == 2:
                overlap = True
            for mode in ('sequential', 'unique-serial'):
                self.assertLessEqual(sum(f'-{mode}-builder-' in name for name in active), 1)
        self.assertTrue(overlap)
        self.assertEqual(serial_changes, 2)

    def test_failed_preflight_still_archives(self):
        with patch.object(sys, 'argv', ['probe', 'one', 'two']), \
                patch.object(probe.platform, 'system', return_value='Linux'):
            previous = Path.cwd()
            try:
                os.chdir(self.root)
                self.assertEqual(probe.main(), 1)
            finally:
                os.chdir(previous)
        self.assertEqual(len(list((self.root / 'test-results').glob('*.tar.gz'))), 1)
