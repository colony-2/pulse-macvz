import importlib.util
import ipaddress
import json
from pathlib import Path
import tempfile
import unittest
from unittest import mock

spec = importlib.util.spec_from_file_location('network', Path(__file__).with_name('linux-kvm-network.py'))
n = importlib.util.module_from_spec(spec)
spec.loader.exec_module(n)


class NetworkTests(unittest.TestCase):
    def test_collision_checks_include_addresses_and_other_route_tables(self):
        occupied = n.occupied_networks(
            [{'dst': 'default'}, {'dst': '10.200.0.0/15', 'dev': 'eth1', 'table': 200},
             {'dst': '10.220.0.0/16', 'dev': n.BRIDGE}],
            [{'ifname': 'eth2', 'addr_info': [{'family': 'inet', 'local': '10.202.4.2', 'prefixlen': 24}]}])
        self.assertEqual(str(n.choose_subnet(occupied)), '10.203.0.0/16')
        with self.assertRaises(RuntimeError):
            n.choose_subnet(occupied, '10.200.0.0/16')
        self.assertEqual(str(n.choose_subnet(occupied, '10.220.0.0/16')), '10.220.0.0/16')

    def test_subnet_exhaustion_fails_without_reusing_a_conflicting_subnet(self):
        with self.assertRaises(RuntimeError):
            n.choose_subnet([ipaddress.ip_network('10.0.0.0/8')])

    def test_rejects_unowned_bridge_before_changing_network(self):
        with tempfile.TemporaryDirectory() as directory:
            with mock.patch.object(n, 'READY', Path(directory)/'ready'), mock.patch.object(n.os, 'geteuid', return_value=0), mock.patch.object(n, 'run', return_value=json.dumps([{'ifname': n.BRIDGE, 'ifalias': 'somebody-else'}])) as run:
                with self.assertRaisesRegex(RuntimeError, 'not managed'):
                    n.configure()
                self.assertEqual(run.call_args_list, [mock.call('ip', '-j', 'link', 'show')])

    def test_interrupted_creation_retains_intent_and_can_retry(self):
        with tempfile.TemporaryDirectory() as directory:
            config=Path(directory)/'network.json'
            calls=[]
            fail=True
            def command(*args):
                nonlocal fail
                calls.append(args)
                if args[:3] == ('ip', 'link', 'add'):
                    self.assertTrue(config.exists(), 'intent must precede resource creation')
                    self.assertIn(json.loads(config.read_text())['bridge_mac'], args)
                    if fail:
                        fail=False
                        raise RuntimeError('interrupted netlink creation')
                return '[]'
            with mock.patch.object(n, 'CONFIG', config), mock.patch.object(n, 'READY', Path(directory)/'ready'), mock.patch.object(n.os, 'geteuid', return_value=0), mock.patch.object(n, 'boot_id', return_value='boot'), mock.patch.object(n, 'run', side_effect=command), mock.patch.object(n, 'ensure_rule'):
                with self.assertRaisesRegex(RuntimeError, 'interrupted'):
                    n.configure()
                saved=config.read_text()
                n.configure()
                self.assertEqual(saved, config.read_text())

    def test_recovers_interruption_before_alias_only_with_matching_intent(self):
        with tempfile.TemporaryDirectory() as directory:
            config=Path(directory)/'network.json'
            config.write_text(json.dumps(dict(version=1, bridge=n.BRIDGE, bridge_mac='02:00:00:00:00:01', boot_id='boot', subnet='10.200.0.0/16')))
            link=dict(ifname=n.BRIDGE, address='02:00:00:00:00:01')
            def command(*args):
                if args == ('ip','-j','link','show'): return json.dumps([link])
                return '[]'
            with mock.patch.object(n,'CONFIG',config), mock.patch.object(n,'READY',Path(directory)/'ready'), mock.patch.object(n.os,'geteuid',return_value=0), mock.patch.object(n,'boot_id',return_value='boot'), mock.patch.object(n,'run',side_effect=command) as run, mock.patch.object(n,'ensure_rule'):
                n.configure()
                run.assert_any_call('ip','link','set',n.BRIDGE,'alias',n.ALIAS)
                self.assertFalse(any(c.args[:3]==('ip','link','add') for c in run.call_args_list))
                link['address']='02:00:00:00:00:02'
                with self.assertRaisesRegex(RuntimeError,'not managed'):
                    n.configure()

    def test_firewall_only_repositions_exact_owned_rules(self):
        with mock.patch.object(n.subprocess, 'run', return_value=mock.Mock(returncode=0)), mock.patch.object(n, 'run') as run:
            n.ensure_rule('filter', 'FORWARD', '-i', n.BRIDGE, '-j', 'ACCEPT')
            commands = [list(c.args) for c in run.call_args_list]
            self.assertEqual([c[5] for c in commands], ['-D', '-I'])
            for command in commands:
                self.assertIn('pulse-macvz-kvm', command)
                self.assertNotIn('-F', command)
                self.assertNotIn('-P', command)

    def test_readiness_rejects_previous_boot_even_when_version_matches(self):
        with tempfile.TemporaryDirectory() as directory:
            conf=Path(directory)/'network.json'
            conf.write_text(json.dumps(dict(version=n.VERSION, boot_id='old')))
            with mock.patch.object(n, 'CONFIG', conf), mock.patch.object(n.os, 'getuid', return_value=55050), mock.patch.object(n.os, 'getgid', return_value=55050), mock.patch.object(n, 'boot_id', return_value='new'):
                with self.assertRaisesRegex(AssertionError, 'stale network'):
                    n.probe()

    def test_atomic_descriptor_is_readable_by_job_and_no_temp_files_remain(self):
        with tempfile.TemporaryDirectory() as directory:
            target=Path(directory)/'network.json'
            n.write_json(target, dict(version=1))
            n.write_json(target, dict(version=2))
            self.assertEqual(json.loads(target.read_text()), dict(version=2))
            self.assertEqual(target.stat().st_mode & 0o777, 0o644)
            self.assertEqual(list(Path(directory).iterdir()), [target])


if __name__ == '__main__':
    unittest.main()
