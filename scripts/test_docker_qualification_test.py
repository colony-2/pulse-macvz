"""Qualification cannot pass on markers, missing stages, or incomplete JobDB evidence."""
import copy
import importlib.util
import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
from tempfile import TemporaryDirectory
import unittest

ROOT = Path(__file__).resolve().parent.parent
spec = importlib.util.spec_from_file_location('qualification_report', ROOT / 'scripts/docker-qualification/report.py')
report = importlib.util.module_from_spec(spec)
spec.loader.exec_module(report)


class QualificationReport(unittest.TestCase):
    def setUp(self):
        self.tmp = TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.root = Path(self.tmp.name)
        self.evidence = {'backend': 'vz', 'checks': dict.fromkeys(report.CHECKS, True), 'peak_running_macos': 2,
                         'identities': [{key: f'{key}-{i}' for key in
                             ('primary_uid', 'engine_id', 'client_key_sha256', 'host_key_sha256')} for i in range(6)]}
        self.events = [{'Action': 'pass', 'Package': 'github.com/colony-2/pulse-macvz', 'Test': 'TestLiveDockerQualification'}]
        for i, identity in enumerate(self.evidence['identities']):
            identity['mac'] = '00:11:22:33:44:0' + str(i % 2)
        self.jobs = [{'status': 'COMPLETED', 'output_ok': True, 'claims': 1, 'renewals': 3, 'secret_calls': 1} for _ in range(4)]

    def result(self, exit_code=0, backend="vz"):
        (self.root / 'qualification.json').write_text(json.dumps(self.evidence))
        (self.root / 'tests.jsonl').write_text('\n'.join(map(json.dumps, self.events)))
        for p in self.root.glob('jobdb-*.json'):
            p.unlink()
        for i, j in enumerate(self.jobs):
            (self.root / f'jobdb-{i}.json').write_text(json.dumps(j))
        return report.summarize(self.root, exit_code, backend)['result']


    def test_vz_requires_native_fault_evidence_and_matching_backend(self):
        self.evidence.pop('backend')
        self.assertEqual(self.result(), 'incomplete')
        self.evidence['backend'] = 'vz'
        self.assertEqual(self.result(), 'passed')
        with self.assertRaises(ValueError):
            self.result(backend='removed-backend')
        for key in report.CHECKS:
            self.evidence['checks'][key] = False
            self.assertEqual(self.result(), 'incomplete')
            self.evidence['checks'][key] = True

    def test_complete_evidence(self):
        self.assertEqual(self.result(), 'passed')
        self.assertEqual(self.result(1), 'incomplete')

    def test_every_fault_pressure_and_isolation_stage_is_required(self):
        for key in report.CHECKS:
            with self.subTest(stage=key):
                self.evidence['checks'][key] = False
                self.assertEqual(self.result(), 'incomplete')
                self.evidence['checks'][key] = True

    def test_skip_cleanup_failure_and_marker_only_cannot_pass(self):
        for event in ({'Action': 'skip', 'Test': 'TestLiveDockerQualification'},
                      {'Action': 'output', 'Output': 'DOCKER_QUALIFICATION_PASS:'}):
            self.events = [event]
            self.assertEqual(self.result(), 'incomplete')
        self.events = [{'Action': 'pass', 'Package': 'github.com/colony-2/pulse-macvz', 'Test': 'TestLiveDockerQualification'},
                       {'Action': 'fail', 'Test': 'cleanup'}]
        self.assertEqual(self.result(), 'incomplete')

    def test_duplicate_keys_or_missing_generations_cannot_pass(self):
        original = copy.deepcopy(self.evidence['identities'])
        for key in ('primary_uid', 'engine_id', 'client_key_sha256', 'host_key_sha256'):
            self.evidence['identities'] = copy.deepcopy(original)
            self.evidence['identities'][5][key] = original[0][key]
            self.assertEqual(self.result(), 'incomplete')
        self.evidence['identities'] = original[:4]
        self.assertEqual(self.result(), 'incomplete')

    def test_concurrent_mac_collision_cannot_pass(self):
        self.evidence['identities'][1]['mac'] = self.evidence['identities'][0]['mac']
        self.assertEqual(self.result(), 'incomplete')

    def test_jobdb_and_vm_limit_are_required(self):
        for key, value in (('claims', 2), ('renewals', 0), ('secret_calls', 0), ('output_ok', False), ('status', 'FAILED')):
            old = self.jobs[0][key]
            self.jobs[0][key] = value
            self.assertEqual(self.result(), 'incomplete')
            self.jobs[0][key] = old
        for peak in (0, 3):
            self.evidence['peak_running_macos'] = peak
            self.assertEqual(self.result(), 'incomplete')
        self.evidence['peak_running_macos'] = 2
        self.jobs.pop()
        self.assertEqual(self.result(), 'incomplete')

    def test_early_failure_with_null_identities_keeps_stage_report(self):
        self.evidence.update(backend='vz', checks={}, identities=None)
        self.jobs = []
        self.assertEqual(self.result(exit_code=1, backend='vz'), 'incomplete')
        result = report.summarize(self.root, 1, 'vz')
        self.assertEqual(result['scope'], 'vz-production-docker-qualification')
        self.assertFalse(any(result['checks'].values()))


if __name__ == '__main__':
    unittest.main()
