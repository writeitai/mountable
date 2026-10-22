"""Offline tests for the spend-cap and file-change webhook showcases."""

from __future__ import annotations

import base64
import contextlib
import io
import json
import os
import tempfile
import threading
import time
import unittest
import urllib.error
import urllib.request
from unittest import mock
from http.server import BaseHTTPRequestHandler, HTTPServer
from pathlib import Path

import showcase
import spend_cap
import webhooks
from rest import ApiError, Rest

API_KEY = "mtbl_orchestrator_secret_showcase"


def _texts(trace: dict[str, object]) -> str:
    parts: list[str] = [json.dumps(trace)]
    panes = trace["panes"]
    assert isinstance(panes, list)
    for pane in panes:
        assert isinstance(pane, dict)
        lines = pane["lines"]
        assert isinstance(lines, list)
        for line in lines:
            assert isinstance(line, dict)
            parts.append(str(line["text"]))
        parts.append(str(pane.get("title", "")))
        parts.append(str(pane.get("subtitle", "")))
    parts.append(str(trace.get("contract", "")))
    return "\n".join(parts)


def _t_ms(trace: dict[str, object], prefix: str) -> int:
    panes = trace["panes"]
    assert isinstance(panes, list)
    for pane in panes:
        assert isinstance(pane, dict)
        lines = pane["lines"]
        assert isinstance(lines, list)
        for line in lines:
            assert isinstance(line, dict)
            if str(line["text"]).startswith(prefix):
                return int(line["t_ms"])
    raise AssertionError(prefix)


def _max_ms(trace: dict[str, object]) -> int:
    found = 0
    panes = trace["panes"]
    assert isinstance(panes, list)
    for pane in panes:
        assert isinstance(pane, dict)
        lines = pane["lines"]
        assert isinstance(lines, list)
        for line in lines:
            assert isinstance(line, dict)
            found = max(found, int(line["t_ms"]))
    return found


class FakeFilesystems:
    def __init__(self, *, stop_a: bool = True, stop_b: bool = False) -> None:
        self.stop_a = stop_a
        self.stop_b = stop_b
        self.created: list[tuple[str, int]] = []
        self.writes: list[tuple[str, str]] = []
        self.files: dict[tuple[str, str], bytes] = {}
        self.closed = False

    def create(self, name: str, spending_cap_cents: int) -> str:
        self.created.append((name, spending_cap_cents))
        return "fs_user_a" if spending_cap_cents == spend_cap.USER_A_CAP else "fs_user_b"

    def write(self, fs_id: str, path: str, body: bytes) -> None:
        self.writes.append((fs_id, path))
        if fs_id == "fs_user_a" and self.stop_a:
            raise spend_cap.CapReached(402, spend_cap.CAP_CODE)
        if fs_id == "fs_user_b" and self.stop_b:
            raise spend_cap.CapReached(402, spend_cap.CAP_CODE)
        self.files[(fs_id, path)] = body

    def read(self, fs_id: str, path: str) -> bytes:
        return self.files[(fs_id, path)]

    def close(self) -> None:
        self.closed = True


class IllustrativeTests(unittest.TestCase):
    def test_spend_cap_dry_run_stops_one_user(self) -> None:
        trace = spend_cap.illustrative_spend_cap()
        encoded = _texts(trace)
        self.assertEqual(trace["id"], "spend-cap")
        self.assertEqual(trace["source"], "illustrative")
        self.assertIsNone(trace["measured_abort_ms"])
        self.assertEqual(len(trace["panes"]), 3)  # type: ignore[arg-type]
        self.assertLessEqual(_max_ms(trace), 30_000)
        self.assertIn("402 filesystem_spending_cap_reached", encoded)
        self.assertIn('"spending_cap_cents":0', encoded)
        self.assertIn('"spending_cap_cents":500', encoded)
        self.assertLess(
            _t_ms(trace, "HTTP 402"),
            _t_ms(trace, "$ curl -X PUT /api/v1/filesystems/fs_user_b/"),
        )
        self.assertIn("working", encoded)
        self.assertNotIn("tenant", encoded.lower())
        self.assertNotIn("mtbl_", encoded)
        self.assertNotIn("whsec_", encoded)
        self.assertIn("does not create a filesystem", str(trace["contract"]))

    def test_webhooks_dry_run_shows_a_rename_as_deleted_and_created(self) -> None:
        trace = webhooks.illustrative_webhooks()
        encoded = _texts(trace)
        self.assertEqual(trace["id"], "webhooks")
        self.assertEqual(trace["source"], "illustrative")
        self.assertIsNone(trace["measured_abort_ms"])
        self.assertEqual(len(trace["panes"]), 2)  # type: ignore[arg-type]
        self.assertLessEqual(_max_ms(trace), 30_000)
        self.assertIn('"type":"files.changed"', encoded)
        self.assertIn('"type":"created","path":"/notes/draft.txt"', encoded)
        self.assertIn('"type":"updated","path":"/notes/draft.txt"', encoded)
        self.assertIn('"type":"deleted","path":"/notes/draft.txt"', encoded)
        self.assertIn('"type":"created","path":"/notes/report.txt"', encoded)
        self.assertLess(_t_ms(trace, "$ printf draft"), _t_ms(trace, "$ printf final"))
        self.assertLess(_t_ms(trace, "$ printf final"), _t_ms(trace, "$ mv "))
        self.assertIn("compared as an integer", str(trace["contract"]))
        self.assertNotIn("tenant", encoded.lower())
        self.assertNotIn("mtbl_", encoded)
        self.assertNotIn("whsec_", encoded)

    def test_cli_dry_run_writes_a_trace_and_usage_needs_credentials(self) -> None:
        stdout = io.StringIO()
        stderr = io.StringIO()
        with contextlib.redirect_stdout(stdout), contextlib.redirect_stderr(stderr), tempfile.TemporaryDirectory() as tmp:
            path = Path(tmp) / "spend-cap.json"
            code = spend_cap.main(["--dry-run", "--trace", str(path)])
            self.assertEqual(code, 0)
            saved = json.loads(path.read_text())
        self.assertEqual(saved["id"], "spend-cap")
        self.assertIn("User B", stdout.getvalue())
        env = os.environ.copy()
        for key in ("MOUNTABLE_API_KEY", "MOUNTABLE_ORG_ID", "MOUNTABLE_ORG"):
            env.pop(key, None)
        with mock.patch.dict(os.environ, env, clear=True):
            stderr = io.StringIO()
            with contextlib.redirect_stderr(stderr):
                code = spend_cap.main([])
            self.assertEqual(code, 2)
            self.assertIn("--dry-run", stderr.getvalue())
            stderr = io.StringIO()
            with contextlib.redirect_stderr(stderr):
                hook_code = webhooks.main([])
            self.assertEqual(hook_code, 2)
            self.assertIn("--dry-run", stderr.getvalue())
        stdout = io.StringIO()
        stderr = io.StringIO()
        with contextlib.redirect_stdout(stdout), contextlib.redirect_stderr(stderr):
            code = webhooks.main(["--dry-run"])
        self.assertEqual(code, 0)
        self.assertIn("files.changed", stdout.getvalue())
        self.assertIn("dry run passed", stderr.getvalue())


class SpendCapRunTests(unittest.TestCase):
    def test_user_a_stops_and_user_b_keeps_working(self) -> None:
        fake = FakeFilesystems()
        trace = spend_cap.run_spend_cap(fake, secrets=[API_KEY])
        encoded = _texts(trace)
        self.assertEqual(trace["source"], "recorded")
        self.assertNotIn(API_KEY, encoded)
        self.assertIn("402 filesystem_spending_cap_reached", encoded)
        self.assertIn("working", encoded)
        self.assertTrue(fake.closed)
        self.assertEqual([cap for _name, cap in fake.created], [0, 500])
        self.assertTrue(all(name.startswith("showcase-user-") for name, _cap in fake.created))
        user_a = [path for fs_id, path in fake.writes if fs_id == "fs_user_a"]
        self.assertEqual(user_a, ["notes/loop-1.txt"])
        self.assertEqual(fake.files[("fs_user_b", "notes/status.txt")], b"working\n")
        self.assertLess(
            _t_ms(trace, "HTTP 402"),
            _t_ms(trace, "$ curl -X PUT /api/v1/filesystems/fs_user_b/"),
        )

    def test_a_loop_that_never_hits_the_cap_fails_closed(self) -> None:
        fake = FakeFilesystems(stop_a=False)
        with self.assertRaises(showcase.ShowcaseError) as caught:
            spend_cap.run_spend_cap(fake)
        self.assertIn("not stopped", str(caught.exception))
        self.assertEqual(len([item for item in fake.writes if item[0] == "fs_user_a"]), spend_cap.ATTEMPTS)
        self.assertNotIn("fs_user_b", [fs_id for fs_id, _path in fake.writes])
        self.assertTrue(fake.closed)

    def test_user_b_is_not_allowed_to_stop(self) -> None:
        fake = FakeFilesystems(stop_b=True)
        with self.assertRaises(showcase.ShowcaseError) as caught:
            spend_cap.run_spend_cap(fake)
        self.assertIn("user B was stopped", str(caught.exception))
        self.assertTrue(fake.closed)


class WebhookLogicTests(unittest.TestCase):
    def test_agent_script_renames_the_draft(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            webhooks.agent_script(root)
            self.assertEqual((root / "notes" / "report.txt").read_text(), "final\n")
            self.assertFalse((root / "notes" / "draft.txt").exists())

    def test_resync_advances_the_cursor_without_events(self) -> None:
        state = webhooks.State()
        message = {
            "type": "files.resync",
            "data": {"filesystem_id": "fs", "webhook_id": "wh", "events": [], "cursor": "4"},
        }
        self.assertTrue(webhooks.apply_message(state, message))
        self.assertEqual(state.cursor, 4)
        self.assertEqual(state.snapshot(), [])
        self.assertFalse(webhooks.apply_message(state, message))

    def test_extra_events_still_match_the_rename_in_order(self) -> None:
        events = [
            {"type": "created", "path": "/notes"},
            {"type": "created", "path": "/notes/draft.txt"},
            {"type": "updated", "path": "/notes/draft.txt"},
            {"type": "updated", "path": "/notes/draft.txt"},
            {"type": "deleted", "path": "/notes/draft.txt"},
            {"type": "created", "path": "/notes/report.txt"},
        ]
        self.assertTrue(webhooks.contains_in_order(events, webhooks.EXPECTED))

    def test_receiver_checks_the_signature_before_applying(self) -> None:
        secret = "whsec_" + base64.b64encode(b"receiver-secret").decode()
        state = webhooks.State()
        state.filesystem_id = "fs_offline"
        state.webhook_id = "wh_offline"
        body = webhooks.files_changed(
            "fs_offline",
            "wh_offline",
            "1",
            [("created", "/notes/draft.txt")],
        )
        stamp = str(int(time.time()))
        with webhooks.receiver(state, 0) as port:
            refused = urllib.request.Request(
                f"http://127.0.0.1:{port}/hooks/mountable",
                data=body,
                method="POST",
            )
            with self.assertRaises(urllib.error.HTTPError) as early:
                urllib.request.urlopen(refused, timeout=2)
            self.assertEqual(early.exception.code, 503)
            state.secret = secret
            signed = urllib.request.Request(
                f"http://127.0.0.1:{port}/hooks/mountable",
                data=body,
                method="POST",
                headers=webhooks.sign(secret, "msg-1", stamp, body),
            )
            with urllib.request.urlopen(signed, timeout=2) as response:
                self.assertEqual(response.status, 204)
        self.assertEqual(state.snapshot(), [{"type": "created", "path": "/notes/draft.txt"}])

    def test_stale_timestamp_is_rejected(self) -> None:
        secret = "whsec_" + base64.b64encode(b"stale-secret").decode()
        body = b"{}"
        headers = webhooks.sign(secret, "msg", "1", body)
        self.assertFalse(webhooks.verify_webhook(secret, headers, body))

    def test_a_later_signature_matches_and_a_short_one_does_not_crash(self) -> None:
        secret = "whsec_" + base64.b64encode(b"primary-secret").decode()
        other = "whsec_" + base64.b64encode(b"previous-secret").decode()
        body = b'{"type":"files.resync"}\n'
        stamp = str(int(time.time()))
        good = webhooks.sign(secret, "msg-rotate", stamp, body)
        other_sig = webhooks.sign(other, "msg-rotate", stamp, body)
        rotated = dict(good)
        rotated["webhook-signature"] = "v1,aa " + other_sig["webhook-signature"] + " " + good["webhook-signature"]
        self.assertTrue(webhooks.verify_webhook(secret, rotated, body))
        short = dict(good)
        short["webhook-signature"] = "v1,aa"
        self.assertFalse(webhooks.verify_webhook(secret, short, body))

    def test_cursors_compare_as_integers(self) -> None:
        secret = "whsec_" + base64.b64encode(b"cursor-secret").decode()
        webhooks.cursors_compare_as_integers(secret, str(int(time.time())))

    def test_opening_resync_unblocks_the_live_wait(self) -> None:
        state = webhooks.State()
        with self.assertRaises(showcase.ShowcaseError):
            webhooks.wait_for_resync(state, 0.01)
        message = {
            "type": "files.resync",
            "data": {"filesystem_id": "fs", "webhook_id": "wh", "events": [], "cursor": "0"},
        }

        def deliver() -> None:
            time.sleep(0.05)
            self.assertTrue(webhooks.apply_message(state, message))

        thread = threading.Thread(target=deliver)
        thread.start()
        try:
            self.assertEqual(webhooks.wait_for_resync(state, 2), 0)
        finally:
            thread.join(timeout=2)
        self.assertEqual(state.snapshot(), [])


class RestTests(unittest.TestCase):
    def test_cap_response_is_parsed_and_redirects_are_not_followed(self) -> None:
        seen: list[str] = []
        hits = {"put": 0}

        class Handler(BaseHTTPRequestHandler):
            def log_message(self, format: str, *args: object) -> None:
                return

            def _auth(self) -> None:
                seen.append(self.headers.get("Authorization", ""))

            def do_PUT(self) -> None:  # noqa: N802
                self._auth()
                hits["put"] += 1
                body = b'{"detail":{"code":"filesystem_spending_cap_reached","message":"cap mtbl_secret_should_not_leak"}}'
                self.send_response(402)
                self.send_header("Content-Length", str(len(body)))
                self.send_header("Content-Type", "application/json")
                self.end_headers()
                self.wfile.write(body)

            def do_GET(self) -> None:  # noqa: N802
                self._auth()
                if self.path.endswith("/redirect"):
                    self.send_response(302)
                    self.send_header("Location", "http://127.0.0.1/elsewhere")
                    self.send_header("Content-Length", "0")
                    self.end_headers()
                    return
                body = b"working\n"
                self.send_response(200)
                self.send_header("Content-Length", str(len(body)))
                self.end_headers()
                self.wfile.write(body)

            def do_POST(self) -> None:  # noqa: N802
                self._auth()
                length = int(self.headers.get("Content-Length", "0"))
                self.rfile.read(length)
                body = b'{"id":"fs_created","spending_cap_cents":0}'
                self.send_response(201)
                self.send_header("Content-Length", str(len(body)))
                self.end_headers()
                self.wfile.write(body)

        server = HTTPServer(("127.0.0.1", 0), Handler)
        thread = threading.Thread(target=server.serve_forever, daemon=True)
        thread.start()
        try:
            port = server.server_address[1]
            rest = Rest(API_KEY, base_url=f"http://127.0.0.1:{port}")
            with self.assertRaises(ApiError) as caught:
                rest.request(
                    "PUT",
                    "/api/v1/filesystems/fs/files/notes/loop-1.txt",
                    data=b"note\n",
                    content_type="application/octet-stream",
                )
            self.assertEqual(caught.exception.status, 402)
            self.assertEqual(caught.exception.code, "filesystem_spending_cap_reached")
            self.assertNotIn(API_KEY, str(caught.exception))
            self.assertNotIn("mtbl_secret_should_not_leak", str(caught.exception))
            self.assertEqual(hits["put"], 1)
            created = rest.json("POST", "/api/v1/orgs/org/filesystems", {"name": "user-a", "spending_cap_cents": 0})
            self.assertEqual(created["id"], "fs_created")
            _status, raw = rest.request("GET", "/api/v1/filesystems/fs/files/notes/status.txt")
            self.assertEqual(raw, b"working\n")
            with self.assertRaises(ApiError) as redirected:
                rest.request("GET", "/redirect")
            self.assertEqual(redirected.exception.status, 302)
            self.assertEqual(redirected.exception.code, "redirect")
            self.assertTrue(all(header == "Bearer " + API_KEY for header in seen))
        finally:
            server.shutdown()
            server.server_close()
            thread.join(timeout=5)


if __name__ == "__main__":
    unittest.main()
