#!/usr/bin/env python3
"""Verify failed process cleanup never tears down the isolated data plane."""
import os
from pathlib import Path
import shutil
import signal
import subprocess
import sys
import tempfile
import time
import unittest

ROOT = Path(__file__).resolve().parents[2]


class CleanupGuardTest(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(prefix='braid-down-guard-')
        self.root = Path(self.temp.name)
        (self.root / 'deploy').mkdir()
        (self.root / 'log/e2e').mkdir(parents=True)
        (self.root / 'bin').mkdir()
        for name in ('e2e-down.sh', 'e2e-process.sh'):
            shutil.copy(ROOT / 'deploy' / name, self.root / 'deploy' / name)
        self.command('docker', '#!/bin/sh\necho called >> "$CALLS"\nexit "${DOCKER_EXIT:-0}"\n')
        self.command('sleep', '#!/bin/sh\nexec /usr/bin/sleep 0.03\n')
        self.env = os.environ.copy()
        self.env.update(PATH=str(self.root / 'bin') + os.pathsep + self.env['PATH'],
                        CALLS=str(self.root / 'docker-calls'))
        self.children = []

    def command(self, name, body):
        path = self.root / 'bin' / name
        path.write_text(body)
        path.chmod(0o755)

    def tearDown(self):
        for process in self.children:
            if process.poll() is None:
                os.killpg(process.pid, signal.SIGKILL)
            process.wait(timeout=5)
        self.temp.cleanup()

    def launch(self):
        ready = self.root / 'child-ready'
        ready.unlink(missing_ok=True)
        process = subprocess.Popen(
            [sys.executable, '-c', 'import pathlib,signal,sys,time; signal.signal(signal.SIGTERM, signal.SIG_IGN); pathlib.Path(sys.argv[2]).touch(); time.sleep(60)', 'cmd/relay', str(ready)],
            start_new_session=True)
        self.children.append(process)
        deadline = time.monotonic() + 5
        while not ready.exists():
            if process.poll() is not None or time.monotonic() >= deadline:
                raise RuntimeError('test process did not install its SIGTERM handler')
            time.sleep(0.01)
        log = self.root / 'log/e2e'
        (log / 'relay.pid').write_text(f'{process.pid}\n')
        stat = Path(f'/proc/{process.pid}/stat').read_text().rsplit(') ', 1)[1].split()
        boot = Path('/proc/sys/kernel/random/boot_id').read_text().strip()
        (log / 'relay.identity').write_text(f'{boot} {stat[19]}\n')
        return process

    def down(self, **env):
        return subprocess.run(['bash', str(self.root / 'deploy/e2e-down.sh')],
                              env={**self.env, **env}, capture_output=True, text=True, timeout=15)

    def assert_preserved(self, result):
        self.assertNotEqual(result.returncode, 0, result.stderr)
        self.assertFalse((self.root / 'docker-calls').exists(), result.stderr)
        self.assertIn('保留数据面', result.stderr)

    def test_identity_mismatch_preserves_data_and_process(self):
        process = self.launch()
        (self.root / 'log/e2e/relay.identity').write_text('wrong-boot 1\n')
        self.assert_preserved(self.down())
        self.assertIsNone(process.poll())

    def test_failed_group_inspection_preserves_data_and_process(self):
        process = self.launch()
        self.command('ps', '#!/bin/sh\n[ "$1" != -eo ] || exit 2\nexec /usr/bin/ps "$@"\n')
        inject = self.root / 'record-kill.sh'
        inject.write_text('kill() { echo called >> "$SIGNALS"; builtin kill "$@"; }\n')
        result = self.down(BASH_ENV=str(inject), SIGNALS=str(self.root / 'signals'))
        self.assert_preserved(result)
        self.assertIn('无法检查', result.stderr)
        self.assertFalse((self.root / 'signals').exists())
        self.assertIsNone(process.poll())

    def test_later_group_inspection_failure_preserves_data(self):
        process = self.launch()
        self.command('ps', '#!/bin/sh\nif [ "$1" = -eo ]; then\n'
                     '  [ ! -e "$PS_SEEN" ] || exit 2\n  touch "$PS_SEEN"\nfi\n'
                     'exec /usr/bin/ps "$@"\n')
        result = self.down(PS_SEEN=str(self.root / 'ps-seen'))
        self.assert_preserved(result)
        self.assertIn('进程组仍活动或无法检查', result.stderr)
        self.assertIsNone(process.poll())

    def test_group_still_alive_preserves_data(self):
        process = self.launch()
        inject = self.root / 'inject.sh'
        inject.write_text('kill() { if [ "$1" = -KILL ]; then return 1; fi; builtin kill "$@"; }\n')
        result = self.down(BASH_ENV=str(inject))
        self.assert_preserved(result)
        self.assertIn('进程组仍活动或无法检查', result.stderr)
        self.assertIsNone(process.poll())

    def test_stopped_groups_allow_compose_and_propagate_failure(self):
        result = self.down(DOCKER_EXIT='42')
        self.assertEqual(result.returncode, 42, result.stderr)
        self.assertEqual((self.root / 'docker-calls').read_text().splitlines(), ['called'])
        self.assertIn('退出码 42', result.stderr)

    def test_owned_group_stops_before_compose(self):
        process = self.launch()
        result = self.down()
        self.assertEqual(result.returncode, 0, result.stderr)
        process.wait(timeout=5)
        self.assertEqual((self.root / 'docker-calls').read_text().splitlines(), ['called'])
        self.assertIn('e2e 栈已停', result.stdout)


if __name__ == '__main__':
    unittest.main(verbosity=2)
