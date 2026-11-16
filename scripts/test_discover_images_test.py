import importlib.util
import json
from pathlib import Path
import subprocess
import sys
import unittest
from unittest.mock import patch

import test_update_images_test as updater

ROOT = Path(__file__).resolve().parent.parent
spec = importlib.util.spec_from_file_location('discovery', ROOT / 'scripts/discover-macos-images.py')
discovery = importlib.util.module_from_spec(spec)
spec.loader.exec_module(discovery)


class Discovery(unittest.TestCase):
    def setUp(self):
        updater.UpdateImages.setUp(self)

    def run_update(self, *args):
        return subprocess.run(['bash', str(ROOT / 'scripts/update-macos-images.sh'),
                               '--c2j', 'a' * 40, *args], env=self.env,
                              capture_output=True, text=True, timeout=60)

    def assert_passed(self, result):
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)

    def test_automatic_update_preserves_jobs_and_exact_existing_names(self):
        self.assert_passed(self.run_update())
        data = json.loads(self.state.read_text())
        self.assertEqual(set(data['vms']), set(self.before))
        self.assertEqual(data['vms']['old-job'], self.before['old-job'])
        for name in [*self.sources, self.orchard]:
            self.assertEqual(data['vms'][name]['Release'], 'new')

    def test_list_is_read_only_and_skips_jobs_linux_and_test_pools(self):
        data = json.loads(self.state.read_text())
        fixture = updater.names.names('test-1234567890abcdef', 'macos-26-arm64', 'standard', 4, 8192, 1)
        for name in fixture:
            data['vms'][name] = dict(data['vms'][self.sources[0]], Name=name)
        linux = 'pulse-test-unknown-linux-base'
        data['vms'][linux] = dict(Name=linux, OS='linux', State='stopped')
        staging = 'pulse-build-20261116T120000Z-123-base'
        data['vms'][staging] = dict(Name=staging, OS='darwin', State='running')
        before = data['vms']
        self.state.write_text(json.dumps(data))
        result = self.run_update('--list')
        self.assert_passed(result)
        self.assertIn(self.sources[1], result.stdout)
        for name in ['old-job', linux, staging, *fixture]:
            self.assertNotIn(name, result.stdout)
        data = json.loads(self.state.read_text())
        self.assertEqual(data['vms'], before)
        self.assertTrue(all(call[1] in ('list', 'get') for call in data['calls']))

    def test_explicit_owner_can_select_integration_pool(self):
        owner = 'test-1234567890abcdef'
        names = updater.names.names(owner, 'macos-26-arm64', 'standard', 4, 8192, 1)
        data = json.loads(self.state.read_text())
        for i, name in enumerate(names):
            data['vms'][name] = dict(data['vms'][self.sources[i]], Name=name)
        self.state.write_text(json.dumps(data))
        result = self.run_update('--discover', '--owner', owner)
        self.assert_passed(result)
        data = json.loads(self.state.read_text())
        for name in names:
            self.assertEqual(data['vms'][name]['Release'], 'new')
        for name in self.sources:
            self.assertEqual(data['vms'][name], self.before[name])

    def test_sparse_slots_and_alias_only_sources_do_not_create_missing_names(self):
        data = json.loads(self.state.read_text())
        del data['vms'][self.sources[1]]
        third = self.sources[2][:-4] + '0003'
        data['vms'][third] = dict(data['vms'].pop(self.sources[2]), Name=third)
        # Only slot 1's Orchard alias and canonical slot 3 remain.
        before = set(data['vms'])
        self.state.write_text(json.dumps(data))
        self.assert_passed(self.run_update())
        data = json.loads(self.state.read_text())
        self.assertEqual(set(data['vms']), before)
        self.assertEqual(data['vms'][third]['Release'], 'new')
        self.assertEqual(data['vms'][self.orchard]['Release'], 'new')

    def test_all_pools_preflight_before_any_updates(self):
        data = json.loads(self.state.read_text())
        data['vms'][self.sources[2]]['State'] = 'running'
        before = data['vms']
        self.state.write_text(json.dumps(data))
        result = self.run_update()
        self.assertNotEqual(result.returncode, 0)
        self.assertIn('not ready', result.stderr)
        data = json.loads(self.state.read_text())
        self.assertEqual(data['vms'], before)
        self.assertTrue(all(call[1] in ('list', 'get') for call in data['calls']))

    def test_vm_properties_exclude_jobs_even_with_template_shaped_name(self):
        path = Path(self.env['TART_HOME']) / 'vms' / self.sources[2] / 'pulse.json'
        path.parent.mkdir(parents=True)
        path.write_text(json.dumps({'version': 1, 'native': {'spec': {'name': self.sources[2]}},
                                    'properties': {'job': {'private': 'do-not-print'}}}))
        result = self.run_update('--list')
        self.assert_passed(result)
        self.assertNotIn(self.sources[2], result.stdout)
        self.assertNotIn('do-not-print', result.stdout + result.stderr)

    def test_no_sources_is_an_error_without_side_effects(self):
        self.state.write_text(json.dumps(dict(vms={}, calls=[], next=0)))
        result = self.run_update()
        self.assertNotEqual(result.returncode, 0)
        self.assertIn('No matching', result.stderr)

    def test_disappearing_source_is_not_recreated_during_publication(self):
        self.env['DELETE_ON_PREPARE'] = self.sources[2]
        result = self.run_update()
        self.assertNotEqual(result.returncode, 0)
        self.assertIn('changed or disappeared', result.stderr)
        data = json.loads(self.state.read_text())
        self.assertNotIn(self.sources[2], data['vms'])
        self.assertFalse(any(call[:3] == ['tart', 'clone', '--overwrite'] and call[-1] == self.sources[2]
                             for call in data['calls']))

    def test_unknown_macos_recipe_fails_before_mutation(self):
        data = json.loads(self.state.read_text())
        name = 'pulse-test-custom-base'
        data['vms'][name] = dict(data['vms'][self.sources[0]], Name=name)
        self.state.write_text(json.dumps(data))
        result = self.run_update()
        self.assertNotEqual(result.returncode, 0)
        self.assertIn('Cannot infer', result.stderr)

    def test_filters_and_main_resolution_once_for_multiple_pools(self):
        data = json.loads(self.state.read_text())
        other = updater.names.names('another', 'macos-15-arm64', 'mac-macos-15-arm64', 4, 8192, 1)
        for i, name in enumerate(other):
            data['vms'][name] = dict(data['vms'][self.sources[i]], Name=name)
        self.state.write_text(json.dumps(data))
        result = self.run_update('--list', '--base', 'macos-15-arm64')
        self.assert_passed(result)
        self.assertIn(other[1], result.stdout)
        self.assertNotIn(self.sources[1], result.stdout)
        result = self.run_update()
        self.assert_passed(result)
        self.assertIn('Found 2 template pool(s)', result.stdout)
        data = json.loads(self.state.read_text())
        for name in [*self.sources, *other]:
            self.assertEqual(data['vms'][name]['Release'], 'new')
        # Mock just the external release lookup and child executions to verify
        # main is resolved once and the exact revision reaches every pool.
        commands = []
        def run(command, **kwargs):
            commands.append(command)
            if command[1].endswith('select-c2j.sh'):
                self.assertEqual(command[3], 'main')
                (Path(command[2]) / 'flake.nix').write_text('inputs.c2j.url = "github:colony-2/c2j/' + 'b' * 40 + '";')
        def native(*args):
            return list(data['vms'].values()) if args[0] == 'list' else data['vms'][args[1]]
        with patch.object(sys, 'argv', ['discovery', '--c2j', 'main']), \
                patch.object(discovery, 'tart', side_effect=native), \
                patch.object(discovery.subprocess, 'run', side_effect=run):
            self.assertEqual(discovery.main(), 0)
        self.assertEqual(len(commands), 3)
        for command in commands[1:]:
            self.assertEqual(command[command.index('--c2j') + 1], 'b' * 40)

    def test_base_only_does_not_create_slots(self):
        data = json.loads(self.state.read_text())
        data['vms'] = {self.sources[0]: data['vms'][self.sources[0]]}
        self.state.write_text(json.dumps(data))
        self.assert_passed(self.run_update())
        data = json.loads(self.state.read_text())
        self.assertEqual(list(data['vms']), [self.sources[0]])
        self.assertEqual(data['vms'][self.sources[0]]['Release'], 'new')

    def test_discovered_docker_pool_keeps_profile(self):
        sources = updater.names.names('test', 'macos-26-arm64', 'standard', 4, 8192, 2, 'engine')
        inventory = [dict(Name=name) for name in sources]
        info = {name: dict(OS='darwin', State='stopped' if i == 0 else 'suspended', CPU=4, Memory=8192)
                for i, name in enumerate(sources)}
        pools = discovery.discover(inventory, json.loads((ROOT / 'bases.json').read_text()), info.__getitem__, self.root)
        self.assertEqual(len(pools), 1)
        self.assertEqual(pools[0]['profile'], 'engine')
        self.assertEqual(set(pools[0]['targets']), set(sources))

    def test_hashed_owner_and_group_names_round_trip(self):
        owner, group = 'long-owner-name-that-is-hashed', 'long-group-name-that-is-hashed'
        sources = updater.names.names(owner, 'macos-26-arm64', group, 8, 16384, 2)
        inventory = [dict(Name=name) for name in sources]
        info = {name: dict(OS='darwin', State='stopped' if i == 0 else 'suspended',
                           CPU=4 if i == 0 else 8, Memory=8192 if i == 0 else 16384)
                for i, name in enumerate(sources)}
        pools = discovery.discover(inventory, json.loads((ROOT / 'bases.json').read_text()),
                                   info.__getitem__, self.root, owner=owner, group=group)
        self.assertEqual(len(pools), 1)
        pool = pools[0]
        self.assertEqual(updater.names.names(pool['owner'], pool['base'], pool['group'],
                                            pool['cpu'], pool['memory'], pool['count']), sources)
