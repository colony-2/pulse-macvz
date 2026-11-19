#!/usr/bin/env python3
"""Collect repeatable, read-only evidence from an existing Pulse/Tart worker."""
import argparse
import base64
import datetime
import hashlib
import ipaddress
import json
import os
from pathlib import Path
import platform
import re
import shutil
import signal
import stat
import subprocess
import tarfile
import tempfile
import time

ROOT = Path(__file__).resolve().parent.parent
LIMIT = 1024 * 1024
NAME = re.compile(r'^[A-Za-z0-9][A-Za-z0-9._-]*$')
UUID = re.compile(r'^[0-9a-fA-F]{8}(?:-[0-9a-fA-F]{4}){3}-[0-9a-fA-F]{12}$')
MAC = re.compile(r'^(?:[0-9a-fA-F]{2}:){5}[0-9a-fA-F]{2}$')
SIGNALS = (
    'PULSE_BASE_READY', 'PULSE_BASE_FAILED', 'PULSE_TEMPLATE_FAILED',
    'PULSE_HANDOFF_OK', 'PULSE_DOCKER_ENGINE_READY',
    'missing valid PULSE_TEMPLATE_READY identity', 'identity mismatch',
    'MAC duplicates another slot', 'MAC collision', 'no prepared record',
    'reservation occupied', 'source-referencing clone observed',
    'template suspension invalid', 'context deadline exceeded',
    'context canceled', 'Permission denied', 'Connection refused',
    'Operation timed out', 'No route to host', 'No space left on device',
    'No such file or directory', 'Host key verification failed',
    'a password is required', 'not allowed to execute', 'PULSE_IMAGE_PREPARED',
    'PULSE_RESTORE_FAILED', 'not ready', 'Cannot identify a reusable source',
    'No matching prepared macOS template pools', 'Cannot infer the recipe',
    'PULSE_TEMPLATE_RESTORE_FAILED', 'PULSE_BOOTSTRAP_OK', 'PULSE_WORKER_OK', 'PULSE_WORKER_FAILED',
    'worker executable unavailable', 'worker start failed', 'delivery deadline expired',
    'invalid launch envelope', 'execution_lease_lost', 'last_known_lease_expired',
)

# This is only sent to an already-running, unfinished macOS source builder.
# Output stays in memory until converted to allowlisted observations.
GUEST_PROBE = r'''set +x
printf 'PULSE_DIAG os_version=%s\n' "$(/usr/bin/sw_vers -productVersion)"
printf 'PULSE_DIAG boot_id=%s\n' "$(/usr/sbin/sysctl -n kern.bootsessionuuid)"
for kind in base template; do
  for f in ready failed; do
    if test -f /var/db/pulse-tart-$kind/$f; then
      printf 'PULSE_DIAG %s_%s=present\n' "$kind" "$f"
    else
      printf 'PULSE_DIAG %s_%s=absent\n' "$kind" "$f"
    fi
  done
  if launchctl print system/dev.pulse-tart.$kind >/dev/null 2>&1; then
    printf 'PULSE_DIAG %s_service=loaded\n' "$kind"
    launchctl print system/dev.pulse-tart.$kind 2>/dev/null | awk -v kind="$kind" '
      /^[[:space:]]*state =/ {print "PULSE_DIAG " kind "_service_state=" $3}
      /^[[:space:]]*last exit code =/ {print "PULSE_DIAG " kind "_service_exit=" $5}'
  else
    printf 'PULSE_DIAG %s_service=not_loaded\n' "$kind"
  fi
  tail -c 65536 /var/log/pulse-tart-$kind.log 2>/dev/null || true
done
if test -f /var/db/pulse-tart-template/ready; then
  printf 'PULSE_TEMPLATE_READY '
  cat /var/db/pulse-tart-template/ready
fi
for tool in c2j pulse-tart-guest; do
  if test -x /opt/pulse-tart/bin/$tool; then
    printf 'PULSE_DIAG %s=executable\n' "$tool"
  else
    printf 'PULSE_DIAG %s=missing\n' "$tool"
  fi
done
'''

# Observe an existing job only. Never bootstrap, ingest, replay a lease, or start
# c2j execution. Payload files are only checked for existence, never read.
JOB_PROBE = r'''set +x
printf 'PULSE_JOB boot_id=%s\n' "$(/usr/sbin/sysctl -n kern.bootsessionuuid)"
/sbin/ifconfig en0 | awk '/ether / {print "PULSE_JOB mac=" $2}'
if test -f /var/db/pulse-tart-template/ready; then
  printf 'PULSE_TEMPLATE_READY '
  cat /var/db/pulse-tart-template/ready
fi
for f in intent fingerprint launch.json pid complete worker.log; do
  if test -f /var/db/pulse-tart/$f; then
    printf 'PULSE_JOB %s=present\n' "$f"
  else
    printf 'PULSE_JOB %s=absent\n' "$f"
  fi
done
for f in pid complete; do
  value=$(head -c 32 /var/db/pulse-tart/$f 2>/dev/null)
  if [[ $value =~ ^-?[0-9]+$ ]]; then printf 'PULSE_JOB %s_value=%s\n' "$f" "$value"; fi
done
pid=$(head -c 32 /var/db/pulse-tart/pid 2>/dev/null)
if [[ $pid =~ ^[1-9][0-9]*$ ]] && kill -0 "$pid" 2>/dev/null; then
  echo 'PULSE_JOB worker_pid_alive=yes'
else
  echo 'PULSE_JOB worker_pid_alive=no'
fi
if /usr/bin/pgrep -x c2j >/dev/null 2>&1; then
  echo 'PULSE_JOB c2j_process=present'
else
  echo 'PULSE_JOB c2j_process=absent'
fi
if launchctl print system/dev.pulse-tart.guest >/dev/null 2>&1; then
  echo 'PULSE_JOB guest_service=loaded'
  launchctl print system/dev.pulse-tart.guest 2>/dev/null | awk '
    /^[[:space:]]*state =/ {print "PULSE_JOB guest_service_state=" $3}
    /^[[:space:]]*last exit code =/ {print "PULSE_JOB guest_service_exit=" $5}'
else
  echo 'PULSE_JOB guest_service=not_loaded'
fi
for f in /var/db/pulse-tart/worker.log /var/log/pulse-tart-guest.log; do
  if test -f "$f"; then
    printf 'PULSE_JOB %s_bytes=%s\n' "$(basename "$f")" "$(stat -f %z "$f")"
    printf 'PULSE_JOB %s_mtime=%s\n' "$(basename "$f")" "$(stat -f %m "$f")"
    tail -c 65536 "$f"
    printf '\n'
  fi
done
'''


def job_checks(text):
    return dict(re.findall(
        r'^PULSE_JOB ((?:boot_id|mac|intent|fingerprint|launch\.json|pid|complete|worker\.log|'
        r'pid_value|complete_value|worker_pid_alive|c2j_process|guest_service|guest_service_state|'
        r'guest_service_exit|(?:worker\.log|pulse-tart-guest\.log)_(?:bytes|mtime)))='
        r'([a-zA-Z0-9_.:-]{1,64})$', text, re.MULTILINE))


def job_observation(vm, guest=None):
    props = vm.get('properties', {})
    job = props.get('job')
    if not job:
        return None
    signals = vm.get('files', {}).get('pulse.events', {}).get('signals', {})
    checks = (guest or {}).get('checks', {})
    stage = 'delivery_not_observed'
    if signals.get('PULSE_TEMPLATE_RESTORE_FAILED'):
        stage = 'restore_identity_failure_observed'
    elif 'complete_value' in checks:
        stage = 'worker_exit_observed'
    elif signals.get('PULSE_WORKER_OK') or signals.get('PULSE_WORKER_FAILED'):
        stage = 'worker_exit_marker_observed'
    elif checks.get('intent') == 'present':
        stage = 'worker_intent_observed'
        if checks.get('worker_pid_alive') == 'yes' and checks.get('c2j_process') == 'present':
            stage = 'worker_process_observed'
    elif signals.get('PULSE_HANDOFF_OK') or checks.get('fingerprint') == 'present':
        stage = 'handoff_observed'
    elif signals.get('PULSE_BOOTSTRAP_OK') or checks.get('guest_service') == 'loaded':
        stage = 'guest_service_observed'
    return {'name': vm['name'], 'launch_id': job.get('launch_id'), 'slot_id': job.get('slot_id'),
            'job_ref': job.get('job_ref', {}), 'created': job.get('created'),
            'native_state': vm.get('tart', {}).get('State'),
            'native_delivered': props.get('native', {}).get('delivered'),
            'delivery': props.get('native', {}).get('delivery'),
            'native_exit': vm.get('files', {}).get('pulse.exit', {}).get('exit_code'),
            'guest_observation': (guest or {}).get('observation', 'not_probed'),
            'stage': stage, 'worker_checks': checks,
            'qualification': 'Observed markers/processes do not establish JobDB lease validity or job completion.'}


def now():
    return datetime.datetime.now(datetime.timezone.utc).isoformat()


def select(data, keys):
    """Explicit scalar allowlist: never recursively copy private properties."""
    if not isinstance(data, dict):
        return {}
    return {key: data[key] for key in keys if key in data
            and isinstance(data[key], (str, int, float, bool, type(None)))}


def evidence(text):
    """Export recognized signals, not arbitrary guest output, leases or errors."""
    out = {'signals': {s: text.count(s) for s in SIGNALS if s in text}}
    out['vz_error_codes'] = sorted(set(re.findall(r'VZErrorDomain\s+Code=(\d+)', text)))
    out['capacity_checks'] = sorted(set(re.findall(
        r'(?:physical macOS ceiling|configured (?:macOS|Linux|total)) count=\d+ limit=\d+', text)))
    out['slot_observations'] = sorted(set(re.findall(
        r'slot [A-Za-z0-9._-]+ (?:recorded readiness phase=[a-z-]+|readiness unknown: no prepared record)', text)))
    out['log_mentions'] = {label: len(re.findall(pattern, text, re.IGNORECASE)) for label, pattern in (
        ('lease_loss_or_expiry', r'lease[^\n]{0,60}(?:expired|lost|invalid|rejected)|execution_lease_lost|last_known_lease_expired'),
        ('renewal_error', r'renew[^\n]{0,60}(?:fail|error)|(?:fail|error)[^\n]{0,60}renew'),
        ('network_error', r'connection refused|no route to host|i/o timeout|no such host|network is unreachable'),
        ('tls_error', r'x509:|certificate verify failed|tls handshake'),
    ) if re.search(pattern, text, re.IGNORECASE)}
    identities, invalid = [], 0
    for line in text.splitlines():
        if not line.startswith('PULSE_TEMPLATE_READY '):
            continue
        try:
            identity = json.loads(line[len('PULSE_TEMPLATE_READY '):])
            mac, boot = identity.get('mac', ''), identity.get('boot_id', '')
            if not MAC.fullmatch(mac) or int(mac[:2], 16) & 1 or not UUID.fullmatch(boot):
                raise ValueError()
            identities.append({'mac': mac.lower(), 'boot_id': boot})
        except (ValueError, TypeError, AttributeError):
            invalid += 1
    out.update(template_identities=identities, invalid_template_markers=invalid)
    return out


def read_file(path, tail=False):
    # No FIFOs, devices or symlinks: collection must remain bounded/read-only.
    info = path.lstat()
    if not stat.S_ISREG(info.st_mode):
        raise ValueError('not a regular file')
    with path.open('rb') as stream:
        if tail and info.st_size > LIMIT:
            stream.seek(-LIMIT, os.SEEK_END)
        data = stream.read(LIMIT + 1)
    if not tail and len(data) > LIMIT:
        raise ValueError('file exceeds limit')
    return data[:LIMIT].decode('utf-8', errors='replace'), {
        'bytes': info.st_size, 'mtime_ns': info.st_mtime_ns,
        'tail_truncated': info.st_size > LIMIT,
    }


def properties(data):
    native = data.get('native') or {}
    props = data.get('properties') or {}
    out = {'version': data.get('version'),
           'native': select(native, ('uid', 'phase', 'pid', 'mac', 'created', 'delivered',
                                     'reusable', 'suspend_requested')),
           'spec': select(native.get('spec'), ('name', 'os', 'arch', 'image', 'cpu', 'memory'))}
    out['native']['delivery'] = select(native.get('delivery'), ('started_at', 'last_attempt_at', 'last_success_at',
            'attempts', 'stage', 'cause', 'execution_possible', 'expired'))
    script = (native.get('spec') or {}).get('startup_script') or {}
    expected = re.search(r'prepare-template\.sh\s+\d+\s+(\d+\.\d+(?:\.\d+)?)\b', script.get('script_content', ''))
    if expected:
        out['expected_template_os_version'] = expected[1]
    for kind in ('slot', 'image', 'job'):
        if props.get(kind):
            out[kind] = select(props[kind], ('id', 'name', 'uid', 'phase', 'worker', 'local_name',
                                             'mac', 'boot_id', 'created', 'probe', 'kind', 'os',
                                             'slot_id', 'pair_id', 'launch_id', 'outcome'))
    if props.get('pair'):
        pair = props['pair']
        out['pair'] = select(pair, ('ID', 'SlotID', 'Base', 'Profile', 'Phase', 'LaunchID',
                                    'Created', 'PrimaryDispatched', 'EngineDispatched'))
        for member in ('Primary', 'Engine'):
            out['pair'][member] = select(pair.get(member), ('name', 'os', 'image', 'cpu', 'memory'))
    if props.get('job'):
        out['job']['job_ref'] = select(props['job'].get('job'), ('instance_id', 'tenant_id', 'job_id'))
    # Scope groups reservations from separate providers; it is not a credential.
    out['scope'] = props.get('scope', data.get('scope'))
    return out


def capacity(vms):
    """Explain candidates; do not pretend an offline snapshot is admission."""
    observed = {v['name']: v for v in vms}
    used = {}

    def add(name, os_name, reason):
        if not name:
            return
        item = used.setdefault(name, {'name': name, 'os': os_name or 'unknown', 'reasons': []})
        if reason not in item['reasons']:
            item['reasons'].append(reason)

    for vm in vms:
        info, props = vm.get('tart', {}), vm.get('properties', {})
        os_name = info.get('OS') or props.get('spec', {}).get('os')
        if info.get('State') not in ('stopped', 'suspended'):
            add(vm['name'], os_name, 'native running or state unknown')
        native = props.get('native', {})
        pending = (info.get('State') == 'stopped' and native.get('phase') in ('starting', 'started')
                   and not native.get('reusable') and 'pulse.exit' not in vm.get('files', {}))
        if pending:
            add(vm['name'], os_name, 'possible pending native run; no exit receipt collected')
        job = props.get('job', {})
        if job and not job.get('pair_id'):
            add(job.get('name'), 'darwin', 'attached job reservation; reconciliation must observe completion')
        pair = props.get('pair', {})
        if pair and pair.get('Phase') != 'gone':
            for member in ('Primary', 'Engine'):
                spec = pair.get(member, {})
                add(spec.get('name'), spec.get('os'), 'pair reservation: ' + str(pair.get('Phase')))
        for kind in ('slot', 'image'):
            record = props.get(kind, {})
            if not record or record.get('phase') in ('ready', 'fallback'):
                continue
            name = record.get('name')
            current = observed.get(name, {}).get('tart', {})
            native = observed.get(name, {}).get('properties', {}).get('native', {})
            if current.get('State') in ('stopped', 'suspended') and native.get('phase') != 'starting':
                continue
            add(name, 'darwin' if kind == 'slot' else record.get('os') or os_name,
                kind + ' preparation reservation: ' + str(record.get('phase')))
    candidates = [used[k] for k in sorted(used)]
    mac = sum(v['os'] != 'linux' for v in candidates)
    return {'reservation_candidates': candidates, 'macos_including_unknown': mac,
            'with_one_more_macos': mac + 1, 'physical_limit': 2,
            'qualification': 'Best-effort snapshot across all owners; not the live provider admission decision.'}


class Collector:
    def __init__(self, args, output):
        self.args, self.output = args, output
        self.home = args.tart_home.expanduser().resolve()
        self.env = dict(os.environ, TART_HOME=str(self.home), TART_NO_AUTO_PRUNE='true')
        self.commands, self.errors = [], []
        self.logs = set(args.pulse_log)
        self.pulse_endpoints = set()
        self.probe_directory = None
        self.probe_binary = None

    def write(self, name, data):
        (self.output / name).write_text(json.dumps(data, indent=2, sort_keys=True) + '\n')

    def command(self, args, env=None, input_text=None, timeout=None):
        record = {'argv': args, 'time': now()}
        try:
            with subprocess.Popen(args, env=env or self.env, stdin=subprocess.PIPE,
                                  stdout=subprocess.PIPE, stderr=subprocess.PIPE,
                                  text=True, errors='replace', start_new_session=True) as process:
                try:
                    stdout, stderr = process.communicate(input_text, timeout=timeout or self.args.command_timeout)
                except (subprocess.TimeoutExpired, KeyboardInterrupt) as error:
                    try:
                        os.killpg(process.pid, signal.SIGKILL)
                    except ProcessLookupError:
                        pass
                    process.communicate()
                    record['error_type'] = type(error).__name__
                    if isinstance(error, KeyboardInterrupt):
                        raise
                    return None
                record.update(exit_code=process.returncode, stderr=evidence(stderr), stdout=evidence(stdout))
                return stdout if process.returncode == 0 else None
        except OSError as error:
            record['error_type'] = type(error).__name__
            return None
        finally:
            self.commands.append(record)

    def json_command(self, args):
        text = self.command(args)
        if text is not None:
            try:
                return json.loads(text)
            except ValueError:
                self.commands[-1]['decode_error'] = True
        return None

    def build_delivery_probe(self):
        self.probe_directory = tempfile.TemporaryDirectory(prefix='pulse-diagnosis-go-ssh-')
        binary = str(Path(self.probe_directory.name) / 'ssh-probe')
        self.command(['go', '-C', str(ROOT), 'build', '-o', binary, './scripts/diagnose-tart-ssh'], timeout=120)
        if self.commands[-1].get('exit_code') == 0 and Path(binary).is_file():
            self.probe_binary = binary
        else:
            self.errors.append('Go SSH probe build unavailable; see commands.json')

    def delivery(self, vm):
        props = vm.get('properties', {})
        if not props.get('job') or vm.get('tart', {}).get('State') != 'running':
            return None
        result = {'name': vm['name'], 'observation': 'unavailable',
                  'qualification': 'Separate process using this checkout; success does not prove the running Pulse process can deliver.'}
        try:
            directory = self.home / 'vms' / vm['name']
            native = json.loads(read_file(directory / 'pulse.json')[0])['native']
            spec = native['spec']
            if (spec['name'] != vm['name'] or native.get('uid') != props.get('native', {}).get('uid')
                    or native.get('reusable') or props.get('slot') or props.get('image')):
                raise ValueError()
            info = directory.lstat()
            checks = {
                'directory_identity_matches': stat.S_ISDIR(info.st_mode) and info.st_dev == native.get('device') and info.st_ino == native.get('inode'),
                'owner_marker_matches': read_file(directory / '.pulse-owner')[0] == native['uid'],
                'hardware_matches': all(vm['tart'].get(a) == b for a, b in (
                    ('CPU', spec.get('cpu')), ('Memory', spec.get('memory')), ('MACAddress', native.get('mac')))),
                'startup_script_present': bool((spec.get('startup_script') or {}).get('script_content')),
            }
            result['local_checks'] = checks
            if not all(checks.values()):
                result['observation'] = 'local_precondition_failed'
                return result
            if not self.probe_binary:
                result['observation'] = 'probe_build_unavailable'
                return result
            address = self.command(['tart', 'ip', vm['name'], '--wait', '5'])
            address = ipaddress.ip_address((address or '').strip())
            endpoint = f'[{address}]:22' if address.version == 6 else f'{address}:22'
            # Credentials are stdin only, never argv, files, or command records.
            request = {'address': endpoint, 'username': spec.get('username', ''),
                       'password': spec.get('password', ''), 'host_key': native.get('host_key')}
            raw = self.command([self.probe_binary], input_text=json.dumps(request), timeout=35)
            if raw is not None:
                result['go_ssh'] = select(json.loads(raw), ('stage', 'outcome', 'cause', 'host_key_pinned'))
                result['observation'] = 'collected'
        except (OSError, ValueError, KeyError, TypeError) as error:
            result['error_type'] = type(error).__name__
        return result

    def host(self):
        out = {'time': now(), 'platform': platform.system(), 'architecture': platform.machine(),
               'tart_home': str(self.home), 'uid': os.getuid(), 'python': platform.python_version()}
        for key, command in (
            ('macos', ['sw_vers', '-productVersion']), ('tart_version', ['tart', '--version']),
            ('source_revision', ['git', '-C', str(ROOT), 'rev-parse', 'HEAD']),
            ('resources', ['sysctl', 'hw.model', 'hw.ncpu', 'hw.memsize']),
        ):
            out[key] = self.command(command)
        try:
            disk = shutil.disk_usage(self.home)
            out['disk'] = dict(total=disk.total, used=disk.used, free=disk.free)
        except OSError as error:
            out['disk_error'] = type(error).__name__
        out['collector_sha256'] = hashlib.sha256(Path(__file__).read_bytes()).hexdigest()
        self.write('environment.json', out)

    def processes(self):
        # comm contains the executable, never argv or environment variables.
        text = self.command(['ps', '-axo', 'pid=,ppid=,etime=,stat=,comm=']) or ''
        processes = []
        for line in text.splitlines():
            parts = line.split(None, 4)
            if len(parts) != 5 or not any(s in Path(parts[4]).name.lower() for s in ('pulse', 'tart', 'orchard')):
                continue
            processes.append(dict(zip(('pid', 'ppid', 'elapsed', 'state', 'executable'), parts)))
        return processes

    def pulse(self, processes):
        builds = []
        for process in processes:
            executable = process['executable']
            if 'pulse' not in Path(executable).name.lower() or 'tart' in Path(executable).name.lower():
                continue
            # macOS ps may return argv[0] rather than an absolute executable.
            # Resolve the mapped executable from this process, not our PATH.
            mapped = self.command(['lsof', '-a', '-p', process['pid'], '-d', 'txt', '-Fn']) or ''
            candidates = [line[1:] for line in mapped.splitlines() if line.startswith('n/')
                          and 'pulse' in Path(line[1:]).name.lower()]
            resolved = next((path for path in candidates if Path(path).name == Path(executable).name), None)
            if not resolved and Path(executable).is_absolute():
                resolved = executable
            build = self.command(['go', 'version', '-m', resolved]) if resolved else None
            builds.append({'pid': process['pid'], 'executable': executable, 'build': [
                line.strip() for line in (build or '').splitlines()
                if re.match(r'\s*(?:path\s|mod\s|dep\s+github.com/colony-2/pulse-tart\s|build\s+vcs[.=])', line)],
                'resolved_executable': resolved})
            listeners = self.command(['lsof', '-nP', '-a', '-p', process['pid'], '-iTCP', '-sTCP:LISTEN', '-Fn']) or ''
            for line in listeners.splitlines():
                # Only query local wildcard/loopback listeners of this process.
                match = re.fullmatch(r'n(\*|127\.0\.0\.1|\[::1\]|\[::\]):(\d+)', line)
                if match and 0 < int(match[2]) < 65536:
                    host = '[::1]' if match[1].startswith('[') else '127.0.0.1'
                    self.pulse_endpoints.add('http://' + host + ':' + match[2])
            # Automatically find redirected stdout/stderr, but never copy raw logs.
            files = self.command(['lsof', '-a', '-p', process['pid'], '-d', '1,2', '-Fn']) or ''
            for line in files.splitlines():
                if line.startswith('n/'):
                    path = Path(line[1:])
                    try:
                        if stat.S_ISREG(path.lstat().st_mode):
                            self.logs.add(path)
                    except OSError:
                        pass
        return builds

    def pulse_status(self):
        statuses = []
        for endpoint in sorted(self.pulse_endpoints):
            data = self.json_command(['curl', '--noproxy', '*', '--silent', '--show-error', '--fail',
                                      '--max-time', str(self.args.command_timeout), endpoint + '/status'])
            item = {'endpoint': endpoint, 'available': isinstance(data, dict)}
            if isinstance(data, dict):
                item['status'] = select(data, ('status', 'version', 'started_at', 'uptime_seconds', 'providers'))
                item['poll'] = select(data.get('poll'), ('running', 'last_started', 'last_finished',
                                                       'last_succeeded', 'passes', 'failed_passes'))
            statuses.append(item)
        return statuses

    def log(self, path, pulse=False):
        try:
            text, meta = read_file(path, tail=True)
            out = dict(meta, **evidence(text))
            if pulse:
                records = []
                for line in text.splitlines():
                    try:
                        data = json.loads(line)
                    except ValueError:
                        continue
                    if not isinstance(data, dict) or data.get('msg') not in ('launch result', 'provider maintenance deferred'):
                        continue
                    record = select(data, ('time', 'level', 'msg', 'service', 'status', 'launch_id', 'execution_admission'))
                    record['diagnostics'] = evidence(line)
                    records.append(record)
                out['pulse_records'] = records[-200:]
            return out
        except (OSError, ValueError) as error:
            return {'error_type': type(error).__name__}

    def vm(self, name, inventory):
        out = {'name': name, 'files': {}}
        if not NAME.fullmatch(name):
            return dict(out, error='unsupported local VM name')
        info = self.json_command(['tart', 'get', name, '--format', 'json'])
        if not isinstance(info, dict):
            self.errors.append('Native inspection unavailable: ' + name)
        out['tart'] = select(info or inventory, ('Name', 'State', 'Running', 'OS', 'CPU', 'Memory', 'MACAddress', 'Disk', 'Size'))
        directory = self.home / 'vms' / name
        if directory.is_symlink():
            return dict(out, error='VM directory is a symlink; files skipped')
        path = directory / 'pulse.json'
        if path.exists():
            try:
                data, meta = read_file(path)
                out['properties'] = properties(json.loads(data))
                out['properties_file'] = meta
            except (OSError, ValueError, TypeError, AttributeError) as error:
                out['properties_error'] = type(error).__name__
        out['files'] = {}
        for filename in ('pulse.events', 'pulse.run.log', 'pulse.exit', 'state.vzvmsave', 'config.json'):
            path = directory / filename
            try:
                info = path.lstat()
                if not stat.S_ISREG(info.st_mode):
                    continue
                entry = {'bytes': info.st_size, 'mtime_ns': info.st_mtime_ns}
                if filename in ('pulse.events', 'pulse.run.log'):
                    entry.update(self.log(path))
                elif filename == 'pulse.exit':
                    value, _ = read_file(path)
                    entry['exit_code'] = int(value.strip()) if re.fullmatch(r'\d+\s*', value) else None
                elif filename == 'config.json':
                    config = json.loads(read_file(path)[0])
                    for key in ('ecid', 'hardwareModel'):
                        if key in config:
                            entry[key + '_sha256'] = hashlib.sha256(json.dumps(config[key], sort_keys=True).encode()).hexdigest()
                out['files'][filename] = entry
            except FileNotFoundError:
                pass
            except (OSError, ValueError, TypeError) as error:
                out['files'][filename] = {'error_type': type(error).__name__}
        return out

    def guest(self, vm, job=False):
        props = vm.get('properties', {})
        source = props.get('slot') or props.get('image')
        if (vm.get('tart', {}).get('State') != 'running'
                or props.get('spec', {}).get('os') != 'darwin'):
            return None
        if job:
            if not props.get('job') or source or props.get('native', {}).get('reusable'):
                return None
        elif not source or source.get('phase') == 'ready' or props.get('job') or props.get('pair'):
            return None
        result = {'name': vm['name'], 'observation': 'unavailable', 'role': 'job' if job else 'builder'}
        try:
            native = json.loads(read_file(self.home / 'vms' / vm['name'] / 'pulse.json')[0])['native']
            spec = native['spec']
            if spec['name'] != vm['name'] or native.get('uid') != props.get('native', {}).get('uid'):
                raise ValueError()
            address = self.command(['tart', 'ip', vm['name'], '--wait', '1'])
            address = str(ipaddress.ip_address((address or '').strip()))
            username = spec.get('username', '')
            if not re.fullmatch(r'[A-Za-z0-9][A-Za-z0-9_-]*', username):
                raise ValueError()
            # Credentials and host keys never enter argv, commands.json or the archive.
            with tempfile.TemporaryDirectory(prefix='pulse-diagnosis-ssh-') as directory:
                work = Path(directory)
                askpass = work / 'askpass'
                askpass.write_text('#!/bin/sh\nprintf "%s\\n" "$PULSE_DIAG_SSH_PASSWORD"\n')
                askpass.chmod(0o700)
                known_hosts = work / 'known_hosts'
                strict = 'accept-new'
                if native.get('host_key'):
                    key = base64.b64decode(native['host_key'], validate=True).decode().strip()
                    known_hosts.write_text(address + ' ' + key + '\n')
                    strict = 'yes'
                env = dict(self.env, PULSE_DIAG_SSH_PASSWORD=spec.get('password', ''),
                           SSH_ASKPASS=str(askpass), SSH_ASKPASS_REQUIRE='force', DISPLAY='pulse-diagnosis')
                text = self.command(['ssh', '-T', '-o', 'ConnectTimeout=5', '-o', 'ConnectionAttempts=1',
                                     '-o', 'NumberOfPasswordPrompts=1', '-o', 'PreferredAuthentications=password',
                                     '-o', 'StrictHostKeyChecking=' + strict, '-o', 'UserKnownHostsFile=' + str(known_hosts),
                                     '-o', 'GlobalKnownHostsFile=/dev/null', '-o', 'LogLevel=ERROR',
                                     username + '@' + address, 'sudo -n /bin/bash -s'], env=env,
                                    input_text=JOB_PROBE if job else GUEST_PROBE)
                result['exit_code'] = self.commands[-1].get('exit_code')
                if text is not None:
                    result.update(observation='collected', diagnostics=evidence(text))
                    result['checks'] = re.findall(
                        r'^PULSE_DIAG ((?:os_version=[0-9.]+|boot_id=[0-9A-Fa-f-]+|'
                        r'(?:base|template)_(?:ready|failed)=(?:present|absent)|'
                        r'(?:c2j|pulse-tart-guest)=(?:executable|missing)|'
                        r'(?:base|template)_service=(?:loaded|not_loaded)|'
                        r'(?:base|template)_service_state=[a-z]+|'
                        r'(?:base|template)_service_exit=-?\d+))$', text, re.MULTILINE)
                    if job:
                        result['checks'] = job_checks(text)
                        identities = result['diagnostics']['template_identities']
                        if len(identities) == 1 and 'boot_id' in result['checks'] and 'mac' in result['checks']:
                            result['snapshot_identity_matches'] = (
                                identities[0]['boot_id'] == result['checks']['boot_id']
                                and identities[0]['mac'] == result['checks']['mac'].lower())
        except (OSError, ValueError, KeyError, TypeError) as error:
            result['error_type'] = type(error).__name__
        return result

    def snapshot(self, index):
        inventory = self.json_command(['tart', 'list', '--source', 'local', '--format', 'json'])
        valid = isinstance(inventory, list) and all(isinstance(v, dict) and isinstance(v.get('Name'), str) for v in inventory)
        if not valid:
            self.errors.append('Native inventory unavailable in sample ' + str(index))
            inventory = []
        entries = {v['Name']: v for v in inventory}
        # Include directories Tart omitted, to reveal partial creations.
        try:
            for path in (self.home / 'vms').iterdir():
                if path.is_dir() and (path / 'pulse.json').exists():
                    entries.setdefault(path.name, {})
        except OSError:
            pass
        vms = [self.vm(name, entries[name]) for name in sorted(entries)]
        processes = self.processes()
        out = {'time': now(), 'inventory_available': valid, 'vms': vms,
               'capacity': capacity(vms), 'processes': processes}
        if index == 1:
            self.write('pulse-builds.json', self.pulse(processes))
        out['pulse_status'] = self.pulse_status()
        out['pulse_logs'] = {str(path): self.log(path, pulse=True) for path in sorted(self.logs)}
        self.write(f'snapshot-{index:02d}.json', out)
        if getattr(self.args, 'jobs', False):
            observations, guests = [], []
            for vm in vms:
                guest = self.guest(vm, job=True) if not self.args.skip_guest else None
                if guest is not None:
                    guests.append(guest)
                item = job_observation(vm, guest)
                if item:
                    observations.append(item)
            out['guest_jobs'], out['job_observations'] = guests, observations
            if getattr(self.args, 'delivery', False) and not self.args.skip_guest:
                out['delivery_probes'] = [result for vm in vms if (result := self.delivery(vm)) is not None]
            self.write(f'snapshot-{index:02d}.json', out)
        return out

    def discovery(self):
        text = self.command(['bash', str(ROOT / 'scripts/update-macos-images.sh'), '--list'])
        result = {'exit_code': self.commands[-1].get('exit_code'),
                  'signals': self.commands[-1].get('stderr'), 'targets': []}
        if text:
            result['targets'] = [match[1] for line in text.splitlines()
                                 if (match := re.fullmatch(r'\s+((?:orchard-)?pulse-[A-Za-z0-9._-]+)(?: \((?:missing; will create|unfinished builder; offline repair)\))?', line))]
        self.write('updater-discovery.json', result)

    def legacy(self):
        for index, directory in enumerate(self.args.state_dir):
            result = {'path': str(directory), 'records': []}
            try:
                state = json.loads(read_file(directory / 'state.json')[0])
                for bucket, kind in (('slots', 'slot'), ('images', 'image'), ('records', 'job'), ('pairs', 'pair')):
                    for record in (state.get(bucket) or {}).values():
                        result['records'].append(properties({'properties': {kind: record}}))
                result['events'] = {path.name: self.log(path) for path in sorted((directory / 'native').glob('*.events'))}
                result['runs'] = {path.name: self.log(path) for path in sorted((directory / 'native').glob('*.run.log'))}
            except (OSError, ValueError, TypeError, AttributeError) as error:
                result['error_type'] = type(error).__name__
            self.write(f'legacy-{index + 1:02d}.json', result)


def summarize(snapshots, errors):
    out = {'collection': 'partial' if errors else 'complete', 'errors': errors,
           'health': 'not determined by offline collection', 'sample_count': len(snapshots),
           'maintenance': 'Process presence and unchanged snapshots do not prove loop liveness or failure.',
           'findings': []}
    if not snapshots:
        return out
    last = snapshots[-1]
    out['capacity'] = last['capacity']
    for vm in last['vms']:
        props = vm.get('properties', {})
        for kind in ('slot', 'image'):
            record = props.get(kind, {})
            if record and record.get('phase') != 'ready':
                out['findings'].append({'name': vm['name'], 'kind': kind, 'phase': record.get('phase')})
        if props.get('slot') and not vm['files'].get('pulse.events', {}).get('template_identities'):
            out['findings'].append({'name': vm['name'], 'observation': 'No valid template marker in collected host events; adopted ready sources may have none.'})
    before = {v['name']: v for v in snapshots[0]['vms']}
    after = {v['name']: v for v in last['vms']}
    out['changed_vms'] = [name for name in sorted(before.keys() | after.keys()) if before.get(name) != after.get(name)]
    out['pulse_logs_collected'] = list(last['pulse_logs'])
    out['job_observations'] = last.get('job_observations', [])
    out['delivery_probes'] = last.get('delivery_probes', [])
    out['observed_job_launches'] = sorted({job['launch_id'] for sample in snapshots
                                         for job in sample.get('job_observations', []) if job.get('launch_id')})
    return out


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--tart-home', type=Path, default=Path(os.environ.get('TART_HOME') or Path.home() / '.tart'))
    parser.add_argument('--samples', type=int, default=3)
    parser.add_argument('--interval', type=float, default=30)
    parser.add_argument('--command-timeout', type=float, default=15)
    parser.add_argument('--pulse-log', type=Path, action='append', default=[], help='Pulse JSON log; repeatable, extracted fields only')
    parser.add_argument('--state-dir', type=Path, action='append', default=[], help='Optional legacy StateDir; repeatable')
    parser.add_argument('--skip-guest', action='store_true', help='Skip all guest SSH checks, including --jobs probes')
    parser.add_argument('--jobs', action='store_true', help='Also inspect running macOS job clones read-only during each snapshot')
    parser.add_argument('--delivery', action='store_true', help='Include --jobs and a read-only Go SSH transport probe (requires Go)')
    parser.add_argument('--output-root', type=Path, default=ROOT / 'test-results')
    args = parser.parse_args()
    if args.delivery:
        args.jobs = True
    if not 1 <= args.samples <= 30 or not 0 <= args.interval <= 300 or not 1 <= args.command_timeout <= 120:
        parser.error('samples: 1..30; interval: 0..300 seconds; command-timeout: 1..120 seconds')
    os.umask(0o077)
    stamp = datetime.datetime.now(datetime.timezone.utc).strftime('%Y%m%dT%H%M%SZ')
    output = args.output_root.expanduser().resolve() / f'{stamp}-tart-diagnosis-{os.getpid()}'
    output.mkdir(parents=True, exist_ok=False)
    collector = Collector(args, output)
    snapshots, status = [], 0
    print(f'Results: {output}', flush=True)
    try:
        collector.host()
        if platform.system() != 'Darwin' or platform.machine() != 'arm64':
            raise RuntimeError('Run on the Apple Silicon Tart worker as the Pulse service user.')
        if args.delivery and not args.skip_guest:
            print('Building read-only Go SSH delivery probe.', flush=True)
            collector.build_delivery_probe()
        for index in range(1, args.samples + 1):
            print(f'Collecting snapshot {index}/{args.samples}', flush=True)
            snapshots.append(collector.snapshot(index))
            if index != args.samples:
                print(f'Waiting {args.interval:g}s for changes; leave Pulse running.', flush=True)
                time.sleep(args.interval)
        collector.discovery()
        collector.legacy()
        if not args.skip_guest:
            print('Checking running unfinished macOS builders over SSH.', flush=True)
            collector.write('guest-builders.json', [result for vm in snapshots[-1]['vms']
                                                   if (result := collector.guest(vm)) is not None])
        if collector.errors:
            status = 1
    except KeyboardInterrupt:
        collector.errors.append('Collection interrupted')
        status = 130
    except Exception as error:
        # Exception messages can contain private JSON; retain type only.
        collector.errors.append(type(error).__name__)
        if isinstance(error, RuntimeError):
            print('Run on the Apple Silicon Tart worker as the Pulse service user.', flush=True)
        status = 1
    finally:
        if collector.probe_directory:
            collector.probe_directory.cleanup()
        summary = summarize(snapshots, collector.errors)
        summary['exit_code'] = status
        collector.write('summary.json', summary)
        collector.write('commands.json', collector.commands)
        (output / 'README.txt').write_text(
            'Read summary.json, then snapshot-*.json, updater-discovery.json and commands.json.\n'
            'This collector does not determine overall health or run reconciliation.\n'
            'Raw VM properties, startup scripts, credentials, leases, process argv and raw logs are not archived.\n'
            'Logs contain recognized diagnostic signals only; unknown error text is omitted.\n'
            'No Pulse log found? Repeat with --pulse-log /path/to/pulse.jsonl.\n'
            'Legacy state is collected only with --state-dir /path/to/StateDir.\n')
        archive = Path(str(output) + '.tar.gz')
        with tarfile.open(archive, 'w:gz') as bundle:
            bundle.add(output, arcname=output.name)
        print(f'Collection: {summary["collection"]}', flush=True)
        print(f'Send: {archive}', flush=True)
    return status


if __name__ == '__main__':
    raise SystemExit(main())
