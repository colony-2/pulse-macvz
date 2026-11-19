#!/usr/bin/env python3
"""Offline repair of proven source builders; preserve private backups and jobs."""
import base64
from contextlib import ExitStack
import fcntl
import json
import os
from pathlib import Path
import re
import subprocess
import sys
import tempfile
import time


def require(condition, message):
    if not condition:
        raise ValueError(message)


def read(path):
    require(not path.is_symlink() and path.is_file(), 'Expected regular file: ' + str(path))
    return json.loads(path.read_text())


def locked(stack, path):
    require(not path.is_symlink(), 'Unsafe lock: ' + str(path))
    path.parent.mkdir(parents=True, exist_ok=True, mode=0o700)
    stream = stack.enter_context(path.open('a+'))
    try:
        fcntl.flock(stream, fcntl.LOCK_EX | fcntl.LOCK_NB)
    except BlockingIOError:
        raise ValueError('Pool is in use. Stop Pulse before repairing incomplete sources: ' + str(path)) from None
    return stream


def observe(name):
    p = subprocess.run(['tart', 'get', name, '--format', 'json'], capture_output=True, text=True, timeout=30)
    require(p.returncode == 0, 'Cannot observe builder: ' + name)
    return json.loads(p.stdout)


def builder(home, name, uid):
    require(re.fullmatch(r'[A-Za-z0-9][A-Za-z0-9._-]*', name), 'Invalid builder name')
    directory = home / 'vms' / name
    require(not directory.is_symlink(), 'Builder directory is a symlink')
    data = read(directory / 'pulse.json')
    native, props = data.get('native') or {}, data.get('properties') or {}
    spec = native.get('spec') or {}
    require(data.get('version') == 1 and native.get('uid') == uid and spec.get('name') == name,
            'Builder identity changed: ' + name)
    require(not native.get('reusable') and (props.get('slot') or props.get('image'))
            and not props.get('job') and not props.get('pair'), 'Refusing to repair a job or reusable VM: ' + name)
    st = directory.stat()
    marker = directory / '.pulse-owner'
    require(not marker.is_symlink() and marker.read_text() == uid and
            (st.st_dev, st.st_ino) == (native.get('device'), native.get('inode')), 'Builder ownership changed: ' + name)
    vm = observe(name)
    require(all(vm.get(k) == v for k, v in [('CPU', spec.get('cpu')), ('Memory', spec.get('memory')),
                                         ('MACAddress', native.get('mac'))]), 'Builder hardware changed: ' + name)
    return data, vm


def retire_legacy(stack, home, scopes, backup):
    # Only retire an obsolete key when every surviving VM in that scope has
    # authoritative attached properties. Never import or synthesize a key.
    root = Path(os.environ.get('PULSE_LEGACY_PROVIDERS_DIR', str(Path.home() / 'Library/Application Support/pulse/providers')))
    for directory in sorted(root.glob('pulse-tart-*')):
        if not (directory / 'state.json').exists() or '.retired-' in directory.name:
            continue
        old = read(directory / 'state.json')
        if old.get('scope') not in scopes:
            continue
        require(not directory.is_symlink(), 'Legacy journal is a symlink')
        locked(stack, directory / 'lock')
        old = read(directory / 'state.json')
        current = []
        for path in (home / 'vms').iterdir():
            if not (path / 'pulse.json').exists():
                continue
            data = read(path / 'pulse.json')
            if data.get('scope') == old['scope']:
                current.append(data.get('properties') or {})
        keys = {p.get('key') for p in current}
        require(len(keys) == 1 and None not in keys, 'Incomplete or conflicting attached keys; collect diagnostics before repair')
        if old.get('key') in keys:
            continue
        require(old.get('version') == 3 and all(p.get('version') == 1 and p.get('scope') == old['scope'] and
                not p.get('job') and not p.get('pair') for p in current), 'Ambiguous legacy state; collect diagnostics before repair')
        require(all(len(base64.b64decode(k, validate=True)) == 32 for k in keys | {old.get('key', '')}), 'Invalid legacy key')
        for path in (directory / 'native').glob('*.json'):
            target = home / 'vms' / path.stem
            require(not target.exists() or (target / 'pulse.json').is_file(), 'Unmigrated legacy VM; collect diagnostics before repair')
        destination = directory.with_name(directory.name + '.retired-' + backup.name)
        directory.rename(destination)
        print('Retired obsolete journal; private backup: ' + str(destination), flush=True)


def repair(plan_path, command):
    plan = read(plan_path)
    home = Path(plan['home']).resolve()
    require(home == Path(os.environ.get('TART_HOME') or Path.home() / '.tart').resolve(), 'Repair plan Tart home mismatch')
    with ExitStack() as stack:
        require(re.fullmatch(r'[A-Za-z0-9][A-Za-z0-9._-]*', plan['base_name']), 'Invalid base name')
        update_lock = home / ('.' + plan['base_name'] + '.update-lock')
        # Acquire before retiring or moving anything, and retain it through the
        # child updater. A check followed by mkdir in the child would race.
        update_lock.mkdir()
        stack.callback(update_lock.rmdir)
        scopes = set()
        for path in (home / 'vms').iterdir():
            if path.name.startswith('pulse-' + plan['owner'] + '-') and (path / 'pulse.json').exists():
                data = read(path / 'pulse.json')
                scope = data.get('scope', '')
                require(re.fullmatch('[a-f0-9]{64}', scope), 'Invalid provider scope')
                scopes.add(scope)
        # Standalone source pools have no provider metadata. Lock every existing
        # runtime scope conservatively when the matching scope is unknown.
        locks = [home / '.pulse' / s / 'lock' for s in scopes] or list((home / '.pulse').glob('*/lock'))
        for path in sorted(locks):
            locked(stack, path)
        for name, uid in plan['builders'].items():
            builder(home, name, uid)
        for name in plan['missing']:
            require(re.fullmatch(r'[A-Za-z0-9][A-Za-z0-9._-]*', name) and not (home / 'vms' / name).exists(), 'Missing source appeared during repair: ' + name)
        backup_root = home / '.pulse-image-repairs'
        backup_root.mkdir(mode=0o700, exist_ok=True)
        backup = Path(tempfile.mkdtemp(prefix=time.strftime('%Y%m%dT%H%M%SZ-'), dir=backup_root))
        print('Repair backup: ' + str(backup), flush=True)
        retire_legacy(stack, home, scopes, backup)
        (backup / 'repair-plan.json').write_text(json.dumps(plan, indent=2))
        for name, uid in plan['builders'].items():
            _, vm = builder(home, name, uid)
            if vm['State'] == 'running':
                subprocess.run(['tart', 'stop', name], check=True, timeout=60)
            # Tart stop has completed. Recheck ownership and the kernel's config
            # lock before moving files; a VM still running must never be moved.
            with ExitStack() as vm_stack:
                fd = os.open(home, os.O_RDONLY)
                vm_stack.callback(os.close, fd)
                fcntl.flock(fd, fcntl.LOCK_EX | fcntl.LOCK_NB)
                _, vm = builder(home, name, uid)
                require(vm['State'] in ('stopped', 'suspended'), 'Builder did not stop: ' + name)
                config = vm_stack.enter_context((home / 'vms' / name / 'config.json').open('r+'))
                fcntl.lockf(config.fileno(), fcntl.LOCK_EX | fcntl.LOCK_NB)
                (home / 'vms' / name).rename(backup / name)
            plan['missing'].append(name)
        plan['builders'] = {}
        plan_path.write_text(json.dumps(plan))
        subprocess.run(command, check=True, env=dict(os.environ, TART_HOME=str(home),
                                                   PULSE_IMAGE_UPDATE_LOCK=str(update_lock)))


if __name__ == '__main__':
    os.umask(0o077)
    try:
        require(len(sys.argv) >= 4 and sys.argv[2] == '--', 'Internal use: repair-image-pool.py PLAN -- COMMAND ...')
        repair(Path(sys.argv[1]), sys.argv[3:])
    except (ValueError, KeyError, OSError, subprocess.SubprocessError):
        # Peer/metadata errors may contain private material; print no raw JSON.
        print('Pool repair stopped. Pulse must be stopped; identities, locks and legacy state must be consistent. '
              'Run diagnose-tart.sh for details. Any announced private backups were retained.', file=sys.stderr)
        sys.exit(1)
