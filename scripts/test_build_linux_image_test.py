"""Exercise native architecture selection and failure handling without a Mac."""
import hashlib
import json
import os
from pathlib import Path
import subprocess
import tempfile
import unittest

ROOT = Path(__file__).resolve().parents[1]


class LinuxImageBuild(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.root = Path(self.tmp.name)
        binary = self.root / 'bin'
        binary.mkdir()
        mock = binary / 'mock'
        mock.write_text('''#!/usr/bin/env python3
import json, os, pathlib, shutil, sys
tool, args = pathlib.Path(sys.argv[0]).name, sys.argv[1:]
with open(os.environ['CALLS'], 'a') as f: f.write(json.dumps([tool, *args]) + '\\n')
if tool == 'uname': print('Darwin' if args == ['-s'] else os.environ['HOST_ARCH'])
elif tool == 'git' and 'checkout' in args:
    root = pathlib.Path(args[1]); (root / 'cloud-init').mkdir()
    (root / 'cloud-init/user-data.distro-with-admin-group').write_text('users: []')
    (root / 'playbook.yml').write_text('- hosts: default\\n  tasks: []\\n')
    (root / 'cloud-init.pkr.hcl').write_text('headless = false\\nplaybook_file = "./playbook.yml"\\n')
elif tool == 'qemu-img': shutil.copyfile(args[-2], args[-1])
elif tool == 'mkisofs': pathlib.Path(args[1]).write_text('iso')
elif tool == 'tart':
    if args[0] == 'get': sys.exit(0 if os.environ.get('EXISTING') else 1)
    if args[0] == 'create':
        vm = pathlib.Path(os.environ['TART_HOME']) / 'vms' / args[-1]
        vm.mkdir(parents=True); (vm / 'config.json').write_text('{}')
elif tool == 'packer' and args[0] == 'build':
    sys.exit(int(os.environ.get('BUILD_EXIT', '0')))
''')
        mock.chmod(0o755)
        for name in ['uname', 'git', 'qemu-img', 'mkisofs', 'tart', 'packer', 'ansible-playbook']:
            (binary / name).symlink_to(mock)
        self.disk = self.root / 'cloud disk.img'
        self.disk.write_bytes(b'test cloud image')
        self.sha = hashlib.sha256(self.disk.read_bytes()).hexdigest()
        self.calls = self.root / 'calls.jsonl'
        self.env = dict(os.environ, PATH=str(binary) + os.pathsep + os.environ['PATH'],
                        TMPDIR=str(self.root), TART_HOME=str(self.root / 'tart'),
                        CALLS=str(self.calls), HOST_ARCH='arm64')

    def run_build(self, arch='arm64', checksum=None):
        return subprocess.run(['bash', str(ROOT / 'scripts/build-linux-image.sh'), arch,
                               'test-vm', str(self.disk), checksum or self.sha],
                              env=self.env, text=True, capture_output=True, timeout=15)

    def recorded(self):
        return [json.loads(line) for line in self.calls.read_text().splitlines()]

    def test_both_native_architectures(self):
        for arch, host, guest in [('arm64', 'arm64', 'aarch64'), ('amd64', 'x86_64', 'x86_64')]:
            with self.subTest(arch=arch):
                self.env['HOST_ARCH'] = host
                self.env['TART_HOME'] = str(self.root / arch)
                result = self.run_build(arch)
                self.assertEqual(result.returncode, 0, result.stderr)
                build = Path(next(line.split(': ', 1)[1] for line in result.stdout.splitlines()
                                  if line.startswith('Build files and logs: ')))
                self.assertIn("ansible_architecture == '" + guest + "'", (build / 'verify.yml').read_text())
                self.assertIn('Arch=' + arch, result.stdout)
                self.assertTrue((Path(self.env['TART_HOME']) / 'vms/test-vm/disk.img').exists())

    def test_wrong_host_rejected_before_vm_creation(self):
        result = self.run_build('amd64')
        self.assertNotEqual(result.returncode, 0)
        self.assertFalse(any(call[0] == 'tart' for call in self.recorded()))

    def test_checksum_rejected_before_vm_creation(self):
        result = self.run_build(checksum='0' * 64)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn('SHA256 mismatch', result.stderr)
        self.assertFalse(any(call[0] == 'tart' for call in self.recorded()))

    def test_existing_vm_is_not_replaced(self):
        self.env['EXISTING'] = '1'
        result = self.run_build()
        self.assertNotEqual(result.returncode, 0)
        self.assertFalse(any(call[:2] == ['tart', 'create'] for call in self.recorded()))

    def test_failed_build_is_not_reported_as_success(self):
        self.env['BUILD_EXIT'] = '17'
        result = self.run_build()
        self.assertEqual(result.returncode, 17)
        self.assertNotIn('Built local', result.stdout)
        self.assertTrue((Path(self.env['TART_HOME']) / 'vms/test-vm/disk.img').exists())


if __name__ == '__main__':
    unittest.main()
