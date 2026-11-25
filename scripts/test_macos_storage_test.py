"""Exercise the builder's storage policy without touching any real disk."""
import os
from pathlib import Path
import subprocess
import tempfile
import unittest

ROOT = Path(__file__).resolve().parent.parent

class MacStorage(unittest.TestCase):
    def run_storage(self, *, free=40, model='VirtualMac2,1', busy=1,
                    grown=False, assigned=False, repair_fails=False, container='disk2'):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            for name in ['data', 'nix']:
                (root / name).mkdir()
            if assigned:
                (root / 'intent').touch()
            script = (ROOT / 'scripts/prepare-base.sh').read_text()
            script = script[script.index('macos_free_space()'):script.index('state=/var/db/pulse-tart-base')]
            for old, new in [('/System/Volumes/Data', str(root / 'data')), ('/nix/store', str(root / 'nix')),
                             ('/var/db/pulse-tart/intent', str(root / 'intent')),
                             ('/var/db/pulse-macvz/intent', str(root / 'new-intent')),
                             ('/usr/sbin/diskutil', 'diskutil_test'), ('/usr/bin/plutil', 'plutil_test'),
                             ('/usr/sbin/sysctl', 'sysctl_test'), ('/usr/bin/pgrep', 'pgrep_test'),
                             ('/bin/df', 'df_test')]:
                script = script.replace(old, new)
            prelude = '''
set -euo pipefail
sysctl_test() { echo "$MODEL"; }
pgrep_test() { return "$BUSY"; }
df_test() { printf 'Filesystem blocks used available\ndisk 80000000 1 %s\n' "$FREE"; }
diskutil_test() {
  echo "$*" >> "$CALLS"
  if [[ $1 == repairDisk ]]; then return "$REPAIR"; fi
  echo plist
}
plutil_test() {
  cat >/dev/null
  case "$2" in
    APFSContainerReference) echo "$CONTAINER";;
    APFSPhysicalStores.0.APFSPhysicalStore) echo disk0s2;;
    ParentWholeDisk) echo disk0;;
    APFSContainerSize) echo "$CURRENT";;
    TotalSize) echo 85899345920;;
    *) return 1;;
  esac
}
'''
            env = dict(os.environ, MODEL=model, BUSY=str(busy), FREE=str(free*1024*1024),
                       REPAIR=str(int(repair_fails)), CONTAINER=container,
                       CURRENT=str((80 if grown else 50)*2**30-524288000), CALLS=str(root/'calls'))
            result = subprocess.run(['bash', '-c', prelude+script+'\ngrow_macos_storage\nmacos_free_space\n'],
                                    env=env, capture_output=True, text=True, timeout=10)
            calls = (root/'calls').read_text() if (root/'calls').exists() else ''
            return result, calls

    def test_growth_then_free_space_check(self):
        result, calls = self.run_storage()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn('repairDisk disk0\napfs resizeContainer disk2 0', calls)
        self.assertIn('40960 MiB available', result.stdout)

    def test_already_grown_does_not_repair_again(self):
        result, calls = self.run_storage(grown=True)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertNotIn('repairDisk', calls)

    def test_low_free_space_fails_publication(self):
        result, _ = self.run_storage(free=29, grown=True)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn('at least 30 GiB', result.stderr)

    def test_exact_minimum(self):
        result, _ = self.run_storage(free=30, grown=True)
        self.assertEqual(result.returncode, 0, result.stderr)

    def test_refuse_host_assigned_and_busy_guests(self):
        for options in [dict(model='Mac16,5'), dict(busy=0), dict(assigned=True)]:
            result, calls = self.run_storage(**options)
            self.assertNotEqual(result.returncode, 0)
            self.assertNotIn('repairDisk', calls)

    def test_failed_repair_never_resizes(self):
        result, calls = self.run_storage(repair_fails=True)
        self.assertNotEqual(result.returncode, 0)
        self.assertNotIn('apfs resizeContainer', calls)

    def test_invalid_discovery_never_repairs(self):
        result, calls = self.run_storage(container='bad disk')
        self.assertNotEqual(result.returncode, 0)
        self.assertNotIn('repairDisk', calls)

if __name__ == '__main__':
    unittest.main()
