import argparse
import base64
import contextlib
import fcntl
import importlib.util
import io
import json
import os
from pathlib import Path
import sys
import tempfile
import unittest
from unittest.mock import patch

ROOT = Path(__file__).resolve().parent.parent
spec = importlib.util.spec_from_file_location('repair', ROOT / 'scripts/repair-stalled-macos-pool.py')
repair = importlib.util.module_from_spec(spec)
spec.loader.exec_module(repair)


class FakeRepair(repair.Repair):
    def __init__(self, args, output, inventory):
        super().__init__(args, output)
        self.inventory = inventory
        self.guest_output = None
        self.ssh_calls = []

    def command(self, args, **kwargs):
        self.commands.append({'argv': args})
        if args[:2] == ['tart', 'list']:
            return json.dumps(list(self.inventory.values()))
        if args[:2] == ['tart', 'get']:
            return json.dumps(self.inventory[args[2]])
        if args[:2] == ['tart', 'suspend']:
            self.inventory[args[2]]['State'] = 'suspended'
            (self.home / 'vms' / args[2] / 'pulse.exit').write_text('0\n')
            return ''
        raise AssertionError(args)

    def ssh(self, name, script):
        self.ssh_calls.append((name, script))
        if self.guest_output is not None:
            return self.guest_output
        identity = {'mac': self.inventory[name]['MACAddress'], 'boot_id': '00000000-0000-0000-0000-000000000001'}
        return 'PULSE_TEMPLATE_READY ' + json.dumps(identity) + '\nPULSE_REPAIR_BOOT ' + identity['boot_id'] + '\n'


class RepairPool(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.home = self.root / 'tart'
        self.output = self.root / 'report'
        self.output.mkdir()
        lock = self.home / '.pulse' / repair.SCOPE / 'lock'
        lock.parent.mkdir(parents=True)
        lock.touch()
        self.inventory = {}
        for index, pool in enumerate(repair.POOLS):
            base, slots = repair.names(pool)
            for name in (base, slots[0]):
                directory = self.home / 'vms' / name
                directory.mkdir(parents=True)
                native = dict(uid=pool[3], mac=f'02:00:00:00:00:{index+1:02x}',
                              spec=dict(name=name, image=base, username='admin', password='SECRET_REPAIR_PASSWORD'))
                native.update(device=directory.stat().st_dev, inode=directory.stat().st_ino)
                props = {}
                if name == base:
                    props['image'] = dict(name=name, phase='ready')
                    native['reusable'] = True
                    native['spec']['image'] = 'ghcr.io/cirruslabs/base@sha256:' + 'a' * 64
                else:
                    props['slot'] = dict(name=name, uid=pool[3], phase='preparing')
                    (directory / '.pulse-owner').write_text(pool[3])
                (directory / 'pulse.json').write_text(json.dumps(dict(scope=repair.SCOPE, native=native, properties=props)))
                self.inventory[name] = dict(Name=name, State='stopped' if name == base else 'running',
                                            OS='darwin', CPU=4, Memory=8192, MACAddress=native['mac'])
        self.args = argparse.Namespace(tart_home=self.home, timeout=60, check=False,
                                       state_dir=self.root / 'legacy')
        self.r = FakeRepair(self.args, self.output, self.inventory)

    def updater(self, command, env):
        self.assertIn('--ensure', command)
        self.assertIn(repair.OWNER_TOKEN, command)
        self.assertEqual(env['PULSE_IMAGE_SSH_PASSWORD'], 'SECRET_REPAIR_PASSWORD')
        self.assertNotIn('SECRET_REPAIR_PASSWORD', str(command))
        # Both blocking builders must suspend before provisioning anything else.
        for pool in repair.POOLS:
            self.assertEqual(self.inventory[repair.names(pool)[1][0]]['State'], 'suspended')
        base = command[command.index('--base') + 1]
        pool = next(p for p in repair.POOLS if p[0] == base)
        name = repair.names(pool)[1][1]
        self.inventory[name] = dict(Name=name, State='suspended', OS='darwin', CPU=4, Memory=8192,
                                    MACAddress='02:00:00:00:00:' + ('03' if base == 'macos-26-arm64' else '04'))
        return argparse.Namespace(returncode=0)

    def test_repair_records_real_markers_keeps_metadata_and_fills_both_pools(self):
        before = {p: p.read_bytes() for p in self.home.rglob('pulse.json')}
        with patch.object(repair.subprocess, 'run', side_effect=self.updater) as update:
            self.r.repair()
        self.assertEqual(update.call_count, 2)
        self.assertEqual(len(self.r.ssh_calls), 2)
        for name, script in self.r.ssh_calls:
            self.assertIn('test ! -e /var/db/pulse-tart/intent', script)
            event = (self.home / 'vms' / name / 'pulse.events').read_text()
            self.assertEqual(len(repair.diagnosis.evidence(event)['template_identities']), 1)
        self.assertEqual(before, {p: p.read_bytes() for p in self.home.rglob('pulse.json')})
        self.assertFalse(any(c['argv'][1] in ('delete', 'stop', 'set') for c in self.r.commands))

    def test_job_reservation_refuses_before_any_mutation(self):
        name = repair.names(repair.POOLS[1])[1][0]
        path = self.home / 'vms' / name / 'pulse.json'
        data = json.loads(path.read_text())
        data['properties']['job'] = {'name': name}
        path.write_text(json.dumps(data))
        with self.assertRaisesRegex(repair.RepairError, 'job/pair'):
            self.r.repair()
        self.assertFalse(self.r.ssh_calls)

    def test_different_uid_refuses_before_any_mutation(self):
        name = repair.names(repair.POOLS[1])[1][0]
        path = self.home / 'vms' / name / 'pulse.json'
        data = json.loads(path.read_text())
        data['native']['uid'] = 'replaced'
        path.write_text(json.dumps(data))
        with self.assertRaisesRegex(repair.RepairError, 'no longer matches'):
            self.r.repair()
        self.assertFalse(self.r.ssh_calls)

    def test_missing_or_wrong_marker_never_suspends_or_fakes_ready(self):
        for output in ('PULSE_TEMPLATE_READY {}\n',
                       'PULSE_TEMPLATE_READY {"mac":"02:00:00:00:00:ff","boot_id":"00000000-0000-0000-0000-000000000001"}\nPULSE_REPAIR_BOOT 00000000-0000-0000-0000-000000000001\n'):
            self.r.guest_output = output
            with self.assertRaises(repair.RepairError):
                self.r.finish_builder(repair.POOLS[0])
            self.assertFalse(any(c['argv'][:2] == ['tart', 'suspend'] for c in self.r.commands))
            self.assertFalse((self.home / 'vms' / repair.names(repair.POOLS[0])[1][0] / 'pulse.events').exists())

    def test_rerun_after_suspension_does_not_resume_source(self):
        self.r.finish_builder(repair.POOLS[0])
        self.r.finish_builder(repair.POOLS[0])
        self.assertEqual(len(self.r.ssh_calls), 1)

    def test_check_only_does_not_mutate(self):
        self.args.check = True
        self.r.repair()
        self.assertFalse(self.r.ssh_calls)
        self.assertTrue(all(c['argv'][1] in ('list', 'get') for c in self.r.commands))

    def test_live_pulse_lock_refuses_and_still_archives(self):
        lock = self.home / '.pulse' / repair.SCOPE / 'lock'
        with lock.open('r') as stream:
            fcntl.flock(stream.fileno(), fcntl.LOCK_EX | fcntl.LOCK_NB)
            with patch.object(repair, 'ROOT', self.root), \
                    patch.object(repair.platform, 'system', return_value='Darwin'), \
                    patch.object(repair.platform, 'machine', return_value='arm64'), \
                    patch.object(sys, 'argv', ['repair', '--tart-home', str(self.home)]), \
                    contextlib.redirect_stdout(io.StringIO()), \
                    patch.object(repair.Repair, 'repair') as mutate:
                self.assertEqual(repair.main(), 1)
                mutate.assert_not_called()
        report = next((self.root / 'test-results').glob('*/summary.json'))
        self.assertIn('Stop Pulse normally', json.loads(report.read_text())['error'])
        self.assertTrue(list((self.root / 'test-results').glob('*.tar.gz')))

    def stale_migration(self):
        legacy = self.args.state_dir
        (legacy / 'native').mkdir(parents=True)
        (legacy / 'lock').touch()
        old_key = base64.b64encode(b'old-key-' * 4).decode()
        current_key = base64.b64encode(b'new-key-' * 4).decode()
        (legacy / 'state.json').write_text(json.dumps(dict(version=3, scope=repair.SCOPE, key=old_key)))
        for path in self.home.rglob('pulse.json'):
            data = json.loads(path.read_text())
            data['properties'].update(version=1, scope=repair.SCOPE, key=current_key)
            path.write_text(json.dumps(data))
        for index, pool in enumerate(repair.POOLS):
            _, slots = repair.names(pool)
            self.inventory[slots[0]]['State'] = 'suspended'
            self.inventory[slots[1]] = dict(Name=slots[1], State='suspended', OS='darwin', CPU=4, Memory=8192,
                                           MACAddress=f'02:00:00:00:01:{index:02x}')
            directory = self.home / 'vms' / slots[1]
            directory.mkdir()
            native = dict(spec=dict(name=slots[1]), reusable=True, uid='historical-' + str(index),
                          mac=f'02:00:00:00:02:{index:02x}')
            (legacy / 'native' / (slots[1] + '.json')).write_text(json.dumps(native))
            (directory / 'pulse.json').write_text(json.dumps(dict(scope=repair.SCOPE, native=native)))
            (directory / 'pulse.events').write_text('obsolete-events')
            (directory / 'state.vzvmsave').write_bytes(b'actual-current-snapshot')
            (directory / 'disk.img').write_bytes(b'actual-current-disk')
        return legacy

    def test_retire_stale_journal_preserves_current_keys_and_vm_disks(self):
        legacy = self.stale_migration()
        journal = (legacy / 'state.json').read_bytes()
        authoritative = {p: p.read_bytes() for p in self.home.rglob('pulse.json')
                         if json.loads(p.read_text()).get('properties')}
        images = {p: p.read_bytes() for p in self.home.rglob('*') if p.name in ('disk.img', 'state.vzvmsave')}
        self.r.retire_legacy_state(required=True)
        plan = json.loads((self.output / 'legacy-retirement.json').read_text())
        backup = Path(plan['backup'])
        self.assertFalse(legacy.exists())
        self.assertEqual((backup / 'journal/state.json').read_bytes(), journal)
        for path, content in authoritative.items(): self.assertEqual(path.read_bytes(), content)
        for path, content in images.items(): self.assertEqual(path.read_bytes(), content)
        for pool in repair.POOLS:
            name = repair.names(pool)[1][1]
            self.assertFalse((self.home / 'vms' / name / 'pulse.json').exists())
            self.assertTrue((backup / 'imported-vm-metadata' / name / 'pulse.json').exists())
        self.assertFalse(self.r.ssh_calls)
        self.assertTrue(all(c['argv'][1] in ('list', 'get') for c in self.r.commands))
        self.assertNotIn(base64.b64encode(b'new-key-' * 4).decode(), (self.output / 'legacy-retirement.json').read_text())

    def test_legacy_retirement_refuses_real_missing_resource_or_mixed_keys(self):
        legacy = self.stale_migration()
        name = repair.names(repair.POOLS[0])[0]
        path = self.home / 'vms' / name / 'pulse.json'
        data = json.loads(path.read_text())
        data['properties']['key'] = base64.b64encode(b'unknown-' * 4).decode()
        path.write_text(json.dumps(data))
        with self.assertRaisesRegex(repair.RepairError, 'Current VM keys disagree'):
            self.r.retire_legacy_state(required=True)
        self.assertTrue(legacy.exists())
        del data['properties']
        path.write_text(json.dumps(data))
        with self.assertRaisesRegex(repair.RepairError, 'Unmigrated non-source'):
            self.r.retire_legacy_state(required=True)
        self.assertTrue(legacy.exists())

    def test_legacy_retirement_check_only_and_legacy_lock(self):
        legacy = self.stale_migration()
        self.args.check = True
        self.r.retire_legacy_state(required=True)
        self.assertTrue(legacy.exists())
        self.assertEqual(json.loads((self.output / 'legacy-retirement.json').read_text())['action'], 'planned')
        with (legacy / 'lock').open('r') as stream:
            fcntl.flock(stream.fileno(), fcntl.LOCK_EX | fcntl.LOCK_NB)
            with self.assertRaisesRegex(repair.RepairError, 'owns the legacy journal'):
                self.r.retire_legacy_state(required=True)

    def test_legacy_retirement_does_not_discard_job_reservation(self):
        legacy = self.stale_migration()
        path = self.home / 'vms' / repair.names(repair.POOLS[0])[1][0] / 'pulse.json'
        data = json.loads(path.read_text())
        data['properties']['job'] = {'launch_id': 'in-flight'}
        path.write_text(json.dumps(data))
        with self.assertRaisesRegex(repair.RepairError, 'job/pair reservations'):
            self.r.retire_legacy_state(required=True)
        self.assertTrue(legacy.exists())


if __name__ == '__main__':
    unittest.main()
