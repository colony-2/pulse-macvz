import argparse
import contextlib
import fcntl
import importlib.util
import io
import json
from pathlib import Path
import tempfile
import unittest

ROOT = Path(__file__).resolve().parents[1]
spec = importlib.util.spec_from_file_location('repair_nix', ROOT / 'scripts/repair-linux-nix.py')
repair = importlib.util.module_from_spec(spec)
spec.loader.exec_module(repair)
SCOPE = 'a' * 64


class Process:
    def __init__(self):
        self.code = None

    def poll(self):
        return self.code

    def wait(self, timeout):
        assert self.code is not None
        return self.code


class FakeRepair(repair.Repair):
    def __init__(self, args, inventory):
        super().__init__(args)
        self.inventory = inventory
        self.commands, self.repairs, self.started = [], [], {}
        self.fail = set()

    def command(self, args, **kwargs):
        self.commands.append(args)
        if args[:2] == ['tart', 'list']:
            return json.dumps(list(self.inventory.values()))
        if args[:2] == ['tart', 'get']:
            return json.dumps(self.inventory[args[2]])
        if args[:2] == ['tart', 'stop']:
            self.inventory[args[2]]['State'] = 'stopped'
            self.started[args[2]].code = 0
            return ''
        raise AssertionError(args)

    def ssh(self, name, data, script):
        self.inspect(name, data)
        if script == 'true\n':
            return ''
        self.repairs.append((name, script))
        if name in self.fail:
            raise repair.RepairError('Nix daemon socket permission denied')
        return 'PULSE_NIX_REPAIRED\nPULSE_NIX_EXISTING_JOB\n'

    def start(self, name):
        self.inventory[name]['State'] = 'running'
        self.started[name] = Process()
        return self.started[name]


class RepairLinuxNix(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.home = Path(self.tmp.name)
        self.lock = self.home / '.pulse' / SCOPE / 'lock'
        self.lock.parent.mkdir(parents=True)
        self.lock.touch(mode=0o600)
        self.inventory = {}
        self.add_vm('linux-primary', 'running', {'pair': {'Primary': {'name': 'linux-primary'}, 'LaunchID': 'active-job'}})
        self.add_vm('linux-base', 'stopped', {'image': {'kind': 'base', 'phase': 'ready'}}, reusable=True)
        self.add_vm('macos-engine', 'running', {'pair': {'Primary': {'name': 'macos-primary'}}})
        self.add_vm('macos-primary', 'running', {'pair': {'Primary': {'name': 'macos-primary'}}}, osname='darwin')
        self.args = argparse.Namespace(tart_home=self.home, include_stopped=False, check=False, vm=None, timeout=2)
        self.r = FakeRepair(self.args, self.inventory)

    def add_vm(self, name, state, props, reusable=False, osname='linux'):
        directory = self.home / 'vms' / name
        directory.mkdir(parents=True)
        native = dict(uid=name + '-uid', mac='02:00:00:00:00:' + f'{len(self.inventory):02x}',
                      inode=directory.stat().st_ino, device=directory.stat().st_dev, reusable=reusable,
                      spec=dict(name=name, os=osname, username='admin', password='PRIVATE_PASSWORD'))
        for filename, content in [('pulse.json', json.dumps(dict(version=1, scope=SCOPE, native=native, properties=props))),
                                  ('.pulse-owner', native['uid'])]:
            path = directory / filename
            path.write_text(content)
            path.chmod(0o600)
        self.inventory[name] = dict(Name=name, State=state, OS=osname, MACAddress=native['mac'])

    def run_repair(self):
        output = io.StringIO()
        with contextlib.redirect_stdout(output):
            code = self.r.run()
        self.assertNotIn('PRIVATE_PASSWORD', output.getvalue())
        return code, output.getvalue()

    def test_running_workers_repaired_without_restarting_jobs_or_companions(self):
        before = {p: p.read_bytes() for p in self.home.rglob('pulse.json')}
        code, output = self.run_repair()
        self.assertEqual(code, 0)
        self.assertEqual([name for name, _ in self.r.repairs], ['linux-primary'])
        self.assertFalse(self.r.started)
        self.assertFalse(any(c[:2] == ['tart', 'stop'] for c in self.r.commands))
        self.assertIn('existing job left running', output)
        self.assertIn('SKIP stopped base', output)
        self.assertEqual(before, {p: p.read_bytes() for p in before})

    def test_stopped_base_repaired_and_returned_to_stopped(self):
        self.args.include_stopped = True
        code, _ = self.run_repair()
        self.assertEqual(code, 0)
        self.assertEqual(set(self.r.started), {'linux-base'})
        self.assertEqual(self.inventory['linux-base']['State'], 'stopped')
        self.assertEqual(self.inventory['linux-primary']['State'], 'running')
        self.assertEqual({name for name, _ in self.r.repairs}, {'linux-primary', 'linux-base'})
        self.assertIn('test ! -e /var/db/pulse-tart/intent', dict(self.r.repairs)['linux-base'])

    def test_base_failure_is_reported_and_base_still_stops(self):
        self.args.include_stopped = True
        self.r.fail.add('linux-base')
        code, output = self.run_repair()
        self.assertEqual(code, 1)
        self.assertIn('FAILED linux-base', output)
        self.assertEqual(self.inventory['linux-base']['State'], 'stopped')
        self.assertEqual(self.inventory['linux-primary']['State'], 'running')

    def test_pulse_must_be_stopped_before_booting_sources(self):
        self.args.include_stopped = True
        with self.lock.open() as stream:
            fcntl.flock(stream.fileno(), fcntl.LOCK_EX | fcntl.LOCK_NB)
            with self.assertRaisesRegex(repair.RepairError, 'Stop Pulse normally'):
                self.run_repair()
        self.assertFalse(self.r.repairs)
        self.assertFalse(self.r.started)

    def test_check_only_makes_no_changes(self):
        self.args.include_stopped = self.args.check = True
        code, output = self.run_repair()
        self.assertEqual(code, 0)
        self.assertIn('Check only', output)
        self.assertFalse(self.r.repairs)
        self.assertFalse(self.r.started)

    def test_unrelated_stale_macos_base_and_companion_do_not_block_linux_repair(self):
        self.args.include_stopped = True
        name = 'pulse-id-565a157910ca985c18d3-macos-15-arm64-base'
        self.add_vm(name, 'stopped', {'image': {'kind': 'base', 'phase': 'ready'}},
                    reusable=True, osname='darwin')
        for unrelated in [name, 'macos-engine']:
            path = self.home / 'vms' / unrelated / 'pulse.json'
            data = json.loads(path.read_text())
            data['native']['inode'] += 1
            path.write_text(json.dumps(data))
            (path.parent / '.pulse-owner').unlink()
        before = {p: p.read_bytes() for p in self.home.rglob('pulse.json')}
        code, _ = self.run_repair()
        self.assertEqual(code, 0)
        self.assertEqual({name for name, _ in self.r.repairs}, {'linux-primary', 'linux-base'})
        self.assertEqual(before, {p: p.read_bytes() for p in before})
        self.assertFalse(any(c[:2] == ['tart', 'get'] and c[2] in [name, 'macos-engine']
                             for c in self.r.commands))

    def test_stale_linux_base_still_refuses_before_any_repair(self):
        self.args.include_stopped = True
        path = self.home / 'vms/linux-base/pulse.json'
        data = json.loads(path.read_text())
        data['native']['inode'] += 1
        path.write_text(json.dumps(data))
        with self.assertRaisesRegex(repair.RepairError, 'VM directory identity changed: linux-base'):
            self.run_repair()
        self.assertFalse(self.r.repairs)
        self.assertFalse(self.r.started)

    def test_replaced_vm_is_refused(self):
        plan = self.r.plan()
        path = self.home / 'vms/linux-primary/pulse.json'
        data = json.loads(path.read_text())
        data['native']['uid'] = 'replacement'
        path.write_text(json.dumps(data))
        (path.parent / '.pulse-owner').write_text('replacement')
        with self.assertRaisesRegex(repair.RepairError, 'VM was replaced'):
            self.r.repair_one(*plan[0])
        self.assertFalse(self.r.repairs)

    def test_vm_filter_does_not_repair_other_workers(self):
        self.args.vm = ['linux-primary']
        code, _ = self.run_repair()
        self.assertEqual(code, 0)
        self.assertEqual([name for name, _ in self.r.repairs], ['linux-primary'])
        self.args.vm = ['macos-engine']
        with self.assertRaisesRegex(repair.RepairError, 'not an owned Linux worker'):
            self.run_repair()


if __name__ == '__main__':
    unittest.main()
