"""Local builds retain a stable signing identity without selecting an arbitrary team."""
import json
import os
from pathlib import Path
import subprocess
import tempfile
import unittest

ROOT = Path(__file__).resolve().parent.parent


class MacSigning(unittest.TestCase):
    def invoke(self, identities='', explicit=None):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            calls = root / 'calls.jsonl'
            for name, body in {
                'security': 'printf "%s\\n" "$TEST_IDENTITIES"',
                'codesign': '''exec python3 - "$@" <<'PY'
import json, os, sys
with open(os.environ['TEST_CALLS'], 'a') as f:
    f.write(json.dumps(sys.argv[1:])+'\\n')
PY''',
            }.items():
                tool = root / name
                tool.write_text('#!/bin/bash\n' + body + '\n')
                tool.chmod(0o755)
            env = dict(os.environ, PATH=str(root)+os.pathsep+os.environ['PATH'],
                       TEST_IDENTITIES=identities, TEST_CALLS=str(calls))
            env.pop('PULSE_CODESIGN_IDENTITY', None)
            if explicit is not None:
                env['PULSE_CODESIGN_IDENTITY'] = explicit
            result = subprocess.run(['bash', str(ROOT/'scripts/sign-macos.sh'),
                                     str(root/'pulse-vz'), 'dev.pulse.vz.runner'],
                                    env=env, capture_output=True, text=True)
            return result, [json.loads(line) for line in calls.read_text().splitlines()] if calls.exists() else []

    def test_apple_identity_and_identifier_are_stable(self):
        result, calls = self.invoke('1) DIST "Developer ID Application: Team"\n2) DEV "Apple Development: Person"')
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(calls[0][calls[0].index('-s')+1], 'DIST')
        self.assertEqual(calls[0][calls[0].index('--identifier')+1], 'dev.pulse.vz.runner')
        self.assertIn('--timestamp=none', calls[0])
        self.assertEqual(calls[1][:2], ['--verify', '--strict'])

    def test_development_identity_fallback(self):
        result, calls = self.invoke('1) DEV "Apple Development: Person"')
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(calls[0][calls[0].index('-s')+1], 'DEV')

    def test_multiple_distribution_identities_require_selection(self):
        result, calls = self.invoke('1) A "Developer ID Application: One"\n2) B "Developer ID Application: Two"')
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual(calls, [])
        self.assertIn('PULSE_CODESIGN_IDENTITY', result.stderr)

    def test_explicit_identity_and_adhoc_override(self):
        for identity in ('Chosen Team', '-'):
            with self.subTest(identity=identity):
                result, calls = self.invoke(explicit=identity)
                self.assertEqual(result.returncode, 0, result.stderr)
                self.assertEqual(calls[0][calls[0].index('-s')+1], identity)

    def test_no_certificate_reports_adhoc_limit(self):
        result, calls = self.invoke()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(calls[0][calls[0].index('-s')+1], '-')
        self.assertIn('consent may reset', result.stderr)


if __name__ == '__main__':
    unittest.main()
