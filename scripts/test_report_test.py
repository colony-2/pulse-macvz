import importlib.util
import hashlib
import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
import tarfile
from tempfile import TemporaryDirectory
import unittest
from unittest.mock import patch

spec = importlib.util.spec_from_file_location("report", Path(__file__).with_name("test-report.py"))
report = importlib.util.module_from_spec(spec)
spec.loader.exec_module(report)


class SourceSnapshot(unittest.TestCase):
    def test_snapshot_survives_edits_and_excludes_private_state(self):
        with TemporaryDirectory() as temp:
            root = Path(temp)
            for name in ("assets.go", "go.mod", "go.sum", "flake.nix", "flake.lock", "bases.json",
                         "guest/main.go", "scripts/bootstrap-guest.sh", "internal/provider/launch.go",
                         "tests/integration/c2j-fixture.go.txt"):
                path = root / name
                path.parent.mkdir(parents=True, exist_ok=True)
                path.write_text("original " + name)
            script = root / "scripts/bootstrap-guest.sh"
            script.chmod(0o755)
            for name in (".git/config", ".c2/state", "test-results/old/lease",
                         "scripts/__pycache__/test.pyc", "private-journal.json",
                         "tart-home/vms/test-vm/pulse.json", "pulse-tart-old/native/test-vm.json"):
                path = root / name
                path.parent.mkdir(parents=True, exist_ok=True)
                path.write_text("private")
            destination = root / "test-results/new"
            destination.mkdir()
            report.snapshot(root, destination)
            script.write_text("edited while tests ran")
            copied = destination / "source/scripts/bootstrap-guest.sh"
            self.assertEqual(copied.read_text(), "original scripts/bootstrap-guest.sh")
            self.assertEqual(copied.stat().st_mode & 0o777, 0o755)
            fingerprints = json.loads((destination / "source-sha256.json").read_text())
            self.assertEqual(len(fingerprints), 10)
            for name, digest in fingerprints.items():
                self.assertEqual(hashlib.sha256((destination / "source" / name).read_bytes()).hexdigest(), digest)
            self.assertEqual(len(list(p for p in (destination / "source").rglob("*") if p.is_file())), 10)

    def test_changes_during_capture_fail_before_testing(self):
        with TemporaryDirectory() as temp, patch.object(report, "capture_source", side_effect=[{}, {"changed": (b"", 0)}]):
            with self.assertRaisesRegex(RuntimeError, "Source changed"):
                report.snapshot(Path(temp), Path(temp))
            self.assertFalse((Path(temp) / "source").exists())


class ReportContract(unittest.TestCase):
    def events(self):
        return [dict(Package=report.ROOT_PACKAGE, Test="TestLive", Action="output", Output=m)
                for m in report.MARKERS + report.NATIVE_MARKERS] + [dict(Package=report.ROOT_PACKAGE, Test="TestLive", Action="pass")]

    def test_complete_execution_passes(self):
        self.assertEqual(report.summarize(self.events(), "tart", 0)["result"], "passed")

    def test_image_update_required_only_for_tart(self):
        events = [e for e in self.events() if e.get("Output") != "IMAGE_UPDATE_PASS:"]
        self.assertEqual(report.summarize(events, "tart", 0)["result"], "failed")
        self.assertEqual(report.summarize(events, "orchard", 0)["result"], "passed")

    def test_native_recovery_required_only_for_tart(self):
        for marker in report.NATIVE_MARKERS[1:]:
            events = [e for e in self.events() if e.get("Output") != marker]
            self.assertEqual(report.summarize(events, "tart", 0)["result"], "failed")
            self.assertEqual(report.summarize(events, "orchard", 0)["result"], "passed")

    def test_missing_jobdb_assertion_fails(self):
        for index in range(len(report.MARKERS)):
            events = self.events()
            del events[index]
            self.assertEqual(report.summarize(events, "tart", 0)["result"], "failed")

    def test_skipped_live_test_fails(self):
        events = self.events()
        events[-1]["Action"] = "skip"
        self.assertEqual(report.summarize(events, "tart", 0)["result"], "failed")

    def test_failure_elsewhere_cannot_be_hidden(self):
        events = self.events() + [dict(Package="some/package", Test="TestOther", Action="fail")]
        self.assertEqual(report.summarize(events, "tart", 0)["result"], "failed")
        self.assertEqual(report.summarize(self.events(), "tart", 1)["result"], "failed")

    def test_markers_from_another_test_do_not_count(self):
        events = self.events()
        for event in events[:-1]:
            event["Test"] = "TestSmoke"
        self.assertEqual(report.summarize(events, "tart", 0)["result"], "failed")


class MacDriver(unittest.TestCase):
    def test_success_and_failures_produce_shareable_archives(self):
        for mode in ("pass", "platform", "missing-recovery", "go-failure"):
            with self.subTest(mode=mode), TemporaryDirectory() as temp:
                root = Path(temp)
                scripts = root / "scripts"
                scripts.mkdir()
                for name in ("test-macos.sh", "test-report.py"):
                    shutil.copy2(Path(__file__).with_name(name), scripts / name)
                for name in ("go.mod", "go.sum", "flake.nix", "flake.lock", "bases.json", "uncommitted.go"):
                    (root / name).write_text("fixture")
                (scripts / "test_fixture_test.py").write_text("import unittest\nclass Fixture(unittest.TestCase):\n def test_ok(self): pass\n")
                private = root / "tart-home/vms/private/pulse.json"
                private.parent.mkdir(parents=True)
                private.write_text("private lease")
                binary = root / "bin"
                binary.mkdir()
                (binary / "python3").symlink_to(sys.executable)
                mocks = {
                    "uname": '#!/bin/sh\nif [ "$FAKE_MODE" = platform ]; then echo Linux; elif [ "$1" = -m ]; then echo arm64; else echo Darwin; fi\n',
                    "sw_vers": "#!/bin/sh\necho macOS-fixture\n",
                    "sysctl": "#!/bin/sh\necho hardware-fixture\n",
                    "git": "#!/bin/sh\necho source-fixture\n",
                    "tart": "#!/bin/sh\necho tart-fixture\n",
                    "go": f'''#!{sys.executable}
import json, os, sys
from pathlib import Path
if sys.argv[1] == 'version':
    print('go-fixture')
    sys.exit(0)
assert sys.argv[1:] == ['test', '-json', '-race', '-v', '-count=1', '-timeout=50m', './...']
assert not any(os.environ.get(k) for k in ('PULSE_TEST_DOCKER_LIBRARY', 'PULSE_TEST_DOCKER_ENDPOINT', 'PULSE_TEST_DOCKER_QUALIFICATION'))
assert Path('uncommitted.go').read_text() == 'fixture'
assert Path.cwd().name == 'source'
Path(os.environ['GO_CALLED']).touch()
for marker in {report.MARKERS + report.NATIVE_MARKERS!r}:
    if os.environ['FAKE_MODE'] == 'missing-recovery' and marker == 'FAILED_RESTORE_RECOVERY_PASS:': continue
    print(json.dumps(dict(Package={report.ROOT_PACKAGE!r}, Test='TestLive', Action='output', Output=marker+'\\n')))
print(json.dumps(dict(Package={report.ROOT_PACKAGE!r}, Test='TestLive', Action='pass')))
sys.exit(1 if os.environ['FAKE_MODE'] == 'go-failure' else 0)
''',
                }
                for name, content in mocks.items():
                    executable = binary / name
                    executable.write_text(content)
                    executable.chmod(0o755)
                env = dict(os.environ, PATH=str(binary) + os.pathsep + os.environ["PATH"], FAKE_MODE=mode,
                           GO_CALLED=str(root / "go-called"), PULSE_TEST_TART_HOME=str(root / "tart-home"),
                           PULSE_TEST_DOCKER_LIBRARY="tart", PULSE_TEST_DOCKER_ENDPOINT="1", PULSE_TEST_DOCKER_QUALIFICATION="1")
                result = subprocess.run(["bash", str(scripts / "test-macos.sh"), "tart"], env=env, capture_output=True, text=True, timeout=30)
                self.assertEqual(result.returncode == 0, mode == "pass", result.stdout + result.stderr)
                self.assertEqual((root / "go-called").exists(), mode != "platform")
                archives = list((root / "test-results").glob("*.tar.gz"))
                self.assertEqual(len(archives), 1, result.stdout + result.stderr)
                self.assertIn("Send: " + str(archives[0]), result.stdout)
                with tarfile.open(archives[0]) as archive:
                    summary = next(m for m in archive.getmembers() if m.name.endswith("/summary.json"))
                    data = json.load(archive.extractfile(summary))
                    self.assertEqual(data["result"], "passed" if mode == "pass" else "failed")
                    self.assertFalse(any(m.name.endswith("/pulse.json") for m in archive.getmembers()))
                    if mode == "pass":
                        self.assertTrue(all(data["native_recovery_checks"].values()))


if __name__ == "__main__":
    unittest.main()
