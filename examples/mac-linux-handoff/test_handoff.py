"""Check the two-process turn order and failures without a network service."""

import contextlib
import importlib.util
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest

HERE = Path(__file__).resolve().parent
spec = importlib.util.spec_from_file_location("handoff", HERE / "handoff.py")
handoff = importlib.util.module_from_spec(spec)
spec.loader.exec_module(handoff)


class HandoffTest(unittest.TestCase):
    def test_both_processes_finish_and_remove_message_files(self):
        with tempfile.TemporaryDirectory() as root:
            commands = [
                [
                    sys.executable,
                    str(HERE / "handoff.py"),
                    role,
                    root,
                    "--run-id",
                    "test",
                ]
                for role in ("mac", "linux")
            ]
            processes = [
                subprocess.Popen(
                    command, stdout=subprocess.PIPE, stderr=subprocess.PIPE
                )
                for command in commands
            ]
            try:
                for process in processes:
                    output, error = process.communicate(timeout=10)
                    self.assertEqual(process.returncode, 0, error.decode())
                    self.assertIn(b"handoff complete", output)
                self.assertEqual(list((Path(root) / "handoff-test").iterdir()), [])
            finally:
                for process in processes:
                    if process.poll() is None:
                        process.kill()
                    process.wait()

    def test_existing_directory_is_never_overwritten(self):
        with tempfile.TemporaryDirectory() as root:
            directory = Path(root) / "handoff-existing"
            directory.mkdir()
            original = directory / "result.txt"
            original.write_bytes(b"keep this")
            with self.assertRaises(FileExistsError):
                handoff.run("linux", Path(root), "existing", 1)
            self.assertEqual(original.read_bytes(), b"keep this")

    def test_wrong_bytes_do_not_acknowledge_a_result(self):
        with tempfile.TemporaryDirectory() as root:
            directory = Path(root) / "handoff-wrong"
            directory.mkdir()
            original = directory / "result.txt"
            original.write_bytes(b"wrong")
            with self.assertRaises(TimeoutError), contextlib.redirect_stdout(None):
                handoff.run("mac", Path(root), "wrong", 0.1)
            self.assertTrue(original.exists())
            self.assertFalse((directory / "reviewed.txt").exists())


if __name__ == "__main__":
    unittest.main()
