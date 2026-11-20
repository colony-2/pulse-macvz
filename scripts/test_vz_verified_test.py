"""A skipped/failed network gate must never launch the expensive Docker suite."""
import importlib.util
import json
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

spec = importlib.util.spec_from_file_location('verified', Path(__file__).with_name('test-vz-docker-verified.py'))
verified = importlib.util.module_from_spec(spec)
spec.loader.exec_module(verified)


class Verification(unittest.TestCase):
    def test_gate_requires_execution_and_marker(self):
        name, marker = 'TestNetwork', 'NETWORK_PASS:'
        events = [{'Action': 'output', 'Test': name, 'Output': marker}, {'Action': 'pass', 'Test': name}]
        self.assertTrue(verified.gate_passed(events, name, marker))
        self.assertFalse(verified.gate_passed(events[:1], name, marker))
        self.assertFalse(verified.gate_passed(events[1:], name, marker))
        self.assertFalse(verified.gate_passed([{'Action': 'skip', 'Test': name}], name, marker))
        self.assertFalse(verified.gate_passed(events+[{'Action': 'fail'}], name, marker))

    def test_cold_linux_source_selection(self):
        with tempfile.TemporaryDirectory() as tmp:
            cache=Path(tmp); entry=cache/'entry'; entry.mkdir()
            names=['pulse-mac','pulse-snapshot','pulse-linux']
            (entry/'sources.json').write_text(json.dumps(names))
            for name in names:
                d=entry/name;d.mkdir()
                (d/'config.json').write_text(json.dumps({'os':'darwin' if name=='pulse-mac' else 'linux','arch':'arm64'}))
            (entry/'pulse-snapshot/state.vzvmsave').write_bytes(b'snapshot')
            self.assertEqual(verified.select_linux_base(cache), (entry/'pulse-linux').resolve())

    def test_launchd_failure_stops_before_vm_and_docker(self):
        with tempfile.TemporaryDirectory() as tmp:
            root=Path(tmp); cache=root/'.local/vz-qualification-cache';cache.mkdir(parents=True)
            called=[]
            def run(args, **kwargs):
                called.append(args)
            with patch.object(verified, 'ROOT', root), patch.object(verified.platform, 'system', return_value='Darwin'), \
                 patch.object(verified.platform, 'machine', return_value='arm64'), \
                 patch.object(verified.sys, 'argv', ['verify', '/mac-base']), \
                 patch.dict(verified.os.environ, {'PULSE_VZ_TEST_CACHE':str(cache)}), \
                 patch.object(verified, 'select_linux_base', return_value=root/'linux'), \
                 patch.object(verified, 'identity', return_value={'uuid':'one','requirement':'certificate'}), \
                 patch.object(verified, 'registration_evidence'), \
                 patch.object(verified.subprocess, 'run', side_effect=run), \
                 patch.object(verified.subprocess, 'call') as full, \
                 patch.object(verified, 'go_gate', side_effect=RuntimeError('launchd denied')):
                self.assertEqual(verified.main(), 2)
                full.assert_not_called()
            self.assertFalse(any('start' in args for args in called))
            report=next((root/'test-results').glob('*/verification.json'))
            self.assertEqual(json.loads(report.read_text())['result'], 'incomplete')
            self.assertIn('launchd denied', json.loads(report.read_text())['error'])
            self.assertEqual(json.loads(report.read_text())['app'], str(root/'.local/vz-verification/PulseProvider.app'))
            self.assertFalse(any('-u' in args for args in called))
            self.assertTrue(Path(str(report.parent)+'.tar.gz').exists())

    def test_late_approval_cannot_mask_rebuild_failure(self):
        with tempfile.TemporaryDirectory() as tmp:
            stage=Path(tmp); report=stage/'endpoint-preflight.json'
            for payload in ({}, {'service': {}}, {'service': {'service.log': 'bridge forwarding failed: no route to host'}}):
                report.write_text(json.dumps(payload))
                with self.assertRaises(RuntimeError):
                    verified.require_no_forwarding_denials(stage)
            report.write_text(json.dumps({'service': {'service.log': ''}}))
            verified.require_no_forwarding_denials(stage)

    def test_conflicting_registered_copy_fails_before_network(self):
        with tempfile.TemporaryDirectory() as tmp:
            app=Path(tmp)/'App.app'; output=Path(tmp)/'registration.json'
            for paths, allowed in (([str(app)], True), ([], False), ([str(app), '/other/App.app'], False)):
                result=verified.subprocess.CompletedProcess([], 0, json.dumps(paths), '')
                with patch.object(verified.subprocess, 'run', return_value=result):
                    if allowed:
                        verified.registration_evidence(app, output)
                    else:
                        with self.assertRaises(RuntimeError):
                            verified.registration_evidence(app, output)
                self.assertTrue(output.exists())


if __name__ == '__main__':
    unittest.main()
