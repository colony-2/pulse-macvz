"""Check worker Nix access setup for fresh bases and cached Linux clones."""
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest

ROOT = Path(__file__).resolve().parents[1]


class LinuxNixAccess(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.root = Path(self.tmp.name)
        self.state = self.root / 'groups.json'
        self.state.write_text(json.dumps(['pulseworker', 'docker']))
        binary = self.root / 'bin'
        binary.mkdir()
        mock = binary / 'mock'
        mock.write_text('''#!/usr/bin/env python3
import json, os, pathlib, sys
tool, args = pathlib.Path(sys.argv[0]).name, sys.argv[1:]
state = pathlib.Path(os.environ['GROUPS_FILE'])
groups = json.loads(state.read_text())
if tool == 'getent':
    assert args == ['group', 'nix-users'], args
    sys.exit(0 if os.environ.get('RESTRICTED_SOCKET') == '1' else 2)
elif tool == 'usermod':
    assert args[-1] == 'pulseworker', args
    group = args[-2]
    if '-aG' in args: groups = sorted(set(groups + [group]))
    else: groups = [group]
    state.write_text(json.dumps(groups))
elif tool == 'id':
    sys.exit(0 if os.environ.get('WORKER_EXISTS', '1') == '1' else 1)
elif tool == 'systemctl':
    assert args == ['restart', 'nix-daemon'], args
    sys.exit(1 if os.environ.get('FAIL_RESTART') == '1' else 0)
elif tool == 'sudo':
    assert args == ['-n', '-H', '-u', 'pulseworker', '/usr/bin/nix',
                    '--extra-experimental-features', 'nix-command', 'store', 'ping', '--store', 'daemon'], args
    if os.environ.get('DENY_SOCKET') == '1' or (os.environ.get('RESTRICTED_SOCKET') == '1' and 'nix-users' not in groups):
        print('getting status of /nix/var/nix/daemon-socket/socket: Permission denied', file=sys.stderr)
        sys.exit(1)
''')
        mock.chmod(0o755)
        for name in ['getent', 'usermod', 'sudo', 'id', 'systemctl']:
            (binary / name).symlink_to(mock)
        self.env = dict(os.environ, PATH=str(binary) + os.pathsep + os.environ['PATH'],
                        GROUPS_FILE=str(self.state), RESTRICTED_SOCKET='1')

    def setup_code(self):
        code = (ROOT / 'scripts/setup-linux-nix.sh').read_text()
        return (code.replace('[[ $EUID == 0 && $(uname -s) == Linux ]]', ':')
                .replace('/etc/nix', str(self.root / 'etc-nix'))
                .replace('/usr/bin/python3', sys.executable))

    def run_code(self, code):
        return subprocess.run(['bash', '-eu', '-c', code], env=self.env,
                              text=True, capture_output=True, timeout=10)

    def test_membership_is_additive_and_repeatable(self):
        before = self.run_code('sudo -n -H -u pulseworker /usr/bin/nix --extra-experimental-features nix-command store ping --store daemon')
        self.assertNotEqual(before.returncode, 0)
        self.assertIn('Permission denied', before.stderr)
        for _ in range(2):
            result = self.run_code(self.setup_code())
            self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(set(json.loads(self.state.read_text())), {'pulseworker', 'docker', 'nix-users'})

    def test_fresh_base_configures_cache_before_worker_creation(self):
        self.env.update(WORKER_EXISTS='0', DENY_SOCKET='1')
        result = self.run_code(self.setup_code())
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertTrue((self.root / 'etc-nix/pulse-macvz.conf').is_file())
        self.assertEqual(json.loads(self.state.read_text()), ['pulseworker', 'docker'])

    def test_restart_failure_is_retryable_after_config_was_written(self):
        self.env['FAIL_RESTART'] = '1'
        self.assertNotEqual(self.run_code(self.setup_code()).returncode, 0)
        config = self.root / 'etc-nix/nix.conf'
        before = config.read_bytes()
        self.env['FAIL_RESTART'] = '0'
        result = self.run_code(self.setup_code())
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(config.read_bytes(), before)
        self.assertEqual(before.count(b'include pulse-macvz.conf'), 1)

    def test_preserves_existing_settings_and_adds_one_include(self):
        directory = self.root / 'etc-nix'
        directory.mkdir()
        config = directory / 'nix.conf'
        original = b'# custom settings\nsubstituters = https://internal.invalid\ntrusted-public-keys = internal:abc=\nfilter-syscalls = true\nmax-jobs = 2'
        config.write_bytes(original)
        config.chmod(0o640)
        for _ in range(2):
            result = self.run_code(self.setup_code())
            self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(config.read_bytes(), original + b'\ninclude pulse-macvz.conf\n')
        self.assertEqual(config.stat().st_mode & 0o777, 0o640)
        managed = (directory / 'pulse-macvz.conf').read_text()
        self.assertIn('extra-substituters = https://colony2.cachix.org', managed)
        self.assertIn('extra-trusted-public-keys = colony2.cachix.org-1:', managed)
        self.assertNotIn('trusted-users', managed)
        self.assertNotIn('require-sigs', managed)
        self.assertNotIn('filter-syscalls', managed)

    def test_both_preparation_paths_use_shared_setup(self):
        for name in ['prepare-base-linux.sh', 'docker-primary.sh']:
            self.assertIn('/setup-linux-nix.sh', (ROOT / 'scripts' / name).read_text())

    def test_open_daemon_does_not_require_a_distribution_specific_group(self):
        self.env['RESTRICTED_SOCKET'] = '0'
        result = self.run_code(self.setup_code())
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(json.loads(self.state.read_text()), ['pulseworker', 'docker'])

    def test_inaccessible_daemon_fails_preparation_even_after_membership_repair(self):
        self.env['DENY_SOCKET'] = '1'
        result = self.run_code(self.setup_code())
        self.assertNotEqual(result.returncode, 0)
        self.assertIn('Permission denied', result.stderr)


if __name__ == '__main__':
    unittest.main()
