"""Check worker Nix access setup for fresh bases and cached Linux clones."""
import json
import os
from pathlib import Path
import subprocess
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
elif tool == 'sudo':
    assert args == ['-n', '-H', '-u', 'pulseworker', '/usr/bin/nix',
                    '--extra-experimental-features', 'nix-command', 'store', 'ping', '--store', 'daemon'], args
    if os.environ.get('DENY_SOCKET') == '1' or (os.environ.get('RESTRICTED_SOCKET') == '1' and 'nix-users' not in groups):
        print('getting status of /nix/var/nix/daemon-socket/socket: Permission denied', file=sys.stderr)
        sys.exit(1)
''')
        mock.chmod(0o755)
        for name in ['getent', 'usermod', 'sudo']:
            (binary / name).symlink_to(mock)
        self.env = dict(os.environ, PATH=str(binary) + os.pathsep + os.environ['PATH'],
                        GROUPS_FILE=str(self.state), RESTRICTED_SOCKET='1')

    def setup_code(self, script):
        lines = (ROOT / 'scripts' / script).read_text().splitlines()
        start = next(i for i, line in enumerate(lines) if line.strip().startswith('if getent group nix-users'))
        # Run the actual setup and its probe, without provisioning the host.
        return '\n'.join(lines[start:start + 2])

    def run_code(self, code):
        return subprocess.run(['bash', '-eu', '-c', code], env=self.env,
                              text=True, capture_output=True, timeout=10)

    def test_fresh_and_cached_base_membership_is_additive_and_repeatable(self):
        for script in ['prepare-base-linux.sh', 'docker-primary.sh']:
            with self.subTest(script=script):
                self.state.write_text(json.dumps(['pulseworker', 'docker']))
                code = self.setup_code(script)
                before = self.run_code(code.splitlines()[-1])
                self.assertNotEqual(before.returncode, 0)
                self.assertIn('Permission denied', before.stderr)
                for _ in range(2):
                    result = self.run_code(code)
                    self.assertEqual(result.returncode, 0, result.stderr)
                self.assertEqual(set(json.loads(self.state.read_text())), {'pulseworker', 'docker', 'nix-users'})

    def test_open_daemon_does_not_require_a_distribution_specific_group(self):
        self.env['RESTRICTED_SOCKET'] = '0'
        for script in ['prepare-base-linux.sh', 'docker-primary.sh']:
            result = self.run_code(self.setup_code(script))
            self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(json.loads(self.state.read_text()), ['pulseworker', 'docker'])

    def test_inaccessible_daemon_fails_preparation_even_after_membership_repair(self):
        self.env['DENY_SOCKET'] = '1'
        for script in ['prepare-base-linux.sh', 'docker-primary.sh']:
            result = self.run_code(self.setup_code(script))
            self.assertNotEqual(result.returncode, 0)
            self.assertIn('Permission denied', result.stderr)


if __name__ == '__main__':
    unittest.main()
