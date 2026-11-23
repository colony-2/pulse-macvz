import importlib.util
from pathlib import Path
import subprocess
import tempfile
import unittest
from unittest import mock

spec = importlib.util.spec_from_file_location('gic', Path(__file__).with_name('configure-linux-gic-traps.py'))
gic = importlib.util.module_from_spec(spec)
spec.loader.exec_module(gic)


class GICPreparationTests(unittest.TestCase):
    def test_preserves_settings_and_is_idempotent(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            other = root / '50-other.cfg'
            other.write_text('GRUB_CMDLINE_LINUX="quiet console=ttyAMA0"\n')
            gic.configure(root)
            before = (root / gic.NAME).stat().st_mtime_ns
            gic.configure(root)
            self.assertEqual((root / gic.NAME).stat().st_mtime_ns, before)
            self.assertEqual(other.read_text(), 'GRUB_CMDLINE_LINUX="quiet console=ttyAMA0"\n')
            result = subprocess.check_output(['bash', '-c', 'source "$1"; source "$2"; printf "%s" "$GRUB_CMDLINE_LINUX"', 'bash', str(other), str(root/gic.NAME)], text=True)
            self.assertEqual(result, 'quiet console=ttyAMA0 ' + ' '.join(gic.PARAMETERS))
            self.assertEqual((root / gic.NAME).stat().st_mode & 0o777, 0o644)
            self.assertEqual(len(list(root.iterdir())), 2)

    def test_refuses_symlink_without_modifying_target(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            other = root/'other'
            other.write_text('untouched')
            (root/gic.NAME).symlink_to(other)
            with self.assertRaises(ValueError):
                gic.configure(root)
            self.assertEqual(other.read_text(), 'untouched')

    @mock.patch.object(gic.os, 'geteuid', return_value=0)
    @mock.patch.object(gic.platform, 'system', return_value='Linux')
    @mock.patch.object(gic.platform, 'machine', return_value='aarch64')
    def test_refuses_delivered_job_and_retries_grub_after_interruption(self, *unused):
        with mock.patch.object(gic.Path, 'exists', return_value=True), mock.patch.object(gic, 'configure') as configure, mock.patch.object(gic.subprocess, 'run') as run:
            with self.assertRaisesRegex(SystemExit, 'job delivery'):
                gic.main()
            configure.assert_not_called()
            run.assert_not_called()
        with mock.patch.object(gic.Path, 'exists', return_value=False), mock.patch.object(gic, 'configure') as configure, mock.patch.object(gic.subprocess, 'run', side_effect=[subprocess.CalledProcessError(1, 'update-grub'), None]) as run:
            with self.assertRaises(subprocess.CalledProcessError):
                gic.main()
            gic.main()
            self.assertEqual(configure.call_count, 2)
            self.assertEqual(run.call_args_list, [mock.call(['update-grub'], check=True)] * 2)


if __name__ == '__main__':
    unittest.main()
