"""Offline contract tests: real SDK, fake HTTP, no provider API calls."""

from __future__ import annotations

import base64
import hashlib
import hmac
import json
import unittest
from unittest.mock import patch

import httpx
import workspace

from mountable_sdk import Mountable, MountableError

SECRET = "whsec_" + base64.b64encode(b"test-signing-key").decode()
BODY = {
    "type": "files.changed",
    "data": {
        "filesystem_id": "fs_42",
        "webhook_id": "hook_42",
        "cursor": "42",
        "events": [
            {"type": "created", "path": "/output.txt", "file": {"etag": "a"}},
            {"type": "updated", "path": "/output.txt", "file": {"etag": "b"}},
            {"type": "deleted", "path": "/output.txt", "file": None},
            {"type": "created", "path": "/renamed.txt", "file": {"etag": "b"}},
        ],
    },
}


def delivery(message=BODY):
    body = json.dumps(message).encode()
    raw = b"msg_42.1000." + body
    signature = base64.b64encode(
        hmac.new(b"test-signing-key", raw, hashlib.sha256).digest()
    ).decode()
    return body, {
        "Webhook-Id": "msg_42",
        "Webhook-Timestamp": "1000",
        "Webhook-Signature": "v1," + signature,
    }


class WorkspaceTests(unittest.TestCase):
    def test_create_limits_atomically_and_register_webhook(self):
        requests = []

        def serve(request):
            requests.append(request)
            if request.url.path.endswith("/webhooks"):
                return httpx.Response(201, json={"id": "hook_42", "secret": SECRET})
            return httpx.Response(201, json={"id": "fs_42"})

        with httpx.Client(
            base_url="https://mountable.io",
            headers={"Authorization": "Bearer mtbl_backend"},
            transport=httpx.MockTransport(serve),
        ) as http:
            fs_id = workspace.create_workspace(http, "org/42", "user-42", 500)
            hook = workspace.register_webhook(
                http, "org/42", fs_id, "https://example.com/hooks/mountable"
            )
        self.assertEqual(fs_id, "fs_42")
        self.assertEqual(hook["secret"], SECRET)
        self.assertEqual(requests[0].url.raw_path, b"/api/v1/orgs/org%2F42/filesystems")
        self.assertEqual(
            json.loads(requests[0].content),
            {
                "name": "user-42",
                "spending_cap_cents": 500,
                "mutations_per_minute": 60,
                "byte_quota": 1073741824,
                "entry_quota": 10000,
            },
        )
        self.assertEqual(
            requests[1].url.raw_path,
            b"/api/v1/orgs/org%2F42/filesystems/fs_42/webhooks",
        )
        self.assertEqual(
            json.loads(requests[1].content),
            {"url": "https://example.com/hooks/mountable", "path_prefix": "/"},
        )

    def test_no_missing_or_invalid_cap_and_no_mutation_retries(self):
        for cap in (None, True, -1, 1.5, 1_000_000_001):
            with (
                self.subTest(cap=cap),
                httpx.Client() as http,
                self.assertRaises(ValueError),
            ):
                workspace.create_workspace(http, "org", "user", cap)
        calls = []

        def fail(request):
            calls.append(request)
            raise httpx.ReadTimeout("unknown outcome")

        with (
            httpx.Client(
                base_url="https://mountable.io", transport=httpx.MockTransport(fail)
            ) as http,
            self.assertRaises(httpx.ReadTimeout),
        ):
            workspace.create_workspace(http, "org", "user", 0)
        self.assertEqual(len(calls), 1)

    def test_real_sdk_reads_writes_and_propagates_caps_without_retry(self):
        calls, modes = [], []
        stored = b""
        capped = False

        def serve(request):
            nonlocal stored
            calls.append(request)
            if request.url.path == "/api/v1/mount-sessions":
                self.assertEqual(
                    request.headers["authorization"], "Bearer mtbl_backend"
                )
                body = json.loads(request.content)
                modes.append(body["mode"])
                self.assertEqual(body["filesystem_id"], "fs_42")
                self.assertEqual(body["credential"], "token")
                return httpx.Response(
                    201,
                    json={
                        "token": "mtblst_worker",
                        "expires_at": "2999-01-01T00:00:00Z",
                    },
                )
            self.assertEqual(request.headers["authorization"], "Bearer mtblst_worker")
            self.assertEqual(
                request.url.path, "/api/v1/filesystems/fs_42/files/output.txt"
            )
            if capped:
                return httpx.Response(
                    402,
                    json={
                        "detail": {
                            "code": "filesystem_spending_cap_reached",
                            "message": "cap reached",
                        }
                    },
                )
            if request.method == "PUT":
                stored = request.content
                return httpx.Response(
                    201,
                    json={
                        "path": "/output.txt",
                        "type": "file",
                        "size": len(stored),
                        "etag": '"abc"',
                        "modified_at": "2026-10-22T00:00:00Z",
                    },
                )
            return httpx.Response(
                200,
                content=stored,
                headers={
                    "Last-Modified": "Thu, 22 Oct 2026 00:00:00 GMT",
                    "ETag": '"abc"',
                },
            )

        with Mountable(api_key="mtbl_backend") as client:
            # The SDK has no transport injection; swap its client for offline HTTP.
            client._http.close()
            client._http = httpx.Client(transport=httpx.MockTransport(serve))
            fs = client.filesystem("fs_42", mode="rw")
            self.assertEqual(fs.write("/output.txt", "hello ✓").path, "/output.txt")
            self.assertEqual(fs.read("/output.txt").data.decode("utf-8"), "hello ✓")
            capped = True
            before = len(calls)
            with self.assertRaises(MountableError) as caught:
                fs.write("/output.txt", "too much")
            self.assertEqual(caught.exception.code, "filesystem_spending_cap_reached")
            self.assertEqual(caught.exception.status, 402)
            self.assertEqual(len(calls) - before, 1)  # one PUT; reuse session, no retry
        self.assertEqual(modes, ["rw"])

    def verify(self, body, headers, secret=SECRET):
        with patch("workspace.time.time", return_value=1000):
            return workspace.verify_webhook(
                secret, headers, body, filesystem_id="fs_42", webhook_id="hook_42"
            )

    def test_signature_case_insensitivity_rotation_and_resync(self):
        body, headers = delivery()
        headers["Webhook-Signature"] = (
            "v1,bad v2,ignored " + headers["Webhook-Signature"]
        )
        self.assertEqual(self.verify(body, headers), BODY)
        resync = {
            **BODY,
            "type": "files.resync",
            "data": {**BODY["data"], "events": []},
        }
        self.assertEqual(self.verify(*delivery(resync)), resync)

    def test_authenticated_unknown_types_are_returned_for_acknowledgement(self):
        for message in [
            {**BODY, "type": "files.future"},
            {
                **BODY,
                "data": {
                    **BODY["data"],
                    "events": [
                        {"type": "future", "path": "/output.txt"},
                    ],
                },
            },
        ]:
            self.assertEqual(self.verify(*delivery(message)), message)

    def test_reject_tampering_replays_wrong_routing_and_malformed_inputs(self):
        body, headers = delivery()
        cases = [
            (body + b" ", headers, SECRET),
            (body, {**headers, "Webhook-Id": "other"}, SECRET),
            (body, {**headers, "Webhook-Timestamp": "699"}, SECRET),
            (body, {**headers, "Webhook-Timestamp": "1301"}, SECRET),
            (body, {**headers, "Webhook-Timestamp": "n/a"}, SECRET),
            (body, {**headers, "Webhook-Signature": "v1,bad"}, SECRET),
            (body, {}, SECRET),
            (body, headers, "whsec_!"),
            (body, headers, "whsec_"),
        ]
        for changed in [
            {**BODY, "data": {**BODY["data"], "filesystem_id": "other"}},
            {**BODY, "data": {**BODY["data"], "webhook_id": "other"}},
            {**BODY, "data": {**BODY["data"], "cursor": "-1"}},
            [],
            {"data": None},
        ]:
            bad_body, bad_headers = delivery(changed)
            cases.append((bad_body, bad_headers, SECRET))
        for args in cases:
            with (
                self.subTest(args=args),
                self.assertRaisesRegex(ValueError, "^invalid Mountable webhook$"),
            ):
                self.verify(*args)


if __name__ == "__main__":
    unittest.main()
