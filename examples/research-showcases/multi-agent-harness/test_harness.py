"""Offline flow, HTTP signatures, limits, and verifier credential boundary."""

import base64
import errno
import http.client
import io
import json
import os
import subprocess
import sys
import tempfile
import time
import unittest
from contextlib import contextmanager, redirect_stdout
from pathlib import Path
from types import SimpleNamespace
from unittest.mock import MagicMock, patch

import harness as h


def delivery(inbox, cursor="10", kind="files.changed", events=None):
    body = json.dumps(
        {
            "type": kind,
            "data": {
                "filesystem_id": inbox.filesystem_id,
                "webhook_id": inbox.webhook_id,
                "cursor": cursor,
                "events": events
                if events is not None
                else [{"type": "created", "path": h.READY}],
            },
        }
    ).encode()
    stamp = str(int(time.time()))
    headers = {
        "webhook-id": "msg-1",
        "webhook-timestamp": stamp,
        "webhook-signature": h.signature(inbox.secret, "msg-1", stamp, body),
    }
    return headers, body


def inbox():
    result = h.Inbox("fs-1", "whsec_" + base64.b64encode(b"test-secret").decode())
    result.webhook_id = "hook-1"
    return result


class MemoryFS:
    def __init__(self):
        self.files = {}
        self.inbox = None

    def write(self, path, value):
        self.files[path] = value.encode()
        if path == h.READY:
            self.inbox.accept(*delivery(self.inbox))

    def read(self, path):
        if path not in self.files:
            error = h.DemoError("missing")
            error.status = 404
            raise error
        return SimpleNamespace(data=self.files[path])


class HarnessTest(unittest.TestCase):
    def test_default_and_explicit_dry_run_are_offline(self):
        for args in ([], ["--dry-run"]):
            with (
                patch.object(h, "live", side_effect=AssertionError("network")),
                redirect_stdout(io.StringIO()) as output,
            ):
                self.assertEqual(h.main(args), 0)
            self.assertIn("signed webhook", output.getvalue())
            self.assertIn("simulated", output.getvalue())

    def test_http_signatures_and_raw_body(self):
        box = inbox()
        headers, body = delivery(box)
        with h.receiver(box, 0) as port:
            conn = http.client.HTTPConnection("127.0.0.1", port, timeout=5)
            for payload, expected in ((body + b" ", 401), (body, 204), (body, 204)):
                conn.request("POST", "/hooks/mountable", body=payload, headers=headers)
                response = conn.getresponse()
                self.assertEqual(response.status, expected)
                response.read()
            conn.close()
        self.assertEqual(box.pending.qsize(), 1)

    def test_reject_stale_wrong_fs_malformed_and_early_delivery(self):
        box = inbox()
        headers, body = delivery(box)
        with patch.object(h.time, "time", return_value=time.time() + 301):
            self.assertEqual(box.accept(headers, body), 401)
        other = inbox()
        other.filesystem_id = "other-fs"
        self.assertEqual(box.accept(*delivery(other)), 400)
        other = inbox()
        other.webhook_id = "other-hook"
        self.assertEqual(box.accept(*delivery(other)), 400)
        self.assertEqual(box.accept({}, b"{}"), 400)
        box.secret = ""
        self.assertEqual(box.accept(headers, body), 503)

    def test_integer_cursor_and_resync(self):
        box = inbox()
        self.assertEqual(box.accept(*delivery(box, "9")), 204)
        self.assertEqual(box.accept(*delivery(box, "10", "files.resync", [])), 204)
        self.assertEqual(box.accept(*delivery(box, "8")), 204)
        self.assertEqual(box.cursor, 10)
        self.assertEqual(box.pending.qsize(), 2)

    def test_resync_before_ready_then_valid_change(self):
        box, fs = inbox(), MemoryFS()
        box.accept(*delivery(box, "1", "files.resync", []))
        # First read is missing; then reconcile the later marker after a change.
        fs.inbox = box
        original_read = fs.read
        calls = []

        def read(path):
            calls.append(path)
            if len(calls) == 1:
                fs.write(h.READY, '{"sha256":"abc"}')
                error = h.DemoError("missing")
                error.status = 404
                raise error
            return original_read(path)

        fs.read = read
        self.assertEqual(h.await_candidate(box, fs, timeout=1), {"sha256": "abc"})
        self.assertEqual(len(calls), 2)
        with self.assertRaises(h.DemoError):
            h.await_candidate(inbox(), fs, timeout=0.001)

    def test_caps_stop_without_retry(self):
        http = MagicMock()
        http.request.return_value = SimpleNamespace(
            is_success=False,
            status_code=402,
            json=lambda: {"detail": {"code": "filesystem_spending_cap_reached"}},
        )
        with self.assertRaisesRegex(h.DemoError, "402 filesystem_spending_cap_reached"):
            h.Control(http).call("POST", "/example", h.LIMITS)
        http.request.assert_called_once()

    def test_management_error_shapes_and_non_json_fallback(self):
        for payload, expected in (
            ({"detail": {"code": "spending_cap_reached"}}, "spending_cap_reached"),
            ({"code": "rate_limited"}, "rate_limited"),
            ({"detail": "private upstream message"}, "request_failed"),
            ([], "request_failed"),
        ):
            http = MagicMock()
            http.request.return_value = SimpleNamespace(
                is_success=False, status_code=402, json=lambda payload=payload: payload
            )
            with self.assertRaisesRegex(h.DemoError, expected):
                h.Control(http).call("POST", "/example")
            http.request.assert_called_once()
        http = MagicMock()
        http.request.return_value.is_success = False
        http.request.return_value.status_code = 502
        http.request.return_value.json.side_effect = ValueError("not JSON")
        with self.assertRaisesRegex(h.DemoError, "502 request_failed"):
            h.Control(http).call("POST", "/example")

    def test_live_mock_provisions_limits_handoff_and_cleanup(self):
        fs, box, calls = MemoryFS(), inbox(), []
        client = MagicMock()
        client.__enter__.return_value = client
        client.filesystem.return_value = fs

        def call(method, path, body=None):
            calls.append((method, path, body))
            if method == "POST" and path.endswith("/filesystems"):
                self.assertEqual({k: body[k] for k in h.LIMITS}, h.LIMITS)
                return {"id": "fs-1"}
            if method == "POST":
                return {"id": "hook-1", "secret": box.secret}

        @contextmanager
        def receiver(actual_box, port):
            fs.inbox = actual_box
            yield port

        @contextmanager
        def mount(_client, _control, fs_id, root):
            self.assertEqual(fs_id, "fs-1")
            for path, value in fs.files.items():
                (root / path.lstrip("/")).write_bytes(value)
            original = Path.write_text

            def deny(path, *args, **kwargs):
                if path.name == "verifier-must-not-write":
                    raise OSError(errno.EROFS, "read-only")
                return original(path, *args, **kwargs)

            with patch.object(Path, "write_text", deny):
                yield root
            for file in root.iterdir():
                file.unlink()  # Simulate unmount revealing the empty mount point.

        modules = {
            "httpx": SimpleNamespace(Client=MagicMock()),
            "mountable_sdk": SimpleNamespace(Mountable=lambda **kw: client),
        }
        with (
            patch.dict(sys.modules, modules),
            patch.dict(os.environ, {"MOUNTABLE_API_KEY": "never-print-me"}),
            patch.object(h.Control, "call", side_effect=call),
            patch.object(h, "receiver", receiver),
            patch.object(h, "readonly_mount", mount),
            redirect_stdout(io.StringIO()) as output,
        ):
            self.assertEqual(
                h.main(
                    [
                        "--live",
                        "--org",
                        "org-1",
                        "--webhook-url",
                        "https://example.com/hooks/mountable",
                    ]
                ),
                0,
            )
        self.assertIn("Verifier passed", output.getvalue())
        self.assertNotIn("never-print-me", output.getvalue())
        self.assertEqual(
            calls[-1][:2],
            ("DELETE", "/api/v1/orgs/org-1/filesystems/fs-1/webhooks/hook-1"),
        )
        client.filesystem.assert_called_once_with("fs-1", mode="rw")

    def test_readonly_mount_uses_ro_ticket_stdin_and_revokes_on_failure(self):
        client, control, process = MagicMock(), MagicMock(), MagicMock()
        client.create_mount_ticket.return_value = SimpleNamespace(
            id="session-1", ticket="one-time-secret"
        )
        process.stdout.readline.return_value = '{"event":"mounted"}\n'
        selector = MagicMock()
        selector.__enter__.return_value = selector
        selector.select.return_value = [object()]
        with (
            tempfile.TemporaryDirectory() as tmp,
            patch.object(h.selectors, "DefaultSelector", return_value=selector),
            patch.object(h.subprocess, "Popen", return_value=process) as popen,
            patch.object(
                h.subprocess, "run", return_value=SimpleNamespace(returncode=0)
            ),
            patch.dict(
                os.environ,
                {"MOUNTABLE_API_KEY": "admin-secret", "OTHER_SECRET": "cloud-secret"},
            ),
        ):
            root = Path(tmp) / "review"
            with (
                self.assertRaisesRegex(h.DemoError, "verification failed"),
                h.readonly_mount(client, control, "fs-1", root),
            ):
                raise h.DemoError("verification failed")
            args, kwargs = popen.call_args
            self.assertEqual(
                args[0],
                ["mountable", "mount", "--ro", "--ticket-stdin", "--json", str(root)],
            )
            self.assertEqual(set(kwargs["env"]), {"PATH", "HOME"})
            self.assertNotIn("secret", str(kwargs["env"]))
        self.assertEqual(client.create_mount_ticket.call_args.kwargs["mode"], "ro")
        process.stdin.write.assert_called_once_with("one-time-secret\n")
        control.call.assert_called_once_with(
            "DELETE", "/api/v1/mount-sessions/session-1"
        )

    def test_changed_candidate_and_broken_code_fail_verification(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            (root / "adder.py").write_text(h.BEFORE)
            (root / "test_adder.py").write_text(h.TEST)
            with self.assertRaises(h.DemoError):
                h.check_candidate(root, {"sha256": "wrong"})
            with (
                self.assertRaises(subprocess.CalledProcessError),
                redirect_stdout(io.StringIO()),
            ):
                h.check_candidate(
                    root, {"sha256": h.hashlib.sha256(h.BEFORE.encode()).hexdigest()}
                )


if __name__ == "__main__":
    unittest.main()
