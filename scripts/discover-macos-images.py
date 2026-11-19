#!/usr/bin/env python3
"""Discover existing Pulse macOS sources and drive the ordinary image updater."""
import argparse
import importlib.util
import json
import os
from pathlib import Path
import re
import subprocess
import sys
import tempfile

ROOT = Path(__file__).resolve().parent.parent
spec = importlib.util.spec_from_file_location('image_names', ROOT / 'scripts/image-names.py')
image_names = importlib.util.module_from_spec(spec)
spec.loader.exec_module(image_names)
TOKEN = r'[A-Za-z0-9][A-Za-z0-9._-]{0,23}'
ORCHARD = re.compile(r'^orchard-(pulse-.+)-[0-9a-fA-F]{8}(?:-[0-9a-fA-F]{4}){3}-[0-9a-fA-F]{12}-[0-9]+$')
TEST_OWNER = re.compile(r'test-[0-9a-f]{16}')


def tart(*args):
    result = subprocess.run(['tart', *args], capture_output=True, text=True, timeout=120)
    if result.returncode:
        raise ValueError(f'tart {args[0]} failed; no discovery update started')
    return json.loads(result.stdout)


def source_role(home, name, repair=False):
    """Use attached properties when available; never print their private fields."""
    path = home / 'vms' / name / 'pulse.json'
    if not path.exists():
        return None  # Legacy and standalone sources have canonical names only.
    try:
        data = json.loads(path.read_text())
        native, properties = data['native'], data.get('properties') or {}
        if data['version'] != 1 or native['spec']['name'] != name:
            raise ValueError()
        if properties.get('job') or properties.get('pair'):
            return 'job'
        if not native.get('reusable'):
            if repair and (properties.get('slot') or properties.get('image')):
                return 'builder'
            raise ValueError()
        return 'source'
    except (ValueError, KeyError, TypeError):
        raise ValueError(f'Cannot identify a reusable source from VM properties: {name}') from None


def discover(inventory, catalog, get, home, owner=None, base=None, group=None, repair=False, slots_count=None):
    canonical = {}
    for item in inventory:
        name = item['Name']
        if not re.fullmatch(r'[A-Za-z0-9][A-Za-z0-9._-]*', name):
            continue
        match = ORCHARD.fullmatch(name)
        key = match[1] if match else name
        if key.startswith('pulse-'):
            canonical.setdefault(key, []).append(name)
    sources, slots, bases, ignored, builders = {}, [], [], [], {}
    catalog = [entry for entry in catalog if entry['Name'].startswith('macos-')]
    for name in sorted(canonical):
        match = None
        for entry in catalog:
            prefix = rf'pulse-({TOKEN})-'
            ordinary = re.fullmatch(prefix + re.escape(image_names.token(entry['Name'])) + r'-base', name)
            docker = re.fullmatch(prefix + re.escape(image_names.token(entry['Name'] + '-docker')) + r'-base', name)
            slot = re.fullmatch(prefix + re.escape(image_names.token(entry['Name'])) +
                                rf'-({TOKEN})-([1-9][0-9]*)cpu-([1-9][0-9]*)m-slot-([0-9]{{4}})', name)
            match = ordinary or docker or slot
            if match:
                break
        if not match:
            if name.endswith('-base') or re.search(r'-slot-[0-9]{4}$', name):
                # Linux bases and temporary test pools are outside this updater.
                if re.match(r'^pulse-test-[0-9a-f]{16}-', name) or name.startswith('pulse-build-'):
                    continue
                if any(get(alias).get('OS') == 'darwin' for alias in canonical[name]):
                    ignored.append(name)
            continue
        pool_owner = match[1]
        if TEST_OWNER.fullmatch(pool_owner) and not owner:
            continue
        if owner and image_names.token(owner) != pool_owner:
            continue
        if base and entry['Name'] != base:
            continue
        if slot and group and slot[2] != image_names.token(group) and not slot[2].startswith(group + '-docker-'):
            continue
        expected = (int(slot[3]), int(slot[4])) if slot else (4, 8192)
        if not (1 <= expected[0] <= 256 and 1024 <= expected[1] <= 1048576):
            raise ValueError(f'Unsupported hardware in source name: {name}')
        aliases = []
        for alias in sorted(canonical[name]):
            role = source_role(home, alias, repair)
            if role == 'job':
                continue
            vm = get(alias)
            if (vm.get('OS') != 'darwin' or (role != 'builder' and vm.get('State') != ('suspended' if slot else 'stopped'))
                    or (vm.get('CPU'), vm.get('Memory')) != expected):
                raise ValueError(f'Source is not ready or its hardware disagrees with its name: {alias}')
            if role == 'builder':
                data = json.loads((home / 'vms' / alias / 'pulse.json').read_text())
                builders[alias] = data['native']['uid']
            aliases.append(alias)
        if not aliases:
            continue
        sources[name] = aliases
        if slot:
            index = int(slot[5])
            if not 1 <= index <= 1000:
                raise ValueError(f'Unsupported slot index: {name}')
            slots.append((pool_owner, entry, slot[2], *expected, index, name))
        elif ordinary:
            bases.append((pool_owner, entry, name))
    if ignored and not (owner or base):
        raise ValueError('Cannot infer the recipe for these macOS sources; use explicit options: ' + ', '.join(ignored))
    pools = {}
    for pool_owner, entry, encoded_group, cpu, memory, index, name in slots:
        profile, pool_group = '', encoded_group
        docker_base = image_names.names(pool_owner, entry['Name'], 'unused', 4, 8192, 1, 'engine')[0]
        if '-docker-' in encoded_group and docker_base in sources:
            pool_group, profile = encoded_group.rsplit('-docker-', 1)
        elif '-docker-' in encoded_group:
            raise ValueError(f'Cannot distinguish a Docker profile from a group name for {name}; use explicit options')
        elif encoded_group.startswith('id-') and docker_base in sources:
            raise ValueError(f'Cannot recover a hashed Docker group/profile for {name}; use explicit options')
        if group and image_names.token(group + ('-docker-' + profile if profile else '')) != encoded_group:
            continue
        key = (pool_owner, entry['Name'], pool_group, cpu, memory, profile)
        base_name = image_names.names(pool_owner, entry['Name'], pool_group, cpu, memory, 1, profile)[0]
        if key not in pools:
            pools[key] = dict(owner=pool_owner, base=entry['Name'], group=pool_group, cpu=cpu,
                              memory=memory, profile=profile, count=0, targets={},
                              ssh_user=entry.get('SSHUsername', 'admin'))
            if base_name in sources:
                pools[key]['targets'][base_name] = sources[base_name]
        pool = pools[key]
        pool['count'] = max(pool['count'], index)
        pool['targets'][name] = sources[name]
    used = {name for pool in pools.values() for name in pool['targets']}
    for pool_owner, entry, name in bases:
        if name not in used and not group:
            # A retained base without slots can be updated too. The target map
            # prevents the explicit updater's default slot from being created.
            base_group = ('standard' if entry['Name'] == catalog[0]['Name'] else 'mac-' + image_names.token(entry['Name'])) if repair else 'standard'
            key = (pool_owner, entry['Name'], base_group, 4, 8192, '')
            pools[key] = dict(owner=pool_owner, base=entry['Name'], group=base_group, cpu=4,
                              memory=8192, profile='', count=1, targets={name: sources[name]},
                              ssh_user=entry.get('SSHUsername', 'admin'))
    result = [pools[key] for key in sorted(pools)]
    if repair:
        for pool in result:
            default_group = 'standard' if pool['base'] == catalog[0]['Name'] else 'mac-' + image_names.token(pool['base'])
            # Recognize the standard two-slot recipe. Custom counts/groups are
            # preserved unless --slots explicitly supplies the desired count.
            standard = not pool['profile'] and pool['cpu'] == 4 and pool['memory'] == 8192 and pool['group'] == default_group and pool['count'] <= 2
            if slots_count is not None or standard:
                pool['count'] = slots_count if slots_count is not None else 2
                for name in image_names.names(pool['owner'], pool['base'], pool['group'], pool['cpu'], pool['memory'], pool['count'], pool['profile']):
                    if name not in canonical or name in sources:
                        pool['targets'].setdefault(name, sources.get(name, [name]))
            pool['missing'] = [alias for aliases in pool['targets'].values() for alias in aliases if alias not in {v['Name'] for v in inventory}]
            pool['builders'] = {alias: builders[alias] for aliases in pool['targets'].values() for alias in aliases if alias in builders}
    return result


def main():
    # Internal selectors used by the shell updater. Plans contain names only,
    # live for this invocation, and are never a persistent VM inventory.
    if len(sys.argv) == 4 and sys.argv[1] in ('selected', 'targets'):
        targets = json.loads(Path(sys.argv[2]).read_text()).get(sys.argv[3], [])
        if sys.argv[1] == 'selected':
            return 0 if targets else 1
        print('\n'.join(targets)) if targets else None
        return 0
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--discover', action='store_true')
    parser.add_argument('--list', action='store_true')
    parser.add_argument('--owner')
    parser.add_argument('--base')
    parser.add_argument('--group')
    parser.add_argument('--c2j', default='latest')
    parser.add_argument('--ssh-user')
    parser.add_argument('--timeout', type=int, default=2700)
    parser.add_argument('--ensure', action='store_true')
    parser.add_argument('--slots', type=int, help='Explicit desired count for discovered pools')
    args = parser.parse_args()
    if not 60 <= args.timeout <= 7200:
        parser.error('--timeout must be between 60 and 7200 seconds')
    if args.slots is not None and not 1 <= args.slots <= 1000:
        parser.error('--slots must be between 1 and 1000')
    home = Path(os.environ.get('TART_HOME') or Path.home() / '.tart')
    try:
        pools = discover(tart('list', '--source', 'local', '--format', 'json'),
                         json.loads((ROOT / 'bases.json').read_text()),
                         lambda name: tart('get', name, '--format', 'json'), home,
                         args.owner, args.base, args.group, repair=True, slots_count=args.slots)
        if not pools:
            raise ValueError('No matching prepared macOS template pools found; use --owner with explicit options to create a new pool')
        print(f'Found {len(pools)} template pool(s):', flush=True)
        for pool in pools:
            print(f'  {pool["owner"]}: {pool["base"]} / {pool["group"]} / {pool["cpu"]} CPU, {pool["memory"]} MiB', flush=True)
            for aliases in pool['targets'].values():
                for name in aliases:
                    note = ' (missing; will create)' if name in pool['missing'] else (' (unfinished builder; offline repair)' if name in pool['builders'] else '')
                    print('    ' + name + note, flush=True)
        if args.list:
            return 0
        if args.ensure and not any(pool['missing'] or pool['builders'] for pool in pools):
            print('All discovered sources already exist; --ensure has nothing to create.')
            return 0
        with tempfile.TemporaryDirectory(prefix='pulse-image-discovery-') as directory:
            work = Path(directory)
            # Resolve main/latest once so all discovered pools get the same c2j.
            (work / 'flake.nix').write_bytes((ROOT / 'flake.nix').read_bytes())
            version = args.c2j
            if not args.ensure:
                subprocess.run(['bash', str(ROOT / 'scripts/select-c2j.sh'), str(work), version], check=True)
                version = re.search(r'inputs\.c2j\.url = "github:colony-2/c2j/([0-9a-f]{40})"',
                                    (work / 'flake.nix').read_text())[1]
            for i, pool in enumerate(pools):
                path = work / f'targets-{i}.json'
                path.write_text(json.dumps(pool['targets']))
                command = ['bash', str(ROOT / 'scripts/update-macos-images.sh'),
                           '--owner', pool['owner'], '--base', pool['base'], '--group', pool['group'],
                           '--slots', str(pool['count']), '--cpu', str(pool['cpu']), '--memory', str(pool['memory']),
                           '--c2j', version, '--timeout', str(args.timeout),
                           '--ssh-user', args.ssh_user or pool['ssh_user'], '--existing-targets', str(path)]
                if pool['profile']:
                    command.extend(['--docker-profile', pool['profile']])
                if args.ensure:
                    command.append('--ensure')
                if pool['missing'] or pool['builders']:
                    repair = work / f'repair-{i}.json'
                    repair.write_text(json.dumps({'home': str(home.resolve()), 'owner': pool['owner'],
                                                  'base_name': image_names.names(pool['owner'], pool['base'], pool['group'], pool['cpu'], pool['memory'], 1, pool['profile'])[0],
                                                  'missing': pool['missing'], 'builders': pool['builders']}))
                    command.extend(['--repair-plan', str(repair)])
                    command = [sys.executable, str(ROOT / 'scripts/repair-image-pool.py'), str(repair), '--', *command]
                subprocess.run(command, check=True)
        return 0
    except (ValueError, OSError, subprocess.SubprocessError) as error:
        print(f'Image discovery/update failed: {error}', file=sys.stderr)
        return 1


if __name__ == '__main__':
    raise SystemExit(main())
