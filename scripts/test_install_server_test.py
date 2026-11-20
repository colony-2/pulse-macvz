"""Installation uses Go's destination and preserves a working build on failure."""
import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import unittest

ROOT = Path(__file__).resolve().parent.parent


class InstallServer(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.root = Path(self.tmp.name)
        self.scripts = self.root / 'scripts'
        self.scripts.mkdir()
        shutil.copy(ROOT / 'scripts/install-server.sh', self.scripts)
        self.tools = self.root / 'tools'
        self.tools.mkdir()
        self.env = dict(os.environ, PATH=str(self.tools) + os.pathsep + os.environ['PATH'],
                        TEST_GOBIN='', TEST_GOPATH=str(self.root / 'go path') + ':' + str(self.root / 'second'),
                        TEST_OS='Linux', TEST_ARCH='arm64', TEST_FAIL='0')
        self.tool('go', 'case "$*" in "env GOBIN") echo "$TEST_GOBIN";; "env GOPATH") echo "$TEST_GOPATH";; *) exit 90;; esac')
        self.tool('uname', 'case "$1" in -s) echo "$TEST_OS";; -m) echo "$TEST_ARCH";; esac')
        (self.scripts / 'build-server.sh').write_text('''#!/bin/bash
set -eu
test "$TEST_FAIL" = 0
printf '#!/bin/sh\necho installed\n' > "$1"
chmod +x "$1"
''')
        (self.scripts / 'build-server-app.sh').write_text('''#!/bin/bash
set -eu
test "$TEST_FAIL" = 0
mkdir -p "$1/Contents/MacOS"
bash "$(dirname "$0")/build-server.sh" "$1/Contents/MacOS/pulse-tart"
''')

    def tool(self, name, body):
        p = self.tools / name
        p.write_text('#!/bin/bash\nset -eu\n' + body + '\n')
        p.chmod(0o755)

    def install(self):
        return subprocess.run(['bash', str(self.scripts / 'install-server.sh')],
                              env=self.env, text=True, capture_output=True)

    def test_default_uses_first_gopath_entry(self):
        result = self.install()
        self.assertEqual(result.returncode, 0, result.stderr)
        target = self.root / 'go path/bin/pulse-tart'
        self.assertEqual(subprocess.check_output([target], text=True).strip(), 'installed')
        self.assertFalse((self.root / 'second/bin').exists())

    def test_macos_bundle_and_path_survive_failed_reinstall(self):
        dest = self.root / 'custom bin'
        self.env.update(TEST_OS='Darwin', TEST_GOBIN=str(dest))
        for _ in range(2):
            result = self.install()
            self.assertEqual(result.returncode, 0, result.stderr)
        target = dest / 'pulse-tart'
        self.assertTrue(target.is_symlink())
        self.assertEqual(target.resolve(), (dest / 'PulseProvider.app/Contents/MacOS/pulse-tart').resolve())
        self.env['TEST_FAIL'] = '1'
        self.assertNotEqual(self.install().returncode, 0)
        self.assertEqual(subprocess.check_output([target], text=True).strip(), 'installed')
        self.assertFalse(list(dest.glob('.pulse-install.*')))

    def test_failed_build_preserves_existing_executable(self):
        dest = self.root / 'custom'
        dest.mkdir()
        target = dest / 'pulse-tart'
        target.write_text('old executable')
        self.env.update(TEST_GOBIN=str(dest), TEST_FAIL='1')
        self.assertNotEqual(self.install().returncode, 0)
        self.assertEqual(target.read_text(), 'old executable')


if __name__ == '__main__':
    unittest.main()
