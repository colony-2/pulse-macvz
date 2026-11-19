#!/usr/bin/env python3
"""Repair Nix daemon access in owned Linux VMs without replaying any job."""
import argparse
import base64
from contextlib import ExitStack
import fcntl
import ipaddress
import json
import os
from pathlib import Path
import re
import shutil
import stat
import subprocess
import tempfile
import time


GUEST_REPAIR = r'''set -euo pipefail
set +x
cd /
trap 'sync' EXIT
test "$(uname -s)" = Linux
test "$(id -u pulseworker)" = 55050
test -x /usr/bin/nix
if getent group nix-users >/dev/null; then usermod -aG nix-users pulseworker; fi
sudo -n -H -u pulseworker /usr/bin/nix --extra-experimental-features nix-command store ping --store daemon
sync
echo PULSE_NIX_REPAIRED
if test -f /var/db/pulse-tart/intent && ! test -f /var/db/pulse-tart/complete; then
  echo PULSE_NIX_EXISTING_JOB
fi
'''


class RepairError(Exception):
    pass


def require(condition, message):
    if not condition:
        raise RepairError(message)


def private_read(path):
    fd = os.open(path, os.O_RDONLY | os.O_NOFOLLOW)
    with os.fdopen(fd, 'r') as stream:
        info = os.fstat(stream.fileno())
        require(stat.S_ISREG(info.st_mode) and info.st_mode & 0o077 == 0,
                'Expected a private regular file: ' + str(path))
        require(info.st_size <= 4 * 1024 * 1024, 'Oversized metadata: ' + str(path))
        return stream.read()


class Repair:
    def __init__(self, args):
        self.args = args
        self.home = args.tart_home.expanduser().absolute()
        self.env = dict(os.environ, TART_HOME=str(self.home), TART_NO_AUTO_PRUNE='true')

    def command(self, args, **kwargs):
        try:
            result = subprocess.run(args, env=kwargs.pop('env', self.env),
                                    timeout=self.args.timeout, capture_output=True, text=True, **kwargs)
        except subprocess.TimeoutExpired:
            raise RepairError(args[0] + ' timed out') from None
        # Do not print arbitrary SSH output or private connection metadata.
        detail = ''
        if '/nix/var/nix/daemon-socket/socket' in result.stderr and 'Permission denied' in result.stderr:
            detail = ': Nix daemon socket permission denied'
        require(result.returncode == 0, f'{args[0]} failed (exit {result.returncode})' + detail)
        return result.stdout

    def read_metadata(self, name):
        require(re.fullmatch(r'[A-Za-z0-9][A-Za-z0-9._-]*', name), 'Invalid VM name')
        directory = self.home / 'vms' / name
        info = directory.lstat()
        require(stat.S_ISDIR(info.st_mode), 'VM directory is not a real directory: ' + name)
        return json.loads(private_read(directory / 'pulse.json'))

    def metadata(self, name):
        data = self.read_metadata(name)
        directory = self.home / 'vms' / name
        info = directory.lstat()
        native = data.get('native') or {}
        spec = native.get('spec') or {}
        require(data.get('version') == 1 and re.fullmatch(r'[a-f0-9]{64}', data.get('scope', ''))
                and spec.get('name') == name and native.get('uid'), 'Invalid Pulse ownership: ' + name)
        require(info.st_ino == native.get('inode') and info.st_dev == native.get('device')
                and private_read(directory / '.pulse-owner') == native['uid'],
                'VM directory identity changed: ' + name)
        return data

    def inspect(self, name, expected):
        data = self.metadata(name)
        require(data['scope'] == expected['scope'] and data['native']['uid'] == expected['native']['uid'],
                'VM was replaced: ' + name)
        vm = json.loads(self.command(['tart', 'get', name, '--format', 'json']))
        require(vm.get('OS') == 'linux' and vm.get('MACAddress') == data['native'].get('mac'),
                'VM hardware identity changed: ' + name)
        return vm

    def plan(self):
        inventory = json.loads(self.command(['tart', 'list', '--source', 'local', '--format', 'json']))
        require(isinstance(inventory, list), 'Invalid Tart inventory')
        selected, seen = [], set()
        for item in inventory:
            name = item['Name']
            if self.args.vm and name not in self.args.vm:
                continue
            require(re.fullmatch(r'[A-Za-z0-9][A-Za-z0-9._-]*', name), 'Invalid VM name in inventory')
            path = self.home / 'vms' / name / 'pulse.json'
            if not path.exists():
                continue
            # Classify before enforcing repair-target identity. Published macOS
            # sources can retain stale inode/device metadata after replacement,
            # and neither those sources nor Docker companions are repair targets.
            data = self.read_metadata(name)
            native = data.get('native') or {}
            if (native.get('spec') or {}).get('os') != 'linux':
                continue
            props = data.get('properties') or {}
            pair, image = props.get('pair') or {}, props.get('image') or {}
            # A macOS Docker companion has no job user or Nix installation.
            primary = (pair.get('Primary') or {}).get('name') == name
            source = image.get('kind') == 'base' and image.get('phase') == 'ready'
            if not (primary or source):
                continue
            # All selected Linux VMs still need the full ownership checks.
            data = self.metadata(name)
            seen.add(name)
            vm = self.inspect(name, data)
            if vm.get('State') == 'running':
                selected.append((name, data, False))
            elif vm.get('State') == 'stopped' and source and data['native'].get('reusable') and not pair and not props.get('job'):
                if self.args.include_stopped:
                    selected.append((name, data, True))
                else:
                    print('SKIP stopped base: ' + name + ' (use --include-stopped with Pulse stopped)', flush=True)
            else:
                print('SKIP non-running worker: ' + name, flush=True)
        if self.args.vm:
            require(set(self.args.vm) <= seen, 'Requested VM is not an owned Linux worker or prepared base')
        require(seen, 'No owned Linux worker VMs found in this Tart home')
        return selected

    def ssh(self, name, data, script):
        self.inspect(name, data)
        address = str(ipaddress.ip_address(self.command(['tart', 'ip', name, '--wait', '5']).strip()))
        native, spec = data['native'], data['native']['spec']
        user = spec.get('username', '')
        require(re.fullmatch(r'[A-Za-z0-9_][A-Za-z0-9_-]*', user), 'Invalid SSH username')
        with tempfile.TemporaryDirectory(prefix='pulse-nix-ssh-') as temporary:
            work = Path(temporary)
            askpass = work / 'askpass'
            askpass.write_text('#!/bin/sh\nprintf "%s\\n" "$PULSE_NIX_SSH_PASSWORD"\n')
            askpass.chmod(0o700)
            known, strict = work / 'known_hosts', 'accept-new'
            if native.get('host_key'):
                key = base64.b64decode(native['host_key'], validate=True).decode().strip()
                require('\n' not in key, 'Invalid pinned SSH host key')
                known.write_text(address + ' ' + key + '\n')
                strict = 'yes'
            env = dict(self.env, SSH_ASKPASS=str(askpass), SSH_ASKPASS_REQUIRE='force', DISPLAY='pulse-nix-repair',
                       PULSE_NIX_SSH_PASSWORD=spec.get('password', ''))
            return self.command(['ssh', '-T', '-o', 'ConnectTimeout=10', '-o', 'NumberOfPasswordPrompts=1',
                                 '-o', 'PreferredAuthentications=password', '-o', 'StrictHostKeyChecking=' + strict,
                                 '-o', 'UserKnownHostsFile=' + str(known), '-o', 'GlobalKnownHostsFile=/dev/null',
                                 user + '@' + address, 'sudo -n /bin/bash -s'], env=env, input=script)

    def start(self, name):
        return subprocess.Popen(['tart', 'run', name, '--no-graphics'], env=self.env,
                                stdin=subprocess.DEVNULL, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)

    def repair_one(self, name, data, boot):
        vm = self.inspect(name, data)
        require(vm['State'] == ('stopped' if boot else 'running'), 'VM state changed: ' + name)
        process = self.start(name) if boot else None
        try:
            if process is not None:
                deadline = time.monotonic() + self.args.timeout
                while True:
                    require(process.poll() is None, 'Tart exited while booting ' + name)
                    try:
                        self.ssh(name, data, 'true\n')
                        break
                    except RepairError:
                        require(time.monotonic() < deadline, 'Guest boot/SSH timed out: ' + name)
                        time.sleep(1)
            script = GUEST_REPAIR
            if boot:
                # Prepared bases must never contain an accepted job, even if
                # host metadata is stale. Do not start the guest launcher.
                script = 'test ! -e /var/db/pulse-tart/intent\n' + script
                script = 'set -e\n' + script
            output = self.ssh(name, data, script)
            require('PULSE_NIX_REPAIRED' in output.splitlines(), 'Nix verification marker missing: ' + name)
            if 'PULSE_NIX_EXISTING_JOB' in output.splitlines():
                print('NOTE ' + name + ': existing job left running; new group membership applies to new processes', flush=True)
        finally:
            if process is not None:
                self.inspect(name, data)
                if process.poll() is None:
                    self.command(['tart', 'stop', name])
                try:
                    process.wait(timeout=30)
                except subprocess.TimeoutExpired:
                    raise RepairError('Tart did not stop the temporarily booted base: ' + name) from None
                require(self.inspect(name, data)['State'] == 'stopped', 'Prepared base did not return to stopped: ' + name)

    def run(self):
        plan = self.plan()
        for name, _, boot in plan:
            print(('BOOT + REPAIR ' if boot else 'REPAIR ') + name, flush=True)
        if self.args.check:
            print('Check only; no VMs changed.')
            return 0
        with ExitStack() as locks:
            # Holding the normal Pulse pool locks prevents source cloning and
            # another controller from starting while a stopped base is edited.
            for scope in sorted({data['scope'] for _, data, boot in plan if boot}):
                path = self.home / '.pulse' / scope / 'lock'
                fd = os.open(path, os.O_RDONLY | os.O_NOFOLLOW)
                stream = locks.enter_context(os.fdopen(fd, 'r'))
                try:
                    fcntl.flock(stream.fileno(), fcntl.LOCK_EX | fcntl.LOCK_NB)
                except BlockingIOError:
                    raise RepairError('Stop Pulse normally, then rerun --include-stopped; Pulse still owns the pool lock') from None
            failed = 0
            for name, data, boot in plan:
                try:
                    self.repair_one(name, data, boot)
                    print('OK ' + name, flush=True)
                except (RepairError, OSError, ValueError, KeyError, TypeError) as error:
                    failed += 1
                    # Metadata parse errors can contain private content; only
                    # our fixed repair errors are suitable for terminal output.
                    detail = str(error) if isinstance(error, RepairError) else type(error).__name__
                    print('FAILED ' + name + ': ' + detail, flush=True)
            print(f'Verified {len(plan) - failed} VM(s); failed {failed}. No jobs were restarted.')
            return int(failed != 0)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--tart-home', type=Path, default=Path(os.environ.get('TART_HOME') or Path.home() / '.tart'))
    parser.add_argument('--vm', action='append', help='Only repair this VM (repeatable); default: all owned Linux workers')
    parser.add_argument('--include-stopped', action='store_true', help='Also boot/repair stopped prepared Linux bases; Pulse must be stopped')
    parser.add_argument('--check', action='store_true', help='Show the repair plan without changing any VM')
    parser.add_argument('--timeout', type=int, default=120, help='Command/boot timeout in seconds (default: 120)')
    args = parser.parse_args()
    if args.timeout < 1:
        parser.error('--timeout must be positive')
    if not shutil.which('tart') or not shutil.which('ssh'):
        parser.error('Run this script on the Mac host with tart and ssh installed')
    try:
        return Repair(args).run()
    except (RepairError, OSError, ValueError, KeyError, TypeError) as error:
        print(str(error) if isinstance(error, RepairError) else 'Repair failed: ' + type(error).__name__)
        return 1


if __name__ == '__main__':
    raise SystemExit(main())
