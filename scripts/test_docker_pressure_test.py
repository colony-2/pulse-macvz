"""Pressure failures retain measurements and fail even when cleanup succeeds."""
import os
import shlex
from pathlib import Path
import subprocess
from tempfile import TemporaryDirectory
import unittest

ROOT = Path(__file__).resolve().parent.parent
CID = 'a' * 64
KERNEL_OOM = ('oom-kill:constraint=CONSTRAINT_MEMCG,nodemask=(null),'
              'oom_memcg=/system.slice/docker-' + CID + '.scope,task=python,pid=123')


class PressureDiagnostics(unittest.TestCase):
    def test_assertions_and_cleanup(self):
        with TemporaryDirectory() as tmp:
            docker = Path(tmp) / 'docker'
            docker.write_text('''#!/bin/bash
case "$1" in
  inspect)
    case "$*" in
      *NanoCpus*) echo "${TEST_QUOTA:-3500000000}";;
      *OOMKilled*) echo "${TEST_OOM:-true}";;
      *HostConfig.Memory*) echo "${TEST_MEMORY:-67108864}";;
      *'.Id'*) printf 'a%.0s' {1..64}; echo;;
    esac;;
  exec)
    echo 'CPU_THROTTLED=42 MEMORY_BYTES=4325228544'
    exit "${TEST_EXEC_EXIT:-0}";;
  wait) echo "${TEST_EXIT:-137}";;
  rm) echo cleanup >> "$TEST_CLEANUP";;
esac
''')
            docker.chmod(0o755)
            sleep = Path(tmp) / 'sleep'
            sleep.write_text('#!/bin/sh\nexit 0\n')
            sleep.chmod(0o755)
            journal = Path(tmp) / 'journalctl'
            journal.write_text('#!/bin/sh\nprintf "%s\\n" "$TEST_KERNEL_LOG"\n')
            journal.chmod(0o755)
            for changes, passed, measurement in [
                ({}, True, 'CONTAINER_OOM_CONFIRMED'),
                ({'TEST_QUOTA': '0'}, False, 'CPU_NANOS=0'),
                ({'TEST_EXEC_EXIT': '1'}, False, 'CPU_THROTTLED=42'),
                ({'TEST_OOM': 'false'}, True, 'OOM_EXIT=137 OOM_KILLED=false'),
                ({'TEST_MEMORY': '0'}, False, 'OOM_LIMIT_BYTES=0'),
                ({'TEST_EXIT': '1'}, False, 'OOM_EXIT=1'),
                ({'TEST_KERNEL_LOG': ''}, False, 'OOM_EXIT=137'),
                ({'TEST_KERNEL_LOG': KERNEL_OOM.replace(CID, 'b' * 64)}, False, 'OOM_EXIT=137'),
                ({'TEST_KERNEL_LOG': KERNEL_OOM.replace('CONSTRAINT_MEMCG', 'CONSTRAINT_NONE')}, False, 'OOM_EXIT=137'),
                ({'TEST_KERNEL_LOG': KERNEL_OOM.replace('oom_memcg=', 'task_memcg=')}, False, 'OOM_EXIT=137'),
            ]:
                with self.subTest(changes=changes):
                    cleanup = Path(tmp) / 'cleanup'
                    cleanup.unlink(missing_ok=True)
                    env = dict(os.environ, PATH=tmp + os.pathsep + os.environ['PATH'],
                               TEST_CLEANUP=str(cleanup), TEST_KERNEL_LOG=KERNEL_OOM)
                    env.update(changes)
                    script = (ROOT / 'scripts/docker-qualification/pressure.sh').read_text()
                    script += '\npython3 ' + shlex.quote(str(ROOT / 'scripts/docker-qualification/verify-oom.py')) + ' "$oom_id"\n'
                    result = subprocess.run(['bash', '-s'], input=script,
                                            env=env, capture_output=True, text=True, timeout=5)
                    self.assertEqual(result.returncode == 0, passed, result.stderr)
                    self.assertIn(measurement, result.stdout)
                    self.assertTrue(cleanup.exists())
                    if not passed:
                        self.assertIn('PRESSURE_FAILURE line=', result.stderr)
                        self.assertNotIn('CONTAINER_OOM_CONFIRMED', result.stdout)


if __name__ == '__main__':
    unittest.main()
