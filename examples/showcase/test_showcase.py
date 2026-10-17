"""The showcase scripts keep the API key out of sandbox mounts and traces."""

from __future__ import annotations

import contextlib
import io
import json
import os
import tempfile
import unittest
from pathlib import Path

import showcase

API_KEY = "mtbl_orchestrator_secret_showcase"
PARENT = {
    showcase.API_KEY_ENV: API_KEY,
    "PATH": os.environ.get("PATH", ""),
    "HOME": tempfile.gettempdir(),
    "NOTE": f"prefix-{API_KEY}",
}


def _texts(trace: dict[str, object]) -> str:
    return json.dumps(trace)


def _max_ms(trace: dict[str, object]) -> int:
    panes = trace["panes"]
    assert isinstance(panes, list)
    found = 0
    for pane in panes:
        assert isinstance(pane, dict)
        lines = pane["lines"]
        assert isinstance(lines, list)
        for line in lines:
            assert isinstance(line, dict)
            found = max(found, int(line["t_ms"]))
    return found


class IllustrativeTests(unittest.TestCase):
    def test_relay_dry_run_is_three_panes_without_secrets(self) -> None:
        trace = showcase.illustrative_relay()
        encoded = _texts(trace)
        self.assertEqual(trace["source"], "illustrative")
        self.assertEqual(len(trace["panes"]), 3)  # type: ignore[arg-type]
        self.assertLessEqual(_max_ms(trace), 30_000)
        self.assertIn("Read-only file system", encoded)
        self.assertIn("sandbox A destroyed", encoded)
        self.assertIn(showcase.PLAN.rstrip(), showcase.format_script(trace))
        self.assertNotIn("mtbltk_", encoded)
        self.assertNotIn("mtbl_", encoded)
        self.assertIn("does not create a filesystem", str(trace["contract"]))

    def test_kill_switch_dry_run_states_the_contract(self) -> None:
        trace = showcase.illustrative_kill_switch()
        encoded = _texts(trace)
        self.assertEqual(len(trace["panes"]), 2)  # type: ignore[arg-type]
        self.assertLessEqual(_max_ms(trace), 30_000)
        self.assertIn(showcase.ABORT_LINE, encoded)
        self.assertIn("15 seconds", str(trace["contract"]))
        self.assertIn("2–4 seconds", str(trace["contract"]))
        self.assertIn("not a new measurement", str(trace["contract"]))
        self.assertNotIn("mtbltk_", encoded)
        self.assertNotIn("mtbl_", encoded)
        revoke_at = 3200
        abort_at = 7200
        self.assertIn(f'"t_ms": {revoke_at}', encoded)
        self.assertEqual(abort_at - revoke_at, 4000)

    def test_cli_dry_run_and_usage(self) -> None:
        stdout = io.StringIO()
        with contextlib.redirect_stdout(stdout), tempfile.TemporaryDirectory() as tmp:
            path = Path(tmp) / "relay.json"
            code = showcase.relay_main(["--dry-run", "--trace", str(path)])
            self.assertEqual(code, 0)
            saved = json.loads(path.read_text())
        self.assertEqual(saved["id"], "relay")
        self.assertIn("Sandbox B", stdout.getvalue())
        stderr = io.StringIO()
        with contextlib.redirect_stderr(stderr):
            code = showcase.kill_switch_main([])
        self.assertEqual(code, 2)
        self.assertIn("--dry-run", stderr.getvalue())


class FakeRunTests(unittest.TestCase):
    def _backend(self) -> showcase.FakeBackend:
        return showcase.FakeBackend(PARENT)

    def test_relay_hands_off_files_and_rejects_the_laptop_write(self) -> None:
        backend = self._backend()
        with tempfile.TemporaryDirectory() as tmp:
            trace = showcase.run_relay(backend, Path(tmp), fs_id="fs_showcase", create=False)
        encoded = _texts(trace)
        self.assertEqual(trace["source"], "recorded")
        self.assertNotIn(API_KEY, encoded)
        self.assertNotIn("mtbltk_", encoded)
        self.assertIn("cannot touch", encoded)
        self.assertNotIn("notes.md", backend.cloud)
        self.assertEqual(backend.cloud["PLAN.md"], showcase.PLAN)
        self.assertEqual(backend.cloud["adder.py"], showcase.ADDER)
        self.assertEqual(len(backend.sandbox_envs), 2)
        for env in backend.sandbox_envs:
            self.assertNotIn(showcase.API_KEY_ENV, env)
            self.assertNotIn(API_KEY, json.dumps(env))
        self.assertEqual(len(backend.login_envs), 1)
        self.assertEqual(backend.login_envs[0][showcase.API_KEY_ENV], API_KEY)
        first_unmount = backend.events.index("unmount")
        second_mount = backend.events.index("mount-ticket", backend.events.index("mount-ticket") + 1)
        self.assertLess(first_unmount, second_mount)
        self.assertEqual(
            [event for event in backend.events if event != "unmount"],
            ["mount-ticket", "mount-ticket", "mount-login"],
        )

    def test_kill_switch_revokes_the_sandbox_session_only(self) -> None:
        backend = self._backend()
        with tempfile.TemporaryDirectory() as tmp:
            trace = showcase.run_kill_switch(
                backend,
                Path(tmp),
                fs_id="fs_showcase",
                create=False,
                pause_s=0,
            )
        encoded = _texts(trace)
        self.assertEqual(trace["source"], "recorded")
        self.assertNotIn(API_KEY, encoded)
        self.assertNotIn("mtbltk_", encoded)
        self.assertIn(showcase.ABORT_LINE, encoded)
        self.assertIsInstance(trace["measured_abort_ms"], int)
        self.assertLess(trace["measured_abort_ms"], 20_000)
        self.assertEqual(backend.revoked, ["ses_1", "ses_1"])
        self.assertEqual(len(backend.sandbox_envs), 1)
        self.assertNotIn(API_KEY, json.dumps(backend.sandbox_envs[0]))


if __name__ == "__main__":
    unittest.main()
