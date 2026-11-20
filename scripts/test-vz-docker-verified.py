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
    source, app, home = out/'source', out/'PulseVZ.app', out/'network-home'
    runner, vmtool = app/'Contents/MacOS/pulse-vz', out/'network-vm'
    env = dict(os.environ, CGO_ENABLED='0', GOWORK='off', PULSE_VZ_RUNNER=str(runner), PULSE_VZ_TEST_CACHE=str(cache))
    result = {'result': 'incomplete', 'checks': {}, 'linux_source': str(base)}
    vm_attempted = registered = False
    status = 2
    def logged(command, log, **kwargs):
        with log.open('w') as stream:
            subprocess.run(command, stdout=stream, stderr=subprocess.STDOUT, check=True, **kwargs)
    def build(label):
        logged(['bash', 'scripts/build-vz-app.sh', str(app)], out/(label+'-build.log'), cwd=source, env=env)
        subprocess.run([LSREGISTER, '-f', str(app)], check=True, stdout=subprocess.DEVNULL)
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
        registered = True
        first = build('initial')
        result['initial_identity'] = first
        go_gate(source, out/'launchd', dict(env, PULSE_TEST_VZ_DOCKER_ENDPOINT='1'),
                'TestLiveVZDockerEndpoint', 'DOCKER_ENDPOINT_SERVICE_PASS:')
        logged(['go', 'build', '-o', str(vmtool), './scripts/vz-network-vm'], out/'vm-build.log', cwd=source, env=env)
        seed_home(base, home)
        print('Starting a disposable Linux VM; then allow Pulse VZ Local Network access if macOS asks.', flush=True)
        target = start_vm()
        go_gate(source, out/'network-initial', dict(env, PULSE_TEST_VZ_NETWORK_ENDPOINT='1', PULSE_TEST_VZ_NETWORK_TARGET=target),
                'TestLiveVZNetworkEndpoint', 'DOCKER_NETWORK_ENDPOINT_PASS:')
        result['checks']['initial_network_and_restart'] = True
        stop_vm()
        print('2/3 Rebuilding at the same app path. Permission should persist; do not approve a second network prompt during this check.', flush=True)
        second = build('rebuilt')
        result['rebuilt_identity'] = second
        if first['uuid'] == second['uuid'] or first['requirement'] != second['requirement']:
            raise RuntimeError('Rebuild must have a new UUID and the same signing requirement')
        target = start_vm()
        go_gate(source, out/'network-rebuilt', dict(env, PULSE_TEST_VZ_NETWORK_ENDPOINT='1', PULSE_TEST_VZ_NETWORK_TARGET=target, PULSE_TEST_VZ_NETWORK_TIMEOUT='20'),
                'TestLiveVZNetworkEndpoint', 'DOCKER_NETWORK_ENDPOINT_PASS:')
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
        if registered:
            subprocess.run([LSREGISTER, '-u', str(app)], capture_output=True)
        if cleanup_errors:
            status = 2; result['result'] = 'incomplete'; result['cleanup_errors'] = cleanup_errors
        (out/'verification.json').write_text(json.dumps(result, indent=2)+'\n')
        # Nested Docker qualification already has its own complete archive.
        def include(info):
            parts = Path(info.name).parts
            if any(part in ('home', 'network-home', 'PulseVZ.app', 'network-vm') for part in parts):
                return None
            if info.name.endswith('.tar.gz'):
                return None
            return info
        with tarfile.open(str(out)+'.tar.gz', 'w:gz') as archive:
            archive.add(out, arcname=out.name, filter=include)
        print('Verification:', result['result'], '\nEvidence archive:', str(out)+'.tar.gz', flush=True)
        lock.close()
    return status


if __name__ == '__main__':
    try:
        raise SystemExit(main())
    except (ValueError, OSError) as error:
        print('Verification could not start:', error, file=sys.stderr)
        raise SystemExit(2)
