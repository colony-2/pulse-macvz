import contextlib
import importlib.util
import io
import json
import os
from pathlib import Path
import subprocess
import sys
import tarfile
import tempfile
import unittest
from unittest.mock import patch

ROOT = Path(__file__).resolve().parent.parent
spec = importlib.util.spec_from_file_location('diagnose', ROOT / 'scripts/diagnose-tart.py')
diagnose = importlib.util.module_from_spec(spec)
spec.loader.exec_module(diagnose)

FAKE = r'''
import json, os, sys, time
from pathlib import Path
tool, args = Path(sys.argv[0]).name, sys.argv[1:]
root = Path(os.environ['DIAG_FIXTURE'])
with (root / 'calls.jsonl').open('a') as f:
    f.write(json.dumps([tool, *args]) + '\n')
if tool == 'tart':
    vms = json.loads((root / 'inventory.json').read_text())
    if args == ['--version']: print('tart 2.41.1')
    elif args[0] == 'list':
        if os.environ.get('DIAG_BAD_LIST'): print('{'); sys.exit(0)
        print(json.dumps(list(vms.values())))
    elif args[0] == 'get':
        if os.environ.get('DIAG_BAD_GET'): sys.exit(1)
        print(json.dumps(vms[args[1]]))
    elif args[0] == 'ip': print('192.0.2.1')
    else: sys.exit(88)
elif tool == 'ps':
    print('100 1 00:10 S /fixture/pulse')
    print('101 1 00:10 S /fixture/tart')
elif tool == 'lsof':
    if 'txt' in args: print('n/fixture/pulse')
    elif '-iTCP' in args: print('n*:8080')
    else: print('n' + str(root / 'pulse.log'))
elif tool == 'curl':
    assert args[-1] == 'http://127.0.0.1:8080/status'
    print(json.dumps({'status':'ok', 'version':'v-test', 'poll':{'passes':12, 'running':False},
                      'private':'SECRET_HTTP_CANARY'}))
elif tool == 'go':
    if 'build' in args:
        target = Path(args[args.index('-o') + 1])
        target.write_bytes(Path(sys.argv[0]).read_bytes())
        target.chmod(0o755)
        sys.exit(0)
    print('/fixture/pulse: go1.26.0')
    print('\tdep\tgithub.com/colony-2/pulse-tart\tv0.0.0-20261116035727-70a37956d130\th1:example')
elif tool == 'ssh-probe':
    request = json.load(sys.stdin)
    assert set(request) == {'address', 'username', 'password', 'host_key'}
    assert request['password'] == 'SECRET_PASSWORD_CANARY'
    print(json.dumps({'stage': 'ssh_handshake', 'outcome': 'failed', 'cause': 'authentication_failed', 'host_key_pinned': False}))
elif tool == 'sw_vers': print('26.6.2')
elif tool == 'sysctl': print('hw.ncpu: 8')
elif tool == 'uname': print('Darwin' if args == ['-s'] else 'arm64')
elif tool == 'ssh':
    assert os.environ['PULSE_DIAG_SSH_PASSWORD'] == 'SECRET_PASSWORD_CANARY'
    script = sys.stdin.read()
    assert script.startswith('set +x')
    if 'PULSE_JOB' in script:
        print('PULSE_JOB boot_id=00000000-0000-0000-0000-000000000001')
        print('PULSE_JOB mac=02:00:00:00:00:01')
        print('PULSE_TEMPLATE_READY {"mac":"02:00:00:00:00:01","boot_id":"00000000-0000-0000-0000-000000000001"}')
        print('PULSE_JOB intent=present\nPULSE_JOB fingerprint=present')
        print('PULSE_JOB pid_value=123\nPULSE_JOB worker_pid_alive=yes\nPULSE_JOB c2j_process=present')
        print('PULSE_JOB complete=absent\nPULSE_JOB worker.log_bytes=1024')
        print('PULSE_JOB lease=SECRET_GUEST_LEASE_CANARY')
        print('execution_lease_lost SECRET_WORKER_LOG_CANARY')
        sys.exit(0)
    print('PULSE_DIAG os_version=26.6.2')
    print('PULSE_DIAG template_ready=absent')
    print('PULSE_DIAG template_failed=present')
    print('PULSE_TEMPLATE_FAILED\nPermission denied\nSECRET_GUEST_CANARY')
else: sys.exit(89)
'''


class Diagnosis(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.home = self.root / 'tart'
        self.binary = self.root / 'bin'
        self.binary.mkdir()
        self.name = 'pulse-pool-macos-26-arm64-standard-4cpu-8192m-slot-0001'
        directory = self.home / 'vms' / self.name
        directory.mkdir(parents=True)
        props = {'version': 1, 'scope': 'scope', 'native': {
            'uid': 'uid', 'phase': 'started', 'delivered': False,
            'spec': {'name': self.name, 'os': 'darwin', 'username': 'admin',
                     'password': 'SECRET_PASSWORD_CANARY',
                     'startup_script': {'script_content': 'SECRET_SCRIPT_CANARY', 'env': {'SECRET': 'SECRET_ENV_CANARY'}}}},
            'properties': {'scope': 'scope', 'key': 'SECRET_KEY_CANARY',
                           'slot': {'id': 'standard-0001', 'name': self.name, 'phase': 'preparing', 'boot_id': ''}}}
        (directory / 'pulse.json').write_text(json.dumps(props))
        (directory / 'pulse.events').write_text('SECRET_EVENT_CANARY\nPULSE_TEMPLATE_READY {}\n')
        (directory / 'pulse.run.log').write_text('VZErrorDomain Code=12 SECRET_RUN_CANARY\n')
        (directory / 'config.json').write_text(json.dumps({'ecid': 'SECRET_ECID_CANARY'}))
        (self.root / 'inventory.json').write_text(json.dumps({
            self.name: dict(Name=self.name, State='running', OS='darwin', CPU=4, Memory=8192),
            'foreign': dict(Name='foreign', State='running', CPU=4, Memory=8192),
            'linux': dict(Name='linux', State='running', OS='linux', CPU=4, Memory=8192),
            'saved': dict(Name='saved', State='suspended', OS='darwin', CPU=4, Memory=8192),
        }))
        (self.root / 'pulse.log').write_text(json.dumps({
            'time': '2026-11-18T20:39:11Z', 'msg': 'launch result', 'service': 'tart',
            'status': 'no_capacity', 'lease': 'SECRET_LEASE_CANARY',
            'reason': 'physical macOS ceiling count=3 limit=2; SECRET_REASON_CANARY',
        }) + '\n')
        for tool in ('tart', 'ps', 'lsof', 'go', 'sw_vers', 'sysctl', 'uname', 'ssh', 'curl'):
            path = self.binary / tool
            path.write_text('#!' + sys.executable + '\n' + FAKE)
            path.chmod(0o755)
        self.env = dict(os.environ, PATH=str(self.binary) + os.pathsep + os.environ['PATH'],
                        DIAG_FIXTURE=str(self.root))

    def run_collector(self, *args, system='Darwin'):
        argv = ['diagnose', '--tart-home', str(self.home), '--samples', '2', '--interval', '0',
                '--output-root', str(self.root / 'results'), *args]
        with patch.dict(os.environ, self.env, clear=True), patch.object(sys, 'argv', argv), \
                patch.object(diagnose.platform, 'system', return_value=system), \
                patch.object(diagnose.platform, 'machine', return_value='arm64'), \
                contextlib.redirect_stdout(io.StringIO()):
            status = diagnose.main()
        output = next(path for path in (self.root / 'results').iterdir() if path.is_dir())
        return status, output

    def test_archive_contains_useful_evidence_without_private_data_or_mutation(self):
        before = {p: p.read_bytes() for p in self.home.rglob('*') if p.is_file()}
        status, output = self.run_collector()
        self.assertEqual(status, 0)
        summary = json.loads((output / 'summary.json').read_text())
        self.assertEqual(summary['capacity']['macos_including_unknown'], 2)
        self.assertEqual(summary['capacity']['with_one_more_macos'], 3)
        self.assertEqual(summary['changed_vms'], [])
        self.assertEqual(summary['sample_count'], 2)
        snapshot = json.loads((output / 'snapshot-01.json').read_text())
        vm = next(v for v in snapshot['vms'] if v['name'] == self.name)
        self.assertEqual(vm['files']['pulse.run.log']['vz_error_codes'], ['12'])
        self.assertEqual(vm['files']['pulse.events']['invalid_template_markers'], 1)
        self.assertTrue(snapshot['pulse_logs'])
        self.assertEqual(snapshot['pulse_status'][0]['poll']['passes'], 12)
        build = json.loads((output / 'pulse-builds.json').read_text())[0]
        self.assertEqual(build['resolved_executable'], '/fixture/pulse')
        self.assertTrue(build['build'])
        guest = json.loads((output / 'guest-builders.json').read_text())[0]
        self.assertIn('template_failed=present', guest['checks'])
        discovery = json.loads((output / 'updater-discovery.json').read_text())
        self.assertEqual(discovery['exit_code'], 0)
        self.assertIn(self.name, discovery['targets'])  # Include annotated unfinished builders.
        with tarfile.open(str(output) + '.tar.gz') as archive:
            for member in archive.getmembers():
                if member.isfile():
                    self.assertNotIn(b'SECRET_', archive.extractfile(member).read(), member.name)
        calls = [json.loads(line) for line in (self.root / 'calls.jsonl').read_text().splitlines()]
        self.assertTrue(all(call[1] in ('--version', 'list', 'get', 'ip') for call in calls if call[0] == 'tart'))
        self.assertEqual(before, {p: p.read_bytes() for p in self.home.rglob('*') if p.is_file()})

    def test_inventory_failure_and_preflight_failure_still_archive(self):
        self.env['DIAG_BAD_LIST'] = '1'
        status, output = self.run_collector('--skip-guest')
        self.assertEqual(status, 1)
        summary = json.loads((output / 'summary.json').read_text())
        self.assertEqual(summary['collection'], 'partial')
        self.assertTrue(Path(str(output) + '.tar.gz').exists())

    def make_job(self):
        name = 'pulse-job-test'
        directory = self.home / 'vms' / name
        directory.mkdir()
        data = json.loads((self.home / 'vms' / self.name / 'pulse.json').read_text())
        data['native']['spec']['name'] = name
        data['native']['spec'].update(cpu=4, memory=8192)
        data['native'].update(device=directory.stat().st_dev, inode=directory.stat().st_ino, mac='02:00:00:00:00:01')
        (directory / '.pulse-owner').write_text(data['native']['uid'])
        data['properties'].pop('slot')
        data['properties']['job'] = {
            'name': name, 'launch_id': 'test-launch', 'slot_id': 'standard-0001',
            'job': {'instance_id': 'instance', 'tenant_id': 'c2', 'job_id': 'job-id',
                    'jobdb': 'SECRET_JOBDB_CANARY'}, 'lease': 'SECRET_JOB_LEASE_CANARY'}
        (directory / 'pulse.json').write_text(json.dumps(data))
        (directory / 'pulse.events').write_text('PULSE_BOOTSTRAP_OK\nPULSE_HANDOFF_OK\n')
        inventory_path = self.root / 'inventory.json'
        inventory = json.loads(inventory_path.read_text())
        inventory[name] = dict(Name=name, State='running', OS='darwin', CPU=4, Memory=8192, MACAddress='02:00:00:00:00:01')
        inventory_path.write_text(json.dumps(inventory))
        return name

    def test_jobs_collect_each_snapshot_without_exposing_payload_or_changing_vms(self):
        name = self.make_job()
        before = {p: p.read_bytes() for p in self.home.rglob('*') if p.is_file()}
        status, output = self.run_collector('--jobs')
        self.assertEqual(status, 0)
        for index in (1, 2):
            snapshot = json.loads((output / f'snapshot-{index:02d}.json').read_text())
            job = snapshot['job_observations'][0]
            self.assertEqual(job['name'], name)
            self.assertEqual(job['stage'], 'worker_process_observed')
            self.assertEqual(job['launch_id'], 'test-launch')
            self.assertEqual(job['job_ref'], {'instance_id': 'instance', 'tenant_id': 'c2', 'job_id': 'job-id'})
            guest = snapshot['guest_jobs'][0]
            self.assertTrue(guest['snapshot_identity_matches'])
            self.assertEqual(guest['diagnostics']['log_mentions']['lease_loss_or_expiry'], 1)
        self.assertEqual(json.loads((output / 'summary.json').read_text())['observed_job_launches'], ['test-launch'])
        with tarfile.open(str(output) + '.tar.gz') as archive:
            for member in archive.getmembers():
                if member.isfile():
                    self.assertNotIn(b'SECRET_', archive.extractfile(member).read(), member.name)
        calls = [json.loads(line) for line in (self.root / 'calls.jsonl').read_text().splitlines()]
        self.assertEqual(sum(call[:3] == ['tart', 'ip', name] for call in calls), 2)
        self.assertTrue(all(call[1] in ('--version', 'list', 'get', 'ip') for call in calls if call[0] == 'tart'))
        self.assertEqual(before, {p: p.read_bytes() for p in self.home.rglob('*') if p.is_file()})

    def test_job_guest_checks_are_opt_in(self):
        name = self.make_job()
        self.run_collector()
        calls = [json.loads(line) for line in (self.root / 'calls.jsonl').read_text().splitlines()]
        self.assertFalse(any(call[:3] == ['tart', 'ip', name] for call in calls))

    def test_delivery_probe_retains_failed_stage_without_private_settings_or_mutation(self):
        self.make_job()
        before = {p: p.read_bytes() for p in self.home.rglob('*') if p.is_file()}
        status, output = self.run_collector('--delivery')
        self.assertEqual(status, 0)
        snapshot = json.loads((output / 'snapshot-01.json').read_text())
        probe = snapshot['delivery_probes'][0]
        self.assertTrue(all(probe['local_checks'].values()))
        self.assertEqual(probe['go_ssh']['cause'], 'authentication_failed')
        self.assertEqual(probe['go_ssh']['stage'], 'ssh_handshake')
        with tarfile.open(str(output) + '.tar.gz') as archive:
            for member in archive.getmembers():
                if member.isfile():
                    self.assertNotIn(b'SECRET_', archive.extractfile(member).read(), member.name)
        self.assertEqual(before, {p: p.read_bytes() for p in self.home.rglob('*') if p.is_file()})

    def test_delivery_probe_refuses_changed_directory_binding(self):
        name = self.make_job()
        path = self.home / 'vms' / name / 'pulse.json'
        data = json.loads(path.read_text())
        data['native']['inode'] += 1
        path.write_text(json.dumps(data))
        status, output = self.run_collector('--delivery', '--samples', '1')
        self.assertEqual(status, 0)
        probe = json.loads((output / 'snapshot-01.json').read_text())['delivery_probes'][0]
        self.assertEqual(probe['observation'], 'local_precondition_failed')
        calls = [json.loads(line) for line in (self.root / 'calls.jsonl').read_text().splitlines()]
        self.assertFalse(any(call[0] == 'ssh-probe' for call in calls))

    def test_skip_guest_overrides_jobs_but_preserves_host_job_evidence(self):
        self.make_job()
        status, output = self.run_collector('--delivery', '--skip-guest')
        self.assertEqual(status, 0)
        snapshot = json.loads((output / 'snapshot-01.json').read_text())
        self.assertEqual(snapshot['job_observations'][0]['stage'], 'handoff_observed')
        self.assertEqual(snapshot['job_observations'][0]['guest_observation'], 'not_probed')
        calls = [json.loads(line) for line in (self.root / 'calls.jsonl').read_text().splitlines()]
        self.assertFalse(any(call[0] == 'ssh' for call in calls))
        self.assertFalse(any(call[0] == 'ssh-probe' or (call[0] == 'go' and 'build' in call) for call in calls))

    def test_job_stages_distinguish_restore_delivery_and_worker_exit(self):
        vm = {'name': 'job', 'properties': {'job': {'launch_id': 'launch'}}}
        self.assertEqual(diagnose.job_observation(vm)['stage'], 'delivery_not_observed')
        vm['files'] = {'pulse.events': diagnose.evidence('PULSE_TEMPLATE_RESTORE_FAILED')}
        self.assertEqual(diagnose.job_observation(vm)['stage'], 'restore_identity_failure_observed')
        vm['files'] = {'pulse.events': diagnose.evidence('PULSE_HANDOFF_OK')}
        guest = {'checks': diagnose.job_checks('PULSE_JOB complete_value=1\n')}
        self.assertEqual(diagnose.job_observation(vm, guest)['stage'], 'worker_exit_observed')
        self.assertEqual(diagnose.job_observation(vm, guest)['worker_checks']['complete_value'], '1')
        self.assertEqual(diagnose.job_checks('PULSE_JOB guest_service=not_loaded\n'), {'guest_service': 'not_loaded'})

    def test_wrong_platform_still_archives(self):
        status, output = self.run_collector(system='Linux')
        self.assertEqual(status, 1)
        self.assertTrue(Path(str(output) + '.tar.gz').exists())
        self.assertEqual(json.loads((output / 'summary.json').read_text())['sample_count'], 0)

    def test_bad_vm_inspection_is_reported_as_partial(self):
        self.env['DIAG_BAD_GET'] = '1'
        status, output = self.run_collector('--skip-guest')
        self.assertEqual(status, 1)
        self.assertEqual(json.loads((output / 'summary.json').read_text())['collection'], 'partial')

    def test_marker_validation_matches_provider(self):
        identity = {'mac': '02:00:00:00:00:01', 'boot_id': '00000000-0000-0000-0000-000000000001'}
        self.assertEqual(len(diagnose.evidence('PULSE_TEMPLATE_READY ' + json.dumps(identity))['template_identities']), 1)
        identity['mac'] = '03:00:00:00:00:01'
        self.assertEqual(diagnose.evidence('PULSE_TEMPLATE_READY ' + json.dumps(identity))['invalid_template_markers'], 1)

    def test_reservations_deduplicate_pair_members_and_include_missing_member(self):
        pair = {'ID': 'p', 'Phase': 'ready', 'Primary': {'name': 'primary', 'os': 'darwin'},
                'Engine': {'name': 'engine', 'os': 'linux'}}
        vms = [{'name': 'primary', 'tart': {'State': 'suspended', 'OS': 'darwin'}, 'properties': {'pair': pair}},
               {'name': 'engine', 'tart': {'State': 'running', 'OS': 'linux'}, 'properties': {'pair': pair}}]
        out = diagnose.capacity(vms)
        self.assertEqual(out['macos_including_unknown'], 1)
        self.assertEqual(len(out['reservation_candidates']), 2)
        self.assertEqual(diagnose.capacity(vms[1:])['macos_including_unknown'], 1)

    def test_timeout_is_bounded_and_retained(self):
        args = type('Args', (), dict(tart_home=self.home, pulse_log=[], command_timeout=0.1))()
        collector = diagnose.Collector(args, self.root)
        self.assertIsNone(collector.command([sys.executable, '-c', 'import time; time.sleep(30)']))
        self.assertEqual(collector.commands[-1]['error_type'], 'TimeoutExpired')

    def test_skip_guest_does_not_ssh(self):
        status, _ = self.run_collector('--skip-guest')
        self.assertEqual(status, 0)
        calls = [json.loads(line) for line in (self.root / 'calls.jsonl').read_text().splitlines()]
        self.assertFalse(any(call[0] == 'ssh' for call in calls))

    def test_guest_probe_skips_job_and_suspended_sources(self):
        args = type('Args', (), dict(tart_home=self.home, pulse_log=[], command_timeout=1))()
        collector = diagnose.Collector(args, self.root)
        vm = {'name': self.name, 'tart': {'State': 'running'}, 'properties': {
            'spec': {'os': 'darwin'}, 'slot': {'phase': 'preparing'}, 'job': {'name': self.name}}}
        self.assertIsNone(collector.guest(vm))
        del vm['properties']['job']
        vm['tart']['State'] = 'suspended'
        self.assertIsNone(collector.guest(vm))
        self.assertEqual(collector.commands, [])

    def test_stopped_pending_run_is_not_reported_as_free_capacity(self):
        vm = {'name': 'pending', 'tart': {'State': 'stopped', 'OS': 'darwin'},
              'properties': {'native': {'phase': 'started'}}}
        self.assertEqual(diagnose.capacity([vm])['macos_including_unknown'], 1)
        vm['files'] = {'pulse.exit': {'exit_code': 1}}
        self.assertEqual(diagnose.capacity([vm])['macos_including_unknown'], 0)

    def test_bare_process_name_never_inspects_a_different_binary_from_path(self):
        args = type('Args', (), dict(tart_home=self.home, pulse_log=[], command_timeout=1))()
        collector = diagnose.Collector(args, self.root)
        with patch.object(collector, 'command', return_value='') as command:
            result = collector.pulse([{'pid': '123', 'executable': 'pulse'}])
        self.assertIsNone(result[0]['resolved_executable'])
        self.assertFalse(any(call.args[0][0] == 'go' for call in command.call_args_list))


if __name__ == '__main__':
    unittest.main()
