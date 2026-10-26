"""Signature, cursor and Slack posting checks. No network calls."""

from __future__ import annotations

import base64
import hashlib
import hmac
import http.client
import importlib.util
import json
import threading
import time
import unittest
from pathlib import Path

HERE = Path(__file__).resolve().parent
spec = importlib.util.spec_from_file_location("receiver", HERE / "receiver.py")
receiver = importlib.util.module_from_spec(spec)
spec.loader.exec_module(receiver)

# Standard Webhooks reference vector. The gateway signs this same input.
VECTOR = {
    "secret": "whsec_MfKQ9r8GKYqrTwjUPD8ILPZIo2LaLaSw",
    "id": "msg_p5jXN8AQM9LWM0D4loKWxJek",
    "timestamp": 1614265330,
    "body": '{"test": 2432232314}',
    "signature": "v1,g0hM9SsE+OTPJTGt/tmIKtSyZlE3uFJELVlNIOLJ1OE=",
}
SECRET = "whsec_" + base64.b64encode(b"example-secret-key").decode()
NOW = 1700000000


def sign(secret: str, msg_id: str, timestamp: int, body: bytes) -> dict[str, str]:
    key = base64.b64decode(secret.removeprefix("whsec_"), validate=True)
    digest = hmac.new(
        key, f"{msg_id}.{timestamp}.".encode() + body, hashlib.sha256
    ).digest()
    return {
        "webhook-id": msg_id,
        "webhook-timestamp": str(timestamp),
        "webhook-signature": "v1," + base64.b64encode(digest).decode(),
    }


def changed(cursor: str, events: list[tuple[str, str]]) -> bytes:
    return json.dumps(
        {
            "type": "files.changed",
            "data": {
                "webhook_id": "wh_example",
                "filesystem_id": "fs_example",
                "events": [
                    {"type": kind, "path": path, "file": None} for kind, path in events
                ],
                "cursor": cursor,
            },
        },
        separators=(",", ":"),
    ).encode()


class Recording:
    def __init__(self, fail: bool = False) -> None:
        self.messages: list[str] = []
        self.fail = fail

    def __call__(self, text: str) -> None:
        if self.fail:
            raise receiver.SlackError("down")
        self.messages.append(text)


class ReceiverTest(unittest.TestCase):
    def test_reference_vector_matches_and_rejects_changes(self):
        headers = {
            "webhook-id": VECTOR["id"],
            "webhook-timestamp": str(VECTOR["timestamp"]),
            "webhook-signature": f"v1,{base64.b64encode(b'not-ours').decode()} {VECTOR['signature']}",
        }
        body = VECTOR["body"].encode()
        self.assertTrue(
            receiver.verify_webhook(
                VECTOR["secret"], headers, body, now=VECTOR["timestamp"] + 10
            )
        )
        self.assertFalse(
            receiver.verify_webhook(
                VECTOR["secret"], headers, body + b" ", now=VECTOR["timestamp"]
            )
        )
        self.assertFalse(
            receiver.verify_webhook(
                "whsec_" + "A" * 32, headers, body, now=VECTOR["timestamp"]
            )
        )
        self.assertFalse(
            receiver.verify_webhook(
                VECTOR["secret"], headers, body, now=VECTOR["timestamp"] + 301
            )
        )

    def test_created_posts_once_and_a_repeat_does_not(self):
        slack = Recording()
        hook = receiver.Receiver(SECRET, slack)
        body = changed("1", [("created", "/notes/report.md")])
        headers = sign(SECRET, "msg_1", NOW, body)
        self.assertEqual(hook.handle(headers, body, now=NOW), 204)
        self.assertEqual(slack.messages, ["created /notes/report.md"])
        self.assertEqual(hook.handle(headers, body, now=NOW), 204)
        self.assertEqual(slack.messages, ["created /notes/report.md"])

    def test_numeric_cursor_accepts_10_after_9(self):
        slack = Recording()
        hook = receiver.Receiver(SECRET, slack)
        first = changed("9", [("created", "/notes/a.md")])
        second = changed("10", [("updated", "/notes/a.md")])
        self.assertEqual(
            hook.handle(sign(SECRET, "msg_9", NOW, first), first, now=NOW), 204
        )
        self.assertEqual(
            hook.handle(sign(SECRET, "msg_10", NOW, second), second, now=NOW), 204
        )
        self.assertEqual(
            slack.messages, ["created /notes/a.md", "updated /notes/a.md"]
        )

    def test_bad_signature_does_not_post(self):
        slack = Recording()
        hook = receiver.Receiver(SECRET, slack)
        body = changed("1", [("created", "/notes/report.md")])
        headers = sign(SECRET, "msg_1", NOW, body)
        headers["webhook-signature"] = "v1," + base64.b64encode(b"nope").decode()
        self.assertEqual(hook.handle(headers, body, now=NOW), 400)
        self.assertEqual(slack.messages, [])
        self.assertEqual(hook.cursor, -1)

    def test_resync_does_not_post_and_a_later_write_does(self):
        slack = Recording()
        hook = receiver.Receiver(SECRET, slack)
        resync = json.dumps(
            {
                "type": "files.resync",
                "data": {
                    "webhook_id": "wh_example",
                    "filesystem_id": "fs_example",
                    "events": [],
                    "cursor": "4",
                },
            },
            separators=(",", ":"),
        ).encode()
        self.assertEqual(
            hook.handle(sign(SECRET, "msg_r", NOW, resync), resync, now=NOW), 204
        )
        self.assertEqual(slack.messages, [])
        body = changed("5", [("deleted", "/notes/old.md"), ("created", "/notes/new.md")])
        self.assertEqual(
            hook.handle(sign(SECRET, "msg_5", NOW, body), body, now=NOW), 204
        )
        self.assertEqual(
            slack.messages, ["deleted /notes/old.md\ncreated /notes/new.md"]
        )

    def test_slack_failure_keeps_the_cursor_for_a_retry(self):
        slack = Recording(fail=True)
        hook = receiver.Receiver(SECRET, slack)
        body = changed("3", [("created", "/notes/report.md")])
        headers = sign(SECRET, "msg_3", NOW, body)
        self.assertEqual(hook.handle(headers, body, now=NOW), 500)
        self.assertEqual(hook.cursor, -1)
        slack.fail = False
        self.assertEqual(hook.handle(headers, body, now=NOW), 204)
        self.assertEqual(slack.messages, ["created /notes/report.md"])

    def test_long_bursts_are_capped_and_unsafe_paths_are_skipped(self):
        events = [("created", f"/notes/{index}.md") for index in range(10)]
        events.append(("created", "/notes/bad\nname.md"))
        text = receiver.slack_text(events)
        self.assertEqual(
            text,
            "\n".join(f"created /notes/{index}.md" for index in range(8))
            + "\nand 2 more",
        )

    def test_slack_url_must_be_the_incoming_webhook_host(self):
        good = "https://hooks.slack.com/services/T000/B000/secret"
        self.assertEqual(receiver.check_slack_url(good), good)
        for bad in (
            "http://hooks.slack.com/services/T000/B000/secret",
            "https://example.com/services/T000/B000/secret",
            "https://hooks.slack.com/services/",
            "https://user:pass@hooks.slack.com/services/T000/B000/secret",
        ):
            with self.assertRaises(ValueError):
                receiver.check_slack_url(bad)

    def test_http_post_reaches_the_handler(self):
        slack = Recording()
        hook = receiver.Receiver(SECRET, slack)
        server = receiver.HTTPServer(("127.0.0.1", 0), receiver._handler(hook))
        thread = threading.Thread(target=server.serve_forever, daemon=True)
        thread.start()
        body = changed("1", [("created", "/notes/report.md")])
        headers = sign(SECRET, "msg_http", int(time.time()), body)
        connection = None
        try:
            port = server.server_address[1]
            connection = http.client.HTTPConnection("127.0.0.1", port, timeout=5)
            connection.request(
                "POST",
                "/hooks/mountable",
                body=body,
                headers={**headers, "Content-Length": str(len(body))},
            )
            response = connection.getresponse()
            payload = response.read()
            connection.close()
            self.assertEqual(response.status, 204)
            self.assertEqual(payload, b"")
            self.assertEqual(slack.messages, ["created /notes/report.md"])
        finally:
            if connection is not None:
                connection.close()
            server.shutdown()
            server.server_close()
            thread.join(timeout=5)

    def test_local_check_prints_the_sample_line(self):
        from io import StringIO
        from contextlib import redirect_stdout

        output = StringIO()
        with redirect_stdout(output):
            receiver.run_check()
        self.assertEqual(output.getvalue(), "created /notes/report.md\n")


if __name__ == "__main__":
    unittest.main()
