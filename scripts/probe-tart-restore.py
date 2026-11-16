#!/usr/bin/env python3
"""Restore disposable clones, first alone and then with a second macOS VM."""
import argparse
import datetime
import hashlib
import ipaddress
import json
import os
from pathlib import Path
import platform
import subprocess
import tarfile
import time
import uuid


class Probe:
    def __init__(self, output, home):
        self.output, self.home = output, home
        self.env = dict(os.environ, TART_HOME=str(home), TART_NO_AUTO_PRUNE='1')
        self.clones = {}
        self.counter = 0
        self.prefix = 'pulse-restore-probe-' + uuid.uuid4().hex[:16]
        self.settle_seconds = 3
        self.poll_seconds = 1

    def command(self, *args):
        result = subprocess.run(['tart', *args], env=self.env, capture_output=True,
                                text=True, timeout=120)
        self.counter += 1
        (self.output / f'command-{self.counter:03d}.json').write_text(json.dumps({
            'arguments': args, 'exit': result.returncode,
            'stdout': result.stdout, 'stderr': result.stderr}, indent=2))
        if result.returncode:
            raise RuntimeError(f'tart {args[0]} failed; see command-{self.counter:03d}.json')
        return result.stdout

    def get(self, name):
        return json.loads(self.command('get', name, '--format', 'json'))

    def inspect(self, name, label):
        metadata = {'name': name, 'native': self.get(name), 'files': {}}
        for filename in ('config.json', 'nvram.bin', 'state.vzvmsave', 'disk.img'):
            path = self.home / 'vms' / name / filename
            if path.is_file():
                st = path.stat()
                entry = {'bytes': st.st_size, 'mtime_ns': st.st_mtime_ns}
                if filename in ('config.json', 'nvram.bin'):
                    entry['sha256'] = hashlib.sha256(path.read_bytes()).hexdigest()
                metadata['files'][filename] = entry
                if filename == 'config.json':
                    config = json.loads(path.read_text())
                    # Compare platform identity without publishing the serial/ECID.
                    for key in ('ecid', 'hardwareModel'):
                        if key in config:
                            metadata[key + '_sha256'] = hashlib.sha256(
                                json.dumps(config[key], sort_keys=True).encode()).hexdigest()
        (self.output / f'{label}.json').write_text(json.dumps(metadata, indent=2))

    def preflight(self, sources):
        identities = []
        for i, source in enumerate(sources):
            info = self.get(source)
            if info.get('State') != 'suspended' or info.get('OS') != 'darwin':
                raise RuntimeError(f'{source} must be a suspended macOS template')
            identities.append(info.get('MACAddress'))
            self.inspect(source, f'source-{i + 1}')
        if not all(identities) or identities[0] == identities[1]:
            raise RuntimeError('Templates must have distinct MAC addresses')
        for vm in json.loads(self.command('list', '--source', 'local', '--format', 'json')):
            if vm.get('State') == 'running' or vm.get('Running'):
                if self.get(vm['Name']).get('OS') != 'linux':
                    raise RuntimeError('Pause other macOS VM workloads before probing')

    def restore(self, source, label):
        name = self.clone(source, label)
        self.run(name, label, restoring=True)
        return name

    def clone(self, source, label):
        name = self.prefix + '-' + label
        # Register before cloning so cleanup also handles an uncertain clone result.
        self.clones[name] = (None, None)
        self.command('clone', source, name)
        self.inspect(name, label + '-before')
        return name

    def run(self, name, label, restoring):
        log_path = self.output / (label + '-run.log')
        log = log_path.open('wb')
        process = subprocess.Popen(['tart', 'run', '--no-graphics', '--suspendable', name],
                                   env=self.env, stdout=log, stderr=subprocess.STDOUT)
        self.clones[name] = (process, log)
        deadline, running_since = time.monotonic() + 90, None
        while time.monotonic() < deadline:
            if process.poll() is not None:
                self.inspect(name, label + '-failed')
                raise RuntimeError(f'{label}: tart run exited {process.returncode}; see {log_path.name}')
            info = self.get(name)
            # Tart marks the VM running while restore is still in progress.
            ready = 'resuming VM' in log_path.read_text(errors='replace') if restoring else False
            if not restoring and info.get('State') == 'running':
                try:
                    ipaddress.ip_address(self.command('ip', name, '--wait', '5').strip())
                    ready = True
                except (RuntimeError, ValueError):
                    pass
            if info.get('State') == 'running' and ready:
                running_since = running_since or time.monotonic()
                if time.monotonic() - running_since >= self.settle_seconds:
                    print(f'{label}: {"restored" if restoring else "booted"}', flush=True)
                    return name
            else:
                running_since = None
            time.sleep(self.poll_seconds)
        raise RuntimeError(f'{label}: start timed out; see {log_path.name}')

    def suspend(self, name, label):
        process, log = self.clones[name]
        self.command('suspend', name)  # One signal; never replay while saving.
        process.wait(timeout=120)
        log.close()
        self.clones[name] = (None, None)
        if process.returncode != 0 or self.get(name).get('State') != 'suspended':
            raise RuntimeError(f'{label}: snapshot preparation failed')
        self.inspect(name, label + '-saved')

    def prepare(self, base, label, hardware, concurrent=False, random_serial=False):
        sources = []
        for i in range(2):
            builder_label = f'{label}-builder-{i + 1}'
            name = self.clone(base, builder_label)
            sources.append(name)
            args = ['set', name, '--cpu', str(hardware['CPU']),
                    '--memory', str(hardware['Memory']), '--random-mac']
            if random_serial:
                args.append('--random-serial')
            self.command(*args)
            self.run(name, builder_label, restoring=False)
            if not concurrent:
                self.suspend(name, builder_label)
        if concurrent:
            # Both VMs must have booted before either releases its host resources.
            for i, name in enumerate(sources):
                self.suspend(name, f'{label}-builder-{i + 1}')
        return sources

    def check(self, sources, label):
        """Record one restore scenario; failed disposable VMs are never retried."""
        before = set(self.clones)
        result = {'case': label, 'sources': sources, 'result': 'failed'}
        try:
            restored = [self.restore(source, f'{label}-{i + 1}')
                        for i, source in enumerate(sources)]
            if any(self.clones[name][0].poll() is not None for name in restored):
                raise RuntimeError('A restored VM exited while starting its peer')
            result['result'] = 'passed'
        except Exception as error:
            result['error'] = str(error)
        finally:
            # Cleanup failure aborts the matrix: do not start more VMs with
            # uncertain occupancy. Prepared sources remain until their case ends.
            for name in list(self.clones):
                if name not in before:
                    self.remove(name)
        print(f'{label}: {result["result"]}', flush=True)
        return result

    def compare_preparation(self, sources, base, rounds, cases):
        info = self.get(base)
        if info.get('State') != 'stopped' or info.get('OS') != 'darwin':
            raise RuntimeError('Preparation comparison requires a stopped macOS base')
        hardware = self.get(sources[0])
        for i in range(rounds):
            label = f'comparison-{i + 1}'
            # Test both orderings and an isolated restore after the pair tests.
            cases.append(self.check(sources, label + '-existing-forward'))
            cases.append(self.check(list(reversed(sources)), label + '-existing-reverse'))
            cases.append(self.check([sources[0]], label + '-existing-alone-after'))
            for mode in ('sequential', 'concurrent', 'unique-serial'):
                scenario = label + '-' + mode
                prepared = self.prepare(base, scenario, hardware,
                                        concurrent=mode == 'concurrent',
                                        random_serial=mode == 'unique-serial')
                cases.append(self.check(prepared, scenario + '-forward'))
                cases.append(self.check(list(reversed(prepared)), scenario + '-reverse'))
                for name in prepared:
                    self.remove(name)

    def remove(self, name):
        process, log = self.clones[name]
        if process is not None:
            if process.poll() is None:
                self.command('stop', name)
            process.wait(timeout=30)
        if log is not None:
            log.close()
        self.command('delete', name)
        del self.clones[name]


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('template1')
    parser.add_argument('template2')
    parser.add_argument('--rounds', type=int)
    parser.add_argument('--compare-preparation', metavar='STOPPED_BASE',
                        help='Compare sequential/concurrent snapshot preparation and unique machine identities, using disposable clones of an existing stopped base; no downloads')
    args = parser.parse_args()
    if args.rounds is None:
        args.rounds = 1 if args.compare_preparation else 2
    if args.rounds < 1:
        parser.error('--rounds must be positive')
    os.umask(0o077)
    stamp = datetime.datetime.now(datetime.timezone.utc).strftime('%Y%m%dT%H%M%SZ')
    output = Path('test-results').resolve() / f'{stamp}-restore-probe-{os.getpid()}'
    output.mkdir(parents=True)
    (output / 'probe-tart-restore.py').write_bytes(Path(__file__).read_bytes())
    home = Path(os.environ.get('PULSE_TEST_TART_HOME') or os.environ.get('TART_HOME') or Path.home() / '.tart')
    probe = Probe(output, home)
    summary = {'result': 'failed', 'templates': [args.template1, args.template2],
               'platform': platform.platform(), 'rounds': args.rounds, 'errors': [],
               'comparison_base': args.compare_preparation, 'cases': []}
    print(f'Results: {output}', flush=True)
    try:
        if platform.system() != 'Darwin' or platform.machine() != 'arm64':
            raise RuntimeError('Requires an Apple Silicon Mac')
        if not home.is_absolute():
            raise RuntimeError('Tart home must be absolute')
        probe.command('--version')
        probe.preflight(summary['templates'])
        if args.compare_preparation:
            probe.compare_preparation(summary['templates'], args.compare_preparation,
                                      args.rounds, summary['cases'])
        else:
            for i in range(args.rounds):
                prefix = f'round-{i + 1}'
                solo = probe.restore(args.template1, prefix + '-alone')
                probe.remove(solo)
                second = probe.restore(args.template2, prefix + '-coordinator')
                first = probe.restore(args.template1, prefix + '-worker')
                # Also ensure the first VM stayed alive during the second restore.
                if probe.clones[second][0].poll() is not None:
                    raise RuntimeError('Coordinator exited during worker restore')
                probe.remove(first)
                probe.remove(second)
        summary['result'] = 'passed' if all(case['result'] == 'passed' for case in summary['cases']) else 'failed'
    except (Exception, KeyboardInterrupt) as error:
        summary['errors'].append(str(error) or 'Interrupted')
    finally:
        for name in list(probe.clones):
            try:
                probe.remove(name)
            except Exception as error:
                summary['result'] = 'failed'
                summary['errors'].append(f'Cleanup {name}: {error}')
        (output / 'summary.json').write_text(json.dumps(summary, indent=2) + '\n')
        with tarfile.open(str(output) + '.tar.gz', 'w:gz') as archive:
            archive.add(output, arcname=output.name)
        print(json.dumps(summary, indent=2))
        print(f'Send: {output}.tar.gz')
    return 0 if summary['result'] == 'passed' else 1


if __name__ == '__main__':
    raise SystemExit(main())
