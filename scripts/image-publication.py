#!/usr/bin/env python3
"""Checkpoint and resume publication of prepared images; never prepare or run a VM."""
import argparse
import importlib.util
import json
import os
from pathlib import Path
import re
import subprocess
import sys

ROOT = Path(__file__).resolve().parent.parent
spec = importlib.util.spec_from_file_location('image_names', ROOT / 'scripts/image-names.py')
names = importlib.util.module_from_spec(spec)
spec.loader.exec_module(names)
NAME = re.compile(r'^[A-Za-z0-9][A-Za-z0-9._-]*$')


def save(path, value):
    temporary = path.with_suffix(path.suffix + '.tmp')
    temporary.write_text(json.dumps(value, indent=2) + '\n')
    temporary.chmod(0o600)
    os.replace(temporary, path)


def tart(*args):
    p = subprocess.run(['tart', *args], capture_output=True, text=True, timeout=120)
    if p.returncode:
        raise ValueError('Tart observation failed: ' + args[0])
    return json.loads(p.stdout)


def fingerprint(home, name):
    if not NAME.fullmatch(name):
        raise ValueError('Invalid VM name')
    path = home / 'vms' / name
    if not path.exists():
        return None
    if path.is_symlink() or not path.is_dir():
        raise ValueError('VM directory must be a real directory')
    info = path.stat()
    out = {'directory': [info.st_dev, info.st_ino], 'files': {}}
    for filename in ('config.json', 'disk.img', 'nvram.bin', 'state.vzvmsave'):
        p = path / filename
        if not p.exists():
            if filename == 'state.vzvmsave':
                continue
            raise ValueError('Incomplete VM files: ' + name)
        if p.is_symlink() or not p.is_file():
            raise ValueError('VM files must be regular')
        s = p.stat()
        out['files'][filename] = [s.st_dev, s.st_ino, s.st_size, s.st_mtime_ns, s.st_ctime_ns]
    return out


def source_only(home, name):
    p = home / 'vms' / name / 'pulse.json'
    if p.exists():
        if p.is_symlink():
            raise ValueError('Unsafe VM metadata')
        data = json.loads(p.read_text())
        props = data.get('properties') or {}
        if props.get('job') or props.get('pair') or not data.get('native', {}).get('reusable'):
            raise ValueError('Refusing publication over a job or unfinished provider builder: ' + name)


def prepare(work, home, base, existing, repair=None, ensure=False):
    missing = set()
    if repair:
        repair_plan = json.loads(Path(repair).read_text())
        if repair_plan['home'] != str(home.resolve()) or repair_plan['builders']:
            raise ValueError('Incomplete repair plan')
        missing = set(repair_plan['missing'])
    pairs = json.loads((work / 'prepared-sources.json').read_text())
    inventory = tart('list', '--source', 'local', '--format', 'json')
    selected = json.loads(Path(existing).read_text()) if existing else None
    plan = {'version': 1, 'home': str(home.resolve()), 'base': base, 'items': [], 'complete': False}
    for pair in pairs:
        source, target = pair['prepared'], pair['target']
        if not source.startswith('pulse-build-'):
            raise ValueError('Expected an updater-owned builder')
        observed = tart('get', source, '--format', 'json')
        state = 'stopped' if target == base else 'suspended'
        if observed['State'] != state:
            raise ValueError('Prepared source changed state')
        aliases = selected.get(target, []) if selected is not None else names.aliases(target, inventory)
        for alias in aliases:
            source_only(home, alias)
            old = fingerprint(home, alias)
            if ensure and old is not None:
                current = tart('get', alias, '--format', 'json')
                if any(current.get(key) != observed.get(key) for key in ('State', 'CPU', 'Memory')):
                    raise ValueError('Existing source is not ready: ' + alias)
                continue
            if selected is not None and old is None and alias not in missing:
                raise ValueError('Discovered source disappeared: ' + alias)
            plan['items'].append({'source': source, 'target': alias, 'state': state,
                                  'source_fingerprint': fingerprint(home, source),
                                  'target_before': old, 'done': False})
    save(work / 'publication-plan.json', plan)


def verify(work, index, source, target, state):
    report = work / f'publication-{index}-verification.json'
    with report.open('w') as out:
        p = subprocess.run([sys.executable, str(ROOT / 'scripts/verify-image-publication.py'),
                            source, target, state], stdout=out)
    return p.returncode == 0


def publish(work, home, resume=False):
    plan = json.loads((work / 'publication-plan.json').read_text())
    if plan.get('version') != 1 or plan['home'] != str(home.resolve()):
        raise ValueError('Publication plan has a different Tart home or unsupported format')
    if plan['complete']:
        print('Publication already completed.', flush=True)
        return
    # Validate ALL retained builders before the first resumed write.
    for item in plan['items']:
        if not item['source'].startswith('pulse-build-') or fingerprint(home, item['source']) != item['source_fingerprint']:
            raise ValueError('Prepared source changed; cannot resume: ' + item['source'])
    confirmed = 0
    for index, item in enumerate(plan['items']):
        source, target, state = (item[k] for k in ('source', 'target', 'state'))
        source_only(home, target)
        if resume and fingerprint(home, target) is not None and verify(work, index, source, target, state):
            item['done'] = True
            confirmed += 1
            save(work / 'publication-plan.json', plan)
            (work / 'confirmed-count').write_text(str(confirmed))
            print('Already published and verified: ' + target, flush=True)
            continue
        if item['done'] or fingerprint(home, target) != item['target_before']:
            raise ValueError('Publication target changed; refusing overwrite: ' + target)
        # Absence is allowed only if the original plan explicitly created a name.
        if item['target_before'] is not None:
            observed = tart('get', target, '--format', 'json')
            if observed['State'] != state:
                raise ValueError('Publication target changed state: ' + target)
        print('Publishing: ' + target, flush=True)
        for label, name in (('source-before', source), ('target-before', target)):
            try: save(work / f'publication-{index}-{label}.json', tart('get', name, '--format', 'json'))
            except ValueError: pass
        with (work / f'publication-{index}.stdout').open('w') as out, (work / f'publication-{index}.stderr').open('w') as err:
            p = subprocess.run(['tart', 'clone', '--overwrite', source, target], stdout=out, stderr=err)
        print((work / f'publication-{index}.stdout').read_text(), end='', flush=True)
        print((work / f'publication-{index}.stderr').read_text(), end='', file=sys.stderr, flush=True)
        if p.returncode:
            (work / f'publication-{index}.exit').write_text(str(p.returncode) + '\n')
            for label, name in (('source-after', source), ('target-after', target)):
                try: save(work / f'publication-{index}-{label}.json', tart('get', name, '--format', 'json'))
                except ValueError: pass
            save(work / 'publication-inventory.json', tart('list', '--source', 'local', '--format', 'json'))
            print('Tart returned an error; checking the complete prepared image (full file comparison).', file=sys.stderr, flush=True)
            if not verify(work, index, source, target, state):
                raise ValueError('Publication could not be verified; prepared VMs retained')
            (work / 'verified-publication-error').touch()
            print(f'Verified complete publication despite Tart exit {p.returncode}: {target}', file=sys.stderr, flush=True)
        item['done'] = True
        confirmed += 1
        save(work / 'publication-plan.json', plan)
        (work / 'confirmed-count').write_text(str(confirmed))
        print('Ready: ' + target, flush=True)
    plan['complete'] = True
    save(work / 'publication-plan.json', plan)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('mode', choices=('plan', 'publish', 'resume'))
    parser.add_argument('work', type=Path)
    parser.add_argument('--base')
    parser.add_argument('--existing-targets')
    parser.add_argument('--repair-plan')
    parser.add_argument('--ensure', action='store_true')
    args = parser.parse_args()
    home = Path(os.environ.get('TART_HOME') or Path.home() / '.tart').resolve()
    lock = None
    locked = False
    os.umask(0o077)
    try:
        if args.mode == 'plan':
            prepare(args.work, home, args.base, args.existing_targets, args.repair_plan, args.ensure)
        elif args.mode == 'publish':
            publish(args.work, home)
        else:
            plan = json.loads((args.work / 'publication-plan.json').read_text())
            if not NAME.fullmatch(plan['base']): raise ValueError('Invalid base name')
            lock = home / ('.' + plan['base'] + '.update-lock')
            lock.mkdir()
            locked = True
            publish(args.work, home, resume=True)
            for name in sorted({item['source'] for item in plan['items']}):
                if fingerprint(home, name) is not None:
                    expected = next(item['source_fingerprint'] for item in plan['items'] if item['source'] == name)
                    if fingerprint(home, name) != expected:
                        raise ValueError('Retained source changed; refusing cleanup: ' + name)
                    source_only(home, name)
                    subprocess.run(['tart', 'delete', name], check=True)
            print('Resumed publication complete. Evidence retained in ' + str(args.work))
        return 0
    except (OSError, ValueError, KeyError, subprocess.SubprocessError) as error:
        print('Image publication stopped: ' + str(error), file=sys.stderr)
        return 1
    finally:
        if locked:
            lock.rmdir()


if __name__ == '__main__':
    sys.exit(main())
