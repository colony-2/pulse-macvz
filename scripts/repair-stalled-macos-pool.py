#!/usr/bin/env python3
"""Finish the two diagnosed builders, then prepare the two missing standard slots."""
import argparse
import base64
import datetime
import fcntl
import importlib.util
import ipaddress
import json
import os
from pathlib import Path
import platform
import re
import shlex
import stat
import subprocess
import tarfile
import tempfile
import time

ROOT = Path(__file__).resolve().parent.parent
module = importlib.util.spec_from_file_location('diagnosis', ROOT / 'scripts/diagnose-tart.py')
diagnosis = importlib.util.module_from_spec(module)
module.loader.exec_module(diagnosis)
OWNER_TOKEN = 'id-565a157910ca985c18d3'
SCOPE = '5e160743ef9ee1a307068d89d00629d212a1d4fb57006b02c3fe6a8a34d6caa0'
LEGACY_ID = 'pulse-tart-e3ae0782ba0071285ea94'
POOLS = (
    ('macos-26-arm64', 'standard', '26.6.2', '261e357a0760d8576109dec5498334e4'),
    ('macos-15-arm64', 'mac-macos-15-arm64', '15.7.7', 'c7870b9e952c050f45992f9bf114583b'),
)


def names(pool):
    base, group, _, _ = pool
    prefix = f'pulse-{OWNER_TOKEN}-{base}'
    return prefix + '-base', [f'{prefix}-{group}-4cpu-8192m-slot-{i:04d}' for i in (1, 2)]


class RepairError(Exception):
    pass


def require(condition, message):
    if not condition:
        raise RepairError(message)


class Repair(diagnosis.Collector):
    def __init__(self, args, output):
        args.pulse_log = []
        args.command_timeout = args.timeout + 60
        super().__init__(args, output)
        self.completed = []

    def run(self, command, **kwargs):
        text = self.command(command, **kwargs)
        require(text is not None, f'{command[0]} {command[1]} failed; see commands.json')
        return text

    def inspect(self, name):
        return json.loads(self.run(['tart', 'get', name, '--format', 'json']))

    def metadata(self, name):
        directory = self.home / 'vms' / name
        require(not directory.is_symlink(), 'Refusing a symlink VM directory: ' + name)
        return json.loads(diagnosis.read_file(directory / 'pulse.json')[0])

    def hardware(self, name, states):
        vm = self.inspect(name)
        require(vm.get('OS') == 'darwin' and vm.get('CPU') == 4 and vm.get('Memory') == 8192
                and vm.get('State') in states, 'Unexpected hardware/state: ' + name)
        return vm

    def preflight(self):
        inventory = json.loads(self.run(['tart', 'list', '--source', 'local', '--format', 'json']))
        present = {v['Name'] for v in inventory}
        # Validate all objects before the first guest mutation. No jobs are touched.
        for directory in (self.home / 'vms').iterdir():
            if not (directory / 'pulse.json').exists():
                continue
            data = self.metadata(directory.name)
            props = data.get('properties') or {}
            if data.get('scope') == SCOPE:
                require(not props.get('job') and not props.get('pair'),
                        'Pool has job/pair reservations; let them drain before repair: ' + directory.name)
        plan = []
        for pool in POOLS:
            base, slots = names(pool)
            require(not (self.home / ('.' + base + '.update-lock')).exists(),
                    'An updater lock already exists for this pool: ' + base)
            self.hardware(base, ('stopped',))
            data = self.metadata(base)
            require(data.get('scope') == SCOPE and data['native'].get('reusable')
                    and (data.get('properties') or {}).get('image', {}).get('phase') == 'ready',
                    'Prepared base identity/readiness changed: ' + base)
            vm = self.hardware(slots[0], ('running', 'suspended'))
            data = self.metadata(slots[0])
            native, props = data['native'], data.get('properties') or {}
            slot = props.get('slot') or {}
            require(data.get('scope') == SCOPE and native.get('uid') == pool[3]
                    and native.get('spec', {}).get('name') == slots[0]
                    and native['spec'].get('image') == base
                    and slot.get('name') == slots[0] and slot.get('uid') == pool[3]
                    and slot.get('phase') in ('preparing', 'suspending', 'ready')
                    and not props.get('job') and not props.get('pair'),
                    'Builder no longer matches the diagnosed unused slot: ' + slots[0])
            require(vm.get('MACAddress') == native.get('mac'), 'Builder MAC changed: ' + slots[0])
            directory = self.home / 'vms' / slots[0]
            owner = diagnosis.read_file(directory / '.pulse-owner')[0]
            st = directory.stat()
            require(owner == native['uid'] and st.st_ino == native.get('inode')
                    and st.st_dev == native.get('device'), 'Builder directory ownership changed: ' + slots[0])
            if slots[1] in present:
                self.hardware(slots[1], ('suspended',))
                path = self.home / 'vms' / slots[1] / 'pulse.json'
                if path.exists():
                    other = self.metadata(slots[1])
                    require(other.get('scope') == SCOPE and other['native'].get('reusable')
                            and not (other.get('properties') or {}).get('job')
                            and not (other.get('properties') or {}).get('pair'),
                            'Second slot exists but is not a reusable source: ' + slots[1])
            else:
                require(not (self.home / 'vms' / slots[1]).exists(), 'Unlisted second-slot directory exists')
            plan.append({'base': base, 'existing_builder': slots[0], 'builder_state': vm['State'],
                         'second_slot': slots[1], 'second_slot_exists': slots[1] in present})
        self.write('plan.json', plan)

    def ssh(self, name, script):
        native = self.metadata(name)['native']
        spec = native['spec']
        address = str(ipaddress.ip_address(self.run(['tart', 'ip', name, '--wait', '5']).strip()))
        user = spec['username']
        require(re.fullmatch(r'[A-Za-z0-9][A-Za-z0-9_-]*', user), 'Invalid builder SSH username')
        with tempfile.TemporaryDirectory(prefix='pulse-repair-ssh-') as temporary:
            work = Path(temporary)
            askpass = work / 'askpass'
            askpass.write_text('#!/bin/sh\nprintf "%s\\n" "$PULSE_IMAGE_SSH_PASSWORD"\n')
            askpass.chmod(0o700)
            known = work / 'known_hosts'
            strict = 'accept-new'
            if native.get('host_key'):
                known.write_text(address + ' ' + base64.b64decode(native['host_key'], validate=True).decode().strip() + '\n')
                strict = 'yes'
            env = dict(self.env, SSH_ASKPASS=str(askpass), SSH_ASKPASS_REQUIRE='force', DISPLAY='pulse-repair',
                       PULSE_IMAGE_SSH_PASSWORD=spec.get('password', ''))
            return self.run(['ssh', '-T', '-o', 'ConnectTimeout=5', '-o', 'NumberOfPasswordPrompts=1',
                             '-o', 'PreferredAuthentications=password', '-o', 'StrictHostKeyChecking=' + strict,
                             '-o', 'UserKnownHostsFile=' + str(known), '-o', 'GlobalKnownHostsFile=/dev/null',
                             user + '@' + address, 'sudo -n /bin/bash -s'], env=env, input_text=script)

    def wait_suspended(self, name):
        deadline = time.monotonic() + 120
        while time.monotonic() < deadline:
            vm = self.inspect(name)
            receipt = self.home / 'vms' / name / 'pulse.exit'
            if vm.get('State') == 'suspended' and receipt.exists():
                require(diagnosis.read_file(receipt)[0].strip() == '0', 'Tart suspend/run failed: ' + name)
                return
            time.sleep(1)
        raise RepairError('Suspension did not finish; leave this builder intact and send the report: ' + name)

    def finish_builder(self, pool):
        _, slots = names(pool)
        name = slots[0]
        vm = self.hardware(name, ('running', 'suspended'))
        path = self.home / 'vms' / name / 'pulse.events'
        old = diagnosis.read_file(path)[0] if path.exists() else ''
        old_identities = diagnosis.evidence(old)['template_identities']
        if vm['State'] == 'suspended':
            require(old_identities and old_identities[0]['mac'] == vm['MACAddress'].lower(),
                    'Suspended builder lacks a matching recorded readiness identity: ' + name)
            self.wait_suspended(name)
            print('Already suspended: ' + name, flush=True)
            return
        print('Finishing template preparation: ' + name, flush=True)
        script = ("set -euo pipefail\nset +x\n"
                  "test ! -e /var/db/pulse-tart/intent\n"
                  "test ! -e /Library/LaunchDaemons/dev.pulse-tart.guest.plist\n"
                  "test ! -e /var/db/pulse-tart-template/failed\n"
                  f"test \"$(/usr/bin/sw_vers -productVersion)\" = {shlex.quote(pool[2])}\n"
                  f"/bin/bash /opt/pulse-tart-image/prepare-template.sh {self.args.timeout} {pool[2]} 'nix git node pnpm uv python'\n"
                  "printf 'PULSE_REPAIR_BOOT '\n/usr/sbin/sysctl -n kern.bootsessionuuid\n")
        text = self.ssh(name, script)
        self.write(pool[0] + '-preparation.json', diagnosis.evidence(text))
        identities = diagnosis.evidence(text)['template_identities']
        boots = re.findall(r'^PULSE_REPAIR_BOOT ([0-9a-fA-F-]+)$', text, re.MULTILINE)
        require(len(identities) == 1 and len(boots) == 1, 'Guest did not return one valid readiness identity: ' + name)
        identity = identities[0]
        require(identity['boot_id'] == boots[0] and identity['mac'] == vm['MACAddress'].lower(),
                'Readiness identity does not match the running builder: ' + name)
        require(not old_identities or all(item == identity for item in old_identities),
                'Existing host events contain a different readiness identity: ' + name)
        # Record only the real guest marker; do not edit private metadata or mark a slot ready.
        marker = next(line for line in text.splitlines() if line.startswith('PULSE_TEMPLATE_READY '))
        fd = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_APPEND | os.O_NOFOLLOW, 0o600)
        with os.fdopen(fd, 'a') as stream:
            stream.write('\n' + marker + '\n')
            stream.flush()
            os.fsync(stream.fileno())
        self.run(['tart', 'suspend', name])
        self.wait_suspended(name)
        self.completed.append(name)
        print('Suspended with verified readiness: ' + name, flush=True)

    def fill_missing(self, pool):
        base, slots = names(pool)
        self.hardware(base, ('stopped',))
        self.hardware(slots[0], ('suspended',))
        inventory = json.loads(self.run(['tart', 'list', '--source', 'local', '--format', 'json']))
        count = 0
        for vm in inventory:
            if vm.get('State') not in ('stopped', 'suspended') and self.inspect(vm['Name']).get('OS') != 'linux':
                count += 1
        require(count < 2, 'No physical macOS position is free for the missing template')
        print('Ensuring both slots from prepared base: ' + pool[0], flush=True)
        native = self.metadata(slots[0])['native']['spec']
        env = dict(self.env, PULSE_IMAGE_SSH_PASSWORD=native.get('password', ''))
        raw_image = self.metadata(base)['native']['spec'].get('image', '')
        require(re.fullmatch(r'[A-Za-z0-9][A-Za-z0-9.:/_-]*@sha256:[a-f0-9]{64}', raw_image),
                'Prepared base no longer references its original catalog image: ' + base)
        command = ['bash', str(ROOT / 'scripts/update-macos-images.sh'), '--owner', OWNER_TOKEN,
                   '--base', pool[0], '--group', pool[1], '--slots', '2', '--cpu', '4', '--memory', '8192',
                   '--ensure', '--ssh-user', native['username'], '--timeout', str(self.args.timeout),
                   '--image', raw_image, '--os-version', pool[2]]
        # The updater reports progress live and owns cleanup of its staging VMs.
        # Keep raw preparation logs private in its temporary directory, outside the archive.
        with tempfile.TemporaryDirectory(prefix='pulse-repair-update-logs-') as temporary:
            env['PULSE_IMAGE_LOG_DIR'] = temporary
            result = subprocess.run(command, env=env)
            self.write(pool[0] + '-updater.json', {path.name: self.log(path) for path in Path(temporary).glob('*.log')})
        self.commands.append({'argv': command, 'exit_code': result.returncode, 'time': diagnosis.now()})
        require(result.returncode == 0, 'Missing-template preparation failed; see the updater log path above')
        for name in slots:
            self.hardware(name, ('suspended',))

    def repair(self):
        if getattr(self.args, 'retire_legacy_state', False):
            self.retire_legacy_state(required=True)
            return
        self.preflight()
        if self.args.check:
            return
        for pool in POOLS:
            self.finish_builder(pool)
        for pool in POOLS:
            self.fill_missing(pool)
        states = [self.hardware(name, ('suspended',)) for pool in POOLS for name in names(pool)[1]]
        macs = [vm.get('MACAddress') for vm in states]
        require(all(macs) and len(set(macs)) == 4, 'Final template MACs are not distinct')
        self.write('templates.json', [diagnosis.select(vm, ('State', 'OS', 'CPU', 'Memory', 'MACAddress')) for vm in states])
        self.retire_legacy_state()

    def retire_legacy_state(self, required=False):
        """Retire the obsolete journal and only proven stale source imports.

        No signing keys are changed or exported. Current per-VM properties remain
        authoritative; private backups live outside the shareable result directory.
        """
        legacy = getattr(self.args, 'state_dir', None)
        if legacy is None:
            legacy = Path.home() / 'Library/Application Support/pulse/providers' / LEGACY_ID
        legacy = legacy.expanduser().absolute()
        if not legacy.exists():
            require(not required, 'Legacy StateDir not found at ' + str(legacy) + '; supply --state-dir with the configured directory')
            self.write('legacy-retirement.json', {'action': 'not_present', 'path': str(legacy)})
            return
        require(not legacy.is_symlink(), 'Legacy StateDir must not be a symlink')
        require(stat.S_ISREG((legacy / 'lock').lstat().st_mode), 'Legacy lock is not a regular file')
        with (legacy / 'lock').open('r') as lock:
            try:
                fcntl.flock(lock.fileno(), fcntl.LOCK_EX | fcntl.LOCK_NB)
            except BlockingIOError:
                raise RepairError('Another process owns the legacy journal; stop it before repair') from None
            old = json.loads(diagnosis.read_file(legacy / 'state.json')[0])
            require(old.get('version') == 3 and old.get('scope') == SCOPE, 'Legacy journal is for a different pool or version')
            old_key = base64.b64decode(old.get('key', ''), validate=True)
            require(len(old_key) == 32, 'Legacy journal key is invalid')
            first = {names(pool)[1][0] for pool in POOLS}
            second = {names(pool)[1][1] for pool in POOLS}
            sources = first | second | {names(pool)[0] for pool in POOLS}
            current_keys, attached = set(), set()
            stale = []
            for directory in (self.home / 'vms').iterdir():
                if not (directory / 'pulse.json').exists():
                    continue
                data = self.metadata(directory.name)
                if data.get('scope') != SCOPE:
                    continue
                native, props = data.get('native') or {}, data.get('properties')
                require(native.get('spec', {}).get('name') == directory.name, 'Native VM name mismatch: ' + directory.name)
                if props:
                    require(props.get('version') == 1 and props.get('scope') == SCOPE,
                            'Invalid attached provider properties: ' + directory.name)
                    key = base64.b64decode(props.get('key', ''), validate=True)
                    require(len(key) == 32, 'Invalid attached provider key: ' + directory.name)
                    current_keys.add(key)
                    require(not props.get('job') and not props.get('pair'),
                            'Pool has job/pair reservations; refusing journal retirement: ' + directory.name)
                    require(props.get('slot') or props.get('image'), 'Unknown attached resource: ' + directory.name)
                    attached.add(directory.name)
                    continue
                # Startup can import an old reusable native record onto a newly
                # published second-slot directory before failing the key check.
                require(directory.name in second, 'Unmigrated non-source resource: ' + directory.name)
                historical = json.loads(diagnosis.read_file(legacy / 'native' / (directory.name + '.json'))[0])
                require(native.get('reusable') and historical.get('reusable')
                        and native.get('uid') == historical.get('uid')
                        and historical.get('spec', {}).get('name') == directory.name,
                        'Missing properties cannot be attributed to stale source migration: ' + directory.name)
                vm = self.hardware(directory.name, ('suspended',))
                require(native.get('mac') and native['mac'].lower() != vm.get('MACAddress', '').lower(),
                        'Source identity does not prove replacement: ' + directory.name)
                for filename in ('pulse.json', 'pulse.events', 'pulse.exit', 'pulse.run.log'):
                    path = directory / filename
                    if path.exists() or path.is_symlink():
                        require(stat.S_ISREG(path.lstat().st_mode), 'Refusing non-regular metadata: ' + str(path))
                        stale.append(path)
            require(first | {names(pool)[0] for pool in POOLS} <= attached,
                    'Expected repaired builders/bases lack current provider properties')
            require(len(current_keys) == 1, 'Current VM keys disagree; refusing journal retirement')
            if old_key in current_keys:
                require(not required, 'Legacy and current keys match; this is not the reported mismatch')
                self.write('legacy-retirement.json', {'action': 'keys_match', 'path': str(legacy)})
                return
            # Confirm the entire repaired pool still consists of sources, not
            # running work. No suspended source is resumed to perform this repair.
            for pool in POOLS:
                base, slots = names(pool)
                require(not (self.home / ('.' + base + '.update-lock')).exists(),
                        'An updater lock already exists for this pool: ' + base)
                self.hardware(base, ('stopped',))
                for name in slots:
                    self.hardware(name, ('suspended',))
            inventory = json.loads(self.run(['tart', 'list', '--source', 'local', '--format', 'json']))
            for item in inventory:
                name = item['Name']
                if name.startswith('pulse-' + OWNER_TOKEN + '-') and name not in sources:
                    # The retained Linux base is also valid, but must have current metadata.
                    require(name in attached, 'Unowned/unmigrated pool resource: ' + name)
            backup = legacy.with_name(legacy.name + '.retired-' + self.output.name)
            require(not backup.exists(), 'Retirement backup already exists')
            plan = {'action': 'planned' if self.args.check else 'retired', 'path': str(legacy),
                    'backup': str(backup), 'stale_metadata': [str(p) for p in stale],
                    'current_provider_metadata_preserved': sorted(attached)}
            if self.args.check:
                self.write('legacy-retirement.json', plan)
                return
            # Prepare the complete plan before moving any private file. Move the
            # journal last; interruption remains recoverable with the same script.
            self.write('legacy-retirement.json', dict(plan, action='in_progress'))
            backup.mkdir(mode=0o700)
            for path in stale:
                destination = backup / 'imported-vm-metadata' / path.parent.name
                destination.mkdir(parents=True, exist_ok=True, mode=0o700)
                path.rename(destination / path.name)
            legacy.rename(backup / 'journal')
            self.write('legacy-retirement.json', plan)
            print('Retired obsolete journal; private backup: ' + str(backup), flush=True)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--check', action='store_true', help='Validate this specific pool without modifying VMs')
    parser.add_argument('--retire-legacy-state', action='store_true', help='Fix the legacy-key startup failure after template repair; no guest operations')
    parser.add_argument('--state-dir', type=Path, help='Configured legacy StateDir; defaults to this pool under ~/Library/Application Support/pulse/providers')
    parser.add_argument('--tart-home', type=Path, default=Path(os.environ.get('TART_HOME') or Path.home() / '.tart'))
    parser.add_argument('--timeout', type=int, default=900, help='Guest preparation timeout in seconds (default 900)')
    args = parser.parse_args()
    if not 60 <= args.timeout <= 7200:
        parser.error('timeout must be 60..7200 seconds')
    os.umask(0o077)
    stamp = datetime.datetime.now(datetime.timezone.utc).strftime('%Y%m%dT%H%M%SZ')
    output = ROOT / 'test-results' / f'{stamp}-tart-repair-{os.getpid()}'
    output.mkdir(parents=True)
    repair = Repair(args, output)
    code, error = 0, None
    print('Results: ' + str(output), flush=True)
    try:
        require(platform.system() == 'Darwin' and platform.machine() == 'arm64', 'Run on the diagnosed Apple Silicon worker')
        lock = repair.home / '.pulse' / SCOPE / 'lock'
        require(lock.is_file() and not lock.is_symlink(), 'Expected pool lock not found; refusing another Tart home')
        with lock.open('r') as stream:
            try:
                fcntl.flock(stream.fileno(), fcntl.LOCK_EX | fcntl.LOCK_NB)
            except BlockingIOError:
                raise RepairError('Stop Pulse normally (Ctrl-C in its terminal), then rerun; Pulse still owns the pool lock') from None
            repair.repair()
        print('Checks passed.' if args.check else 'Four suspended templates are prepared. Restart Pulse normally to reconcile/adopt them.', flush=True)
    except (Exception, KeyboardInterrupt) as exc:
        code = 130 if isinstance(exc, KeyboardInterrupt) else 1
        error = str(exc) if isinstance(exc, RepairError) else type(exc).__name__
        print('Repair stopped: ' + error, flush=True)
        print('Leave completed templates intact; send the archive. The script never deletes existing VMs.', flush=True)
    finally:
        repair.write('summary.json', {'exit_code': code, 'error': error, 'check_only': args.check,
                                     'legacy_state_only': args.retire_legacy_state,
                                     'completed_builders': repair.completed,
                                     'next_step': 'Restart Pulse after successful repair, then run diagnose-tart.sh.'})
        repair.write('commands.json', repair.commands)
        archive = str(output) + '.tar.gz'
        with tarfile.open(archive, 'w:gz') as bundle:
            bundle.add(output, arcname=output.name)
        print('Send: ' + archive, flush=True)
    return code


if __name__ == '__main__':
    raise SystemExit(main())
