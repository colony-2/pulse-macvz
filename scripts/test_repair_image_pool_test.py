import fcntl
import importlib.util
import json
import os
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

spec = importlib.util.spec_from_file_location('pool_repair', Path(__file__).with_name('repair-image-pool.py'))
repair = importlib.util.module_from_spec(spec)
spec.loader.exec_module(repair)


class PoolRepair(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory()
        self.addCleanup(temporary.cleanup)
        self.home = Path(temporary.name).resolve()
        self.name = 'pulse-owner-macos-26-arm64-standard-4cpu-8192m-slot-0001'
        self.base = 'pulse-owner-macos-26-arm64-base'
        self.directory = self.home / 'vms' / self.name
        self.directory.mkdir(parents=True)
        info = self.directory.stat()
        self.scope = 'a' * 64
        self.data = dict(version=1, scope=self.scope,
                         native=dict(uid='builder-id', device=info.st_dev, inode=info.st_ino,
                                     mac='02:00:00:00:00:01', spec=dict(name=self.name, cpu=4, memory=8192)),
                         properties=dict(slot=dict(id='slot-1')))
        self.metadata = self.directory / 'pulse.json'
        self.metadata.write_text(json.dumps(self.data))
        (self.directory / '.pulse-owner').write_text('builder-id')
        (self.directory / 'config.json').write_text('{}')
        (self.directory / 'disk.img').write_bytes(b'original disk')
        self.plan_path = self.home / 'repair.json'
        self.plan_path.write_text(json.dumps(dict(home=str(self.home), owner='owner', base_name=self.base,
                                                  missing=[], builders={self.name: 'builder-id'})))
        self.update_lock = self.home / ('.' + self.base + '.update-lock')
        self.provider_lock = self.home / '.pulse' / self.scope / 'lock'
        self.provider_lock.parent.mkdir(parents=True)
        self.provider_lock.touch()
        self.enterContext(patch.dict(os.environ, TART_HOME=str(self.home),
                                    PULSE_LEGACY_PROVIDERS_DIR=str(self.home / 'legacy')))
        self.observe = self.enterContext(patch.object(repair, 'observe', return_value=dict(
            State='stopped', CPU=4, Memory=8192, MACAddress='02:00:00:00:00:01')))

    def test_existing_updater_refuses_before_moving_builder(self):
        self.update_lock.mkdir()
        with patch.object(repair.subprocess, 'run') as run:
            with self.assertRaises(FileExistsError):
                repair.repair(self.plan_path, ['updater'])
        run.assert_not_called()
        self.observe.assert_not_called()
        self.assertTrue(self.update_lock.is_dir())
        self.assertTrue(self.metadata.is_file())
        self.assertFalse((self.home / '.pulse-image-repairs').exists())

    def test_provider_lock_refuses_and_releases_only_own_update_lock(self):
        with self.provider_lock.open('r') as stream:
            fcntl.flock(stream, fcntl.LOCK_EX | fcntl.LOCK_NB)
            with self.assertRaisesRegex(ValueError, 'Pool is in use'):
                repair.repair(self.plan_path, ['updater'])
        self.assertFalse(self.update_lock.exists())
        self.assertTrue(self.metadata.exists())

    def test_backup_preserves_builder_and_lock_spans_child_updater(self):
        def update(command, check, env):
            self.assertEqual(command, ['updater'])
            self.assertTrue(check)
            self.assertEqual(env['PULSE_IMAGE_UPDATE_LOCK'], str(self.update_lock))
            self.assertTrue(self.update_lock.is_dir())
            self.assertFalse(self.directory.exists())
            with self.provider_lock.open('r') as stream:
                with self.assertRaises(BlockingIOError):
                    fcntl.flock(stream, fcntl.LOCK_EX | fcntl.LOCK_NB)
        with patch.object(repair.subprocess, 'run', side_effect=update) as run:
            repair.repair(self.plan_path, ['updater'])
        run.assert_called_once()
        backup = next((self.home / '.pulse-image-repairs').iterdir()) / self.name
        self.assertEqual((backup / 'disk.img').read_bytes(), b'original disk')
        self.assertEqual(json.loads((backup / 'pulse.json').read_text()), self.data)
        plan = json.loads(self.plan_path.read_text())
        self.assertEqual(plan['builders'], {})
        self.assertEqual(plan['missing'], [self.name])
        self.assertFalse(self.update_lock.exists())

    def test_job_target_refuses_without_moving_files(self):
        self.data['properties']['job'] = {'launch_id': 'existing-job'}
        self.metadata.write_text(json.dumps(self.data))
        with patch.object(repair.subprocess, 'run') as run:
            with self.assertRaisesRegex(ValueError, 'Refusing to repair a job'):
                repair.repair(self.plan_path, ['updater'])
        run.assert_not_called()
        self.assertTrue(self.metadata.exists())
        self.assertFalse(self.update_lock.exists())
