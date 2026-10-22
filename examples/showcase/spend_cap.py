#!/usr/bin/env python3
"""Two end users. One hits a monthly spend cap. The other keeps working.

`--dry-run` prints an illustrated sequence and does not call Mountable.
A live run creates two filesystems through the REST API. User A's cap is
0 cents, so the first billable write is refused with
402 filesystem_spending_cap_reached. User B's cap is 500 cents. One small
file is written and read back. Delete both filesystems in the console when
you are done. The API cannot delete a filesystem.
"""

from __future__ import annotations

import argparse
import json
import os
import sys
import uuid
from pathlib import Path
from typing import Protocol
from urllib.parse import quote

import showcase
from rest import ApiError, Rest

API_KEY_ENV = showcase.API_KEY_ENV
CAP_CODE = "filesystem_spending_cap_reached"
USER_A_CAP = 0
USER_B_CAP = 500
ATTEMPTS = 3
TITLE = "One user's monthly cap stops their billable work."
SPEND_CONTRACT = (
    "Dry-run visualization. Playing it does not create a filesystem or call "
    "the Mountable API. User A has a monthly spend cap of 0 cents, so the "
    "first billable write stops with 402 filesystem_spending_cap_reached. "
    "Stored files stay stored. User B has a separate filesystem and keeps "
    "reading and writing."
)
SPEND_RECORDED = (
    "Recorded by examples/showcase/spend_cap.py against Mountable. "
    "User A's filesystem was created with a 0-cent monthly cap. The first "
    "billable write stopped with 402 filesystem_spending_cap_reached. "
    "User B's filesystem was created with a 500-cent cap. One small file "
    "was written and read back. This recording is not a billing measurement."
)


class CapReached(showcase.ShowcaseError):
    def __init__(self, status: int, code: str) -> None:
        self.status = status
        self.code = code
        super().__init__(f"HTTP {status} {code}")


class Filesystems(Protocol):
    def create(self, name: str, spending_cap_cents: int) -> str: ...

    def write(self, fs_id: str, path: str, body: bytes) -> None: ...

    def read(self, fs_id: str, path: str) -> bytes: ...

    def close(self) -> None: ...


def _line(t_ms: int, text: str) -> dict[str, object]:
    return {"t_ms": t_ms, "text": text}


def _pane(title: str, subtitle: str, lines: list[dict[str, object]]) -> dict[str, object]:
    return {"title": title, "subtitle": subtitle, "lines": lines}


def illustrative_spend_cap() -> dict[str, object]:
    refused = (
        'HTTP 402 {"detail":{"code":"filesystem_spending_cap_reached",'
        '"message":"the filesystem\'s monthly spending cap is reached"}}'
    )
    return {
        "id": "spend-cap",
        "title": TITLE,
        "source": "illustrative",
        "measured_abort_ms": None,
        "contract": SPEND_CONTRACT,
        "panes": [
            _pane(
                "App backend",
                "A monthly cap is set when each user's filesystem is created.",
                [
                    _line(0, "$ # two end users, two filesystems"),
                    _line(
                        600,
                        '$ # POST /api/v1/orgs/$ORG_ID/filesystems {"name":"user-a","spending_cap_cents":0}',
                    ),
                    _line(1200, '{"id":"fs_user_a","spending_cap_cents":0}'),
                    _line(
                        1800,
                        '$ # POST /api/v1/orgs/$ORG_ID/filesystems {"name":"user-b","spending_cap_cents":500}',
                    ),
                    _line(2400, '{"id":"fs_user_b","spending_cap_cents":500}'),
                ],
            ),
            _pane(
                "User A",
                "This user's billable writes stop at the cap.",
                [
                    _line(3000, "$ # the agent keeps writing notes"),
                    _line(3600, "$ curl -X PUT /api/v1/filesystems/fs_user_a/files/notes/loop-1.txt"),
                    _line(4200, refused),
                    _line(4800, "# the loop stops; stored files stay stored"),
                ],
            ),
            _pane(
                "User B",
                "A separate filesystem. This user keeps working.",
                [
                    _line(5400, "$ curl -X PUT /api/v1/filesystems/fs_user_b/files/notes/status.txt"),
                    _line(6000, "HTTP 201"),
                    _line(6600, "$ curl /api/v1/filesystems/fs_user_b/files/notes/status.txt"),
                    _line(7200, "working"),
                ],
            ),
        ],
    }


def _file_url(fs_id: str, path: str) -> str:
    segments = [quote(segment, safe="") for segment in path.strip("/").split("/") if segment]
    return "/api/v1/filesystems/" + quote(fs_id, safe="") + "/files/" + "/".join(segments)


def _explain(exc: ApiError) -> showcase.ShowcaseError:
    if exc.code == "payment_required":
        return showcase.ShowcaseError(
            "a saved payment method is required before a filesystem can have a spend cap"
        )
    if exc.code == "spending_cap_below_reserved":
        return showcase.ShowcaseError("the cap is below what this filesystem already reserves")
    return showcase.ShowcaseError(f"HTTP {exc.status} {exc.code}")


class LiveFilesystems:
    def __init__(self, rest: Rest, org_id: str) -> None:
        self.rest = rest
        self.org_id = org_id
        self.sessions: list[str] = []
        self.tokens: dict[str, str] = {}

    def create(self, name: str, spending_cap_cents: int) -> str:
        try:
            created = self.rest.json(
                "POST",
                "/api/v1/orgs/" + quote(self.org_id, safe="") + "/filesystems",
                {"name": name, "spending_cap_cents": spending_cap_cents},
            )
        except ApiError as exc:
            raise _explain(exc) from None
        fs_id = created.get("id")
        if not isinstance(fs_id, str) or not fs_id:
            raise showcase.ShowcaseError("filesystem create returned no id")
        print(
            f"showcase: created {fs_id} ({name}); delete it in the console when you are done.",
            file=sys.stderr,
        )
        return fs_id

    def _token(self, fs_id: str) -> str:
        cached = self.tokens.get(fs_id)
        if cached:
            return cached
        try:
            created = self.rest.json(
                "POST",
                "/api/v1/mount-sessions",
                {
                    "filesystem_id": fs_id,
                    "mode": "rw",
                    "max_duration_seconds": 600,
                    "idempotency_key": uuid.uuid4().hex,
                    "credential": "token",
                },
            )
        except ApiError as exc:
            raise _explain(exc) from None
        token = created.get("token")
        session_id = created.get("id")
        if not isinstance(token, str) or not token or not isinstance(session_id, str):
            raise showcase.ShowcaseError("token session returned no token")
        self.sessions.append(session_id)
        self.tokens[fs_id] = token
        return token

    def write(self, fs_id: str, path: str, body: bytes) -> None:
        try:
            self.rest.request(
                "PUT",
                _file_url(fs_id, path),
                data=body,
                bearer=self._token(fs_id),
                content_type="application/octet-stream",
            )
        except ApiError as exc:
            if exc.status == 402 and exc.code == CAP_CODE:
                raise CapReached(exc.status, exc.code) from None
            raise _explain(exc) from None

    def read(self, fs_id: str, path: str) -> bytes:
        try:
            _status, raw = self.rest.request("GET", _file_url(fs_id, path), bearer=self._token(fs_id))
        except ApiError as exc:
            if exc.status == 402 and exc.code == CAP_CODE:
                raise CapReached(exc.status, exc.code) from None
            raise _explain(exc) from None
        return raw

    def close(self) -> None:
        for session_id in self.sessions:
            try:
                self.rest.request("DELETE", "/api/v1/mount-sessions/" + quote(session_id, safe=""))
            except ApiError as exc:
                print(f"showcase: could not revoke session: HTTP {exc.status} {exc.code}", file=sys.stderr)
        self.sessions.clear()


def run_spend_cap(filesystems: Filesystems, secrets: list[str] | None = None) -> dict[str, object]:
    trace = showcase.Trace("spend-cap", TITLE, SPEND_RECORDED)
    trace.secrets.extend(secrets or [])
    trace.pane("backend", "App backend", "A monthly cap is set when each user's filesystem is created.")
    trace.pane("user-a", "User A", "This user's billable writes stop at the cap.")
    trace.pane("user-b", "User B", "A separate filesystem. This user keeps working.")
    suffix = uuid.uuid4().hex[:8]
    try:
        user_a = filesystems.create(f"showcase-user-a-{suffix}", USER_A_CAP)
        trace.say("backend", f"$ # create user-a spending_cap_cents {USER_A_CAP}")
        trace.say("backend", f"created {user_a}")
        user_b = filesystems.create(f"showcase-user-b-{suffix}", USER_B_CAP)
        trace.say("backend", f"$ # create user-b spending_cap_cents {USER_B_CAP}")
        trace.say("backend", f"created {user_b}")

        stopped = False
        for index in range(1, ATTEMPTS + 1):
            path = f"notes/loop-{index}.txt"
            trace.say("user-a", f"$ curl -X PUT /api/v1/filesystems/{user_a}/files/{path}")
            try:
                filesystems.write(user_a, path, b"note\n")
            except CapReached as exc:
                trace.say("user-a", f"HTTP {exc.status} {exc.code}")
                trace.say("user-a", "# the loop stops; stored files stay stored")
                stopped = True
                break
            trace.say("user-a", "HTTP 201")
        if not stopped:
            raise showcase.ShowcaseError(
                "user A was not stopped with 402 filesystem_spending_cap_reached"
            )

        trace.say("user-b", f"$ curl -X PUT /api/v1/filesystems/{user_b}/files/notes/status.txt")
        try:
            filesystems.write(user_b, "notes/status.txt", b"working\n")
        except CapReached as exc:
            raise showcase.ShowcaseError(
                f"user B was stopped too: HTTP {exc.status} {exc.code}"
            ) from None
        trace.say("user-b", "HTTP 201")
        body = filesystems.read(user_b, "notes/status.txt")
        if body != b"working\n":
            raise showcase.ShowcaseError("user B did not read the file back")
        trace.say("user-b", f"$ curl /api/v1/filesystems/{user_b}/files/notes/status.txt")
        trace.say("user-b", "working")
    finally:
        filesystems.close()
    return trace.body


def _emit(trace: dict[str, object], trace_path: Path | None) -> None:
    sys.stdout.write(showcase.format_script(trace))
    if trace_path is not None:
        trace_path.parent.mkdir(parents=True, exist_ok=True)
        trace_path.write_text(json.dumps(trace, indent=2) + "\n")


def _parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(description="Per-user monthly spend cap demo")
    parser.add_argument("--dry-run", action="store_true", help="print the illustrated sequence and call nothing")
    parser.add_argument(
        "--org",
        default=os.environ.get("MOUNTABLE_ORG_ID") or os.environ.get("MOUNTABLE_ORG"),
        help="organisation id (or set MOUNTABLE_ORG_ID)",
    )
    parser.add_argument("--trace", type=Path, help="write the trace JSON here")
    return parser


def main(argv: list[str] | None = None) -> int:
    args = _parser().parse_args(argv)
    try:
        if args.dry_run:
            _emit(illustrative_spend_cap(), args.trace)
            return 0
        api_key = os.environ.get(API_KEY_ENV, "")
        if not api_key:
            raise showcase.UsageError("MOUNTABLE_API_KEY is not set. Use --dry-run to preview without a filesystem.")
        if not args.org:
            raise showcase.UsageError("Pass --org ORG_ID or set MOUNTABLE_ORG_ID.")
        filesystems = LiveFilesystems(Rest(api_key), args.org)
        trace = run_spend_cap(filesystems, secrets=[api_key])
    except showcase.ShowcaseError as exc:
        print(f"showcase: {exc}", file=sys.stderr)
        return 2 if isinstance(exc, showcase.UsageError) else 1
    _emit(trace, args.trace)
    return 0


if __name__ == "__main__":
    sys.exit(main())
