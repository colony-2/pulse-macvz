#!/usr/bin/env python3
"""Gate Docker qualification on bundled launchd-to-guest connectivity and rebuilds."""
import fcntl
import hashlib
import json
import os
from pathlib import Path
import platform
import plistlib
import shutil
import struct
import subprocess
import sys
import tarfile
import tempfile

ROOT = Path(__file__).resolve().parent.parent
LSREGISTER = '/System/Library/Frameworks/CoreServices.framework/Frameworks/LaunchServices.framework/Support/lsregister'


def select_linux_base(cache, explicit=None):
    candidates = [Path(explicit)] if explicit else []
    if not explicit:
        for manifest in sorted(cache.glob('*/sources.json'), key=lambda p: p.stat().st_mtime, reverse=True):
            for name in json.loads(manifest.read_text()):
                if Path(name).name != name:
                    raise ValueError('Invalid cached source name')
                candidates.append(manifest.parent / name)
    for candidate in candidates:
        config = json.loads((candidate / 'config.json').read_text())
        if config.get('os') == 'linux' and config.get('arch') == 'arm64' and not (candidate / 'state.vzvmsave').exists():
            return candidate.resolve()
    raise ValueError('No prepared Linux source found; set PULSE_VZ_NETWORK_BASE to an absolute cold Linux image directory.')


def seed_home(base, home):
    seed = home / 'vms/seed'
    seed.mkdir(parents=True, mode=0o700)
    with (base / 'disk.img').open('rb') as disk:
        fcntl.flock(disk, fcntl.LOCK_SH | fcntl.LOCK_NB)
        for name in ('config.json', 'disk.img', 'nvram.bin'):
            source = base / name
            if source.is_symlink() or not source.is_file():
                raise ValueError('Image files must be regular files')
            subprocess.run(['/bin/cp', '-c', str(source), str(seed/name)], check=True)
            (seed/name).chmod(0o600)


def identity(binary):
    data = binary.read_bytes()
    if data[:4] != b'\xcf\xfa\xed\xfe':
        raise ValueError('Expected a 64-bit Mach-O runner')
    offset, uuid = 32, None
    for _ in range(struct.unpack_from('<I', data, 16)[0]):
        command, size = struct.unpack_from('<II', data, offset)
        if size < 8 or offset+size > len(data):
            raise ValueError('Invalid Mach-O command')
        if command == 0x1b:
            uuid = data[offset+8:offset+24].hex()
        offset += size
    if uuid is None or uuid == '0'*32:
        raise ValueError('Runner lacks a unique Mach-O UUID')
    signed = subprocess.run(['codesign', '-dvv', '-r-', str(binary)], capture_output=True, text=True, check=True)
    lines = (signed.stdout+signed.stderr).splitlines()
    requirement = next((line for line in lines if line.startswith('designated =>')), None)
    if requirement is None or not any(line.startswith('TeamIdentifier=') and not line.endswith('not set') for line in lines):
        raise ValueError('Rebuild-consent verification requires an Apple-issued signing identity; ad-hoc signing is insufficient.')
    return {'uuid': uuid, 'requirement': requirement, 'sha256': hashlib.sha256(data).hexdigest()}


def gate_passed(events, test, marker):
    return (any(e.get('Action') == 'pass' and e.get('Test') == test for e in events)
            and not any(e.get('Action') == 'fail' for e in events)
            and any(marker in e.get('Output', '') for e in events))


def go_gate(source, stage, env, test, marker):
    stage.mkdir()
    env = dict(env, PULSE_TEST_OUTPUT_DIR=str(stage))
    events = []
    with (stage/'tests.jsonl').open('w') as raw:
        process = subprocess.Popen(['go', 'test', './internal/local', '-run', '^'+test+'$', '-json', '-count=1', '-timeout=150s'],
                                   cwd=source, env=env, stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True)
        try:
            for line in process.stdout:
                raw.write(line); raw.flush()
                try:
                    event = json.loads(line)
                except json.JSONDecodeError:
                    print(line, end='', flush=True)
                    continue
                events.append(event)
                if event.get('Output'):
                    print(event['Output'], end='', flush=True)
            code = process.wait()
        finally:
            if process.poll() is None:
                process.terminate()
                try:
                    process.wait(timeout=10)
                except subprocess.TimeoutExpired:
                    process.kill(); process.wait()
    if code or not gate_passed(events, test, marker):
        raise RuntimeError(stage.name+' failed; Docker qualification was not started. See '+str(stage))


def registration_evidence(app, output):
    # Query LaunchServices through its public API. Keep evidence even if the
    # hosting session cannot reach LaunchServices (for example under Seatbelt).
    code = r'''ObjC.import('AppKit');
var urls = $.NSWorkspace.sharedWorkspace.URLsForApplicationsWithBundleIdentifier('dev.pulse.provider.qualification');
var paths = [];
for (var i = 0; i < urls.count; ++i) paths.push(ObjC.unwrap(urls.objectAtIndex(i).path));
JSON.stringify(paths);'''
    p = subprocess.run(['/usr/bin/osascript', '-l', 'JavaScript', '-e', code], capture_output=True, text=True, timeout=15)
    evidence = {'expected': str(app), 'exit': p.returncode, 'stdout': p.stdout, 'stderr': p.stderr}
    output.write_text(json.dumps(evidence, indent=2)+'\n')
    if p.returncode == 0 and p.stdout.strip().startswith('['):
        paths = json.loads(p.stdout)
        if paths != [str(app)]:
            raise RuntimeError('LaunchServices does not resolve exactly the verification app; see '+str(output))


def require_no_forwarding_denials(stage):
    # The VM tool has already established direct SSH readiness. Reject a
    # connectivity failure even if a late Allow click makes a retry succeed.
    diagnostics = json.loads((stage/'endpoint-preflight.json').read_text())
    if not diagnostics or any('service.log' not in service for service in diagnostics.values()):
        raise RuntimeError('Missing rebuilt bridge log; cannot verify consent persistence')
    for service in diagnostics.values():
        if 'bridge forwarding failed:' in service.get('service.log', ''):
            raise RuntimeError('Rebuilt app required connectivity retries; consent persistence is not verified. See '+str(stage))


def collect_identity_logs(out):
    predicate = '(process == "nehelper" OR process == "tccd" OR process == "lsd") AND (eventMessage CONTAINS[c] "dev.pulse.provider.qualification" OR eventMessage CONTAINS[c] "PulseProvider.app")'
    try:
        with (out/'identity-system.log').open('w') as log:
            subprocess.run(['/usr/bin/log', 'show', '--last', '3m', '--info', '--debug', '--style', 'compact', '--predicate', predicate], stdout=log, stderr=subprocess.STDOUT, timeout=20)
    except Exception as error:
        (out/'identity-log-error.txt').write_text(str(error)+'\n')


def main():
    if platform.system() != 'Darwin' or platform.machine() != 'arm64' or len(sys.argv) != 2 or not Path(sys.argv[1]).is_absolute():
        raise ValueError('Usage: python3 scripts/test-vz-docker-verified.py /absolute/prepared-macos-base (Apple Silicon macOS)')
    os.umask(0o077)
    cache = Path(os.environ.get('PULSE_VZ_TEST_CACHE', str(ROOT/'.local/vz-qualification-cache')))
    if not cache.is_absolute():
        raise ValueError('This verification requires an absolute PULSE_VZ_TEST_CACHE')
    base = select_linux_base(cache, os.environ.get('PULSE_VZ_NETWORK_BASE'))
    cache.mkdir(parents=True, exist_ok=True)
    lock = (cache/'verification.lock').open('a+')
    fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
    (ROOT/'test-results').mkdir(exist_ok=True)
    out = Path(tempfile.mkdtemp(prefix='vz-verified.', dir=ROOT/'test-results'))
    print('Verification artifacts:', out, flush=True)
    source, home = out/'source', out/'network-home'
    # One installation per checkout; archives must not become competing apps.
    app_root = ROOT/'.local/vz-verification'
    app_root.mkdir(parents=True, exist_ok=True)
    app_lock = (app_root/'lock').open('a+')
    fcntl.flock(app_lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
    app = app_root/'PulseProvider.app'
    runner, vmtool = app/'Contents/MacOS/pulse-macvz', out/'network-vm'
    env = dict(os.environ, CGO_ENABLED='0', GOWORK='off', PULSE_VZ_RUNNER=str(runner), PULSE_VZ_TEST_CACHE=str(cache), PULSE_APP_FLAVOR='qualification')
    result = {'result': 'incomplete', 'checks': {}, 'linux_source': str(base),
              'app': str(app), 'bundle_id': 'dev.pulse.provider.qualification'}
    vm_attempted = False
    status = 2
    def logged(command, log, **kwargs):
        with log.open('w') as stream:
            subprocess.run(command, stdout=stream, stderr=subprocess.STDOUT, check=True, **kwargs)
    def build(label):
        logged(['bash', 'scripts/build-server-app.sh', str(app)], out/(label+'-build.log'), cwd=source, env=env)
        subprocess.run([LSREGISTER, '-f', str(app)], check=True, stdout=subprocess.DEVNULL)
        registration_evidence(app, out/(label+'-registration.json'))
        return identity(runner)
    def start_vm():
        nonlocal vm_attempted
        vm_attempted = True
        logged([str(vmtool), 'start', str(home), str(runner)], out/'vm-start.log', cwd=source, env=env)
        return (home/'target').read_text().strip()
    def stop_vm():
        nonlocal vm_attempted
        if vm_attempted:
            logged([str(vmtool), 'stop', str(home), str(runner)], out/'vm-stop.log', cwd=source, env=env)
            vm_attempted = False
    try:
        subprocess.run(['python3', 'scripts/test-report.py', 'snapshot', str(out)], cwd=ROOT, check=True)
        print('1/3 Building and checking the packaged launchd runner.', flush=True)
        first = build('initial')
        result['initial_identity'] = first
        go_gate(source, out/'launchd', dict(env, PULSE_TEST_VZ_DOCKER_ENDPOINT='1'),
                'TestLiveVZDockerEndpoint', 'DOCKER_ENDPOINT_SERVICE_PASS:')
        logged(['go', 'build', '-o', str(vmtool), './scripts/vz-network-vm'], out/'vm-build.log', cwd=source, env=env)
        seed_home(base, home)
        print('Starting a disposable Linux VM; then allow Pulse Provider Verification Local Network access if macOS asks.', flush=True)
        target = start_vm()
        go_gate(source, out/'network-initial', dict(env, PULSE_TEST_VZ_NETWORK_ENDPOINT='1', PULSE_TEST_VZ_NETWORK_TARGET=target),
                'TestLiveVZNetworkEndpoint', 'DOCKER_NETWORK_ENDPOINT_PASS:')
        result['checks']['initial_network_and_restart'] = True
        stop_vm()
        print('2/3 Rebuilding at the same app path. Permission must persist. If another Local Network prompt appears, leave it unanswered until this check ends; the report will record the failure.', flush=True)
        second = build('rebuilt')
        result['rebuilt_identity'] = second
        if first['uuid'] == second['uuid'] or first['requirement'] != second['requirement']:
            raise RuntimeError('Rebuild must have a new UUID and the same signing requirement')
        target = start_vm()
        go_gate(source, out/'network-rebuilt', dict(env, PULSE_TEST_VZ_NETWORK_ENDPOINT='1', PULSE_TEST_VZ_NETWORK_TARGET=target, PULSE_TEST_VZ_NETWORK_TIMEOUT='20'),
                'TestLiveVZNetworkEndpoint', 'DOCKER_NETWORK_ENDPOINT_PASS:')
        require_no_forwarding_denials(out/'network-rebuilt')
        result['checks']['rebuilt_network_and_restart'] = True
        stop_vm()
        print('3/3 Network gates passed. Starting full Docker qualification with the verified runner and prepared-source cache.', flush=True)
        status = subprocess.call(['bash', 'scripts/test-vz-docker-qualification.sh', sys.argv[1]], cwd=source, env=env)
        result['checks']['docker_qualification'] = status == 0
        result['result'] = 'passed' if status == 0 else 'incomplete'
    except (Exception, KeyboardInterrupt) as error:
        status = 2
        result['error'] = str(error) or type(error).__name__
        print('Verification stopped:', result['error'], flush=True)
    finally:
        cleanup_errors = []
        try:
            stop_vm()
        except Exception as error:
            cleanup_errors.append(str(error))
        for service_file in out.glob('*/endpoint-service.txt'):
            service = service_file.read_text().strip()
            if service.startswith(f'user/{os.getuid()}/dev.pulse-vz.engine.'):
                subprocess.run(['/bin/launchctl', 'bootout', service], capture_output=True)
        # Keep this single installed app registered between runs. Unregistering
        # or accumulating app copies is not representative of an app upgrade.
        if status != 0:
            collect_identity_logs(out)
        if cleanup_errors:
            status = 2; result['result'] = 'incomplete'; result['cleanup_errors'] = cleanup_errors
        (out/'verification.json').write_text(json.dumps(result, indent=2)+'\n')
        # Nested Docker qualification already has its own complete archive.
        def include(info):
            parts = Path(info.name).parts
            if any(part in ('home', 'network-home', 'PulseProvider.app', 'network-vm') for part in parts):
                return None
            if info.name.endswith('.tar.gz'):
                return None
            return info
        with tarfile.open(str(out)+'.tar.gz', 'w:gz') as archive:
            archive.add(out, arcname=out.name, filter=include)
        print('Verification:', result['result'], '\nEvidence archive:', str(out)+'.tar.gz', flush=True)
        if result['checks'].get('initial_network_and_restart') and not result['checks'].get('rebuilt_network_and_restart'):
            print('The rebuild check has ended. If its Local Network prompt is still open for Pulse Provider Verification, you may Allow it now; the failed result is already saved.', flush=True)
        app_lock.close()
        lock.close()
    return status


if __name__ == '__main__':
    try:
        raise SystemExit(main())
    except (ValueError, OSError) as error:
        print('Verification could not start:', error, file=sys.stderr)
        raise SystemExit(2)
