"""The cases of sdk/contract.md, one test per case ID."""

from __future__ import annotations

import base64
import hashlib
import hmac
import json
import re
import time
from collections.abc import Iterator
from datetime import datetime, timedelta, timezone
from pathlib import Path
from typing import Any

import httpx
import pytest

from mountable_sdk import (
    Change,
    File,
    FileInfo,
    FilesystemInfo,
    FilesystemUsage,
    Mountable,
    MountableError,
    MountSession,
    Webhook,
    _client,
    verify_webhook,
)
from tests.mock_api import (
    API_KEY,
    CHANGES,
    FILES,
    FILESYSTEMS,
    FS,
    LIST,
    ME,
    SESSIONS,
    WEBHOOK_SECRET,
    WEBHOOKS,
    MockAPI,
    Reply,
    Request,
    error,
    file_info,
    filesystem_view,
    me,
    webhook_view,
)

MODIFIED = datetime(2026, 10, 17, 12, 0, 0, 123456, tzinfo=timezone.utc)
CREATED = datetime(2026, 10, 1, 9, 30, tzinfo=timezone.utc)


@pytest.fixture
def api() -> Iterator[MockAPI]:
    server = MockAPI()
    yield server
    server.close()


@pytest.fixture
def client(api: MockAPI, monkeypatch: pytest.MonkeyPatch) -> Iterator[Mountable]:
    monkeypatch.delenv("MOUNTABLE_ORG", raising=False)
    with Mountable(api_key=API_KEY, base_url=api.url) as c:
        yield c


def raises(fn: Any) -> MountableError:
    with pytest.raises(MountableError) as caught:
        fn()
    return caught.value


# --- operations ------------------------------------------------------------


def test_c01_write_creates_and_returns_file_info(api: MockAPI, client: Mountable):
    api.route(
        "PUT", f"{FILES}/tickets/AB-42.md", Reply(201, file_info("/tickets/AB-42.md"))
    )
    fs = client.filesystem(FS)
    info = fs.write("tickets/AB-42.md", b"abc")
    assert info == FileInfo("/tickets/AB-42.md", "file", 3, '"e1"', MODIFIED)
    fs.write("tickets/AB-42.md", "žluť")
    first, second = api.operations()
    assert first.method == "PUT" and first.body == b"abc"
    assert first.headers["content-type"] == "application/octet-stream"
    assert first.headers["content-length"] == "3"
    assert second.body == "žluť".encode()


def test_c02_write_replaces_and_returns_file_info(api: MockAPI, client: Mountable):
    api.route(
        "PUT", f"{FILES}/a.txt", Reply(200, file_info("/a.txt", etag='"e2"', size=5))
    )
    info = client.filesystem(FS).write("a.txt", b"hello")
    assert info.etag == '"e2"' and info.size == 5


def test_c03_read_returns_data_and_info_from_headers(api: MockAPI, client: Mountable):
    api.route(
        "GET",
        f"{FILES}/tickets/AB-42.md",
        Reply(
            200,
            b"abc",
            {
                "Content-Type": "application/octet-stream",
                "ETag": '"e1"',
                "Last-Modified": "Sat, 17 Oct 2026 12:00:00 GMT",
            },
        ),
    )
    file = client.filesystem(FS).read("tickets/AB-42.md")
    assert file == File(
        b"abc",
        FileInfo(
            "/tickets/AB-42.md",
            "file",
            3,
            '"e1"',
            datetime(2026, 10, 17, 12, tzinfo=timezone.utc),
        ),
    )
    assert api.operations()[0].method == "GET"


def test_c04_delete_returns_nothing(api: MockAPI, client: Mountable):
    api.route("DELETE", f"{FILES}/old.md", Reply(204))
    assert client.filesystem(FS).delete("old.md") is None
    assert api.calls()[-1] == f"DELETE {FILES}/old.md"
    assert "if-match" not in api.operations()[0].headers


def test_c05_list_pages_through_a_directory(api: MockAPI, client: Mountable):
    page = {
        "entries": [
            file_info("/tickets/a.md"),
            # The API omits etag for anything but a file.
            {
                k: v
                for k, v in file_info("/tickets/sub", type="directory", size=0).items()
                if k != "etag"
            },
        ],
        "next": "sub",
    }
    api.route("GET", f"{LIST}/tickets", Reply(200, page))
    api.route("GET", f"{LIST}/", Reply(200, {"entries": [], "next": None}))
    fs = client.filesystem(FS)
    result = fs.list("tickets", after="a", limit=2)
    assert result.next == "sub"
    assert result.entries[0] == FileInfo("/tickets/a.md", "file", 3, '"e1"', MODIFIED)
    assert result.entries[1].type == "directory" and result.entries[1].etag is None
    root = fs.list()
    assert root.entries == [] and root.next is None
    paged, last = api.operations()
    assert paged.query == {"after": "a", "limit": "2"}
    assert last.path == f"{LIST}/" and last.query == {}


def test_c06_changes_without_cursor_returns_a_cursor(api: MockAPI, client: Mountable):
    api.route("GET", CHANGES, Reply(200, {"events": [], "cursor": "c100"}))
    batch = client.filesystem(FS).changes()
    assert batch.events == [] and batch.cursor == "c100"
    assert api.operations()[0].query == {}


def test_c07_changes_with_cursor_returns_events(api: MockAPI, client: Mountable):
    events = [
        {"type": "created", "path": "/a.md", "file": file_info("/a.md")},
        {"type": "deleted", "path": "/b.md", "file": None},
    ]
    api.route("GET", CHANGES, Reply(200, {"events": events, "cursor": "c102"}))
    fs = client.filesystem(FS)
    batch = fs.changes(cursor="c100", wait=5)
    assert batch.cursor == "c102"
    assert batch.events[0].type == "created" and batch.events[0].path == "/a.md"
    assert batch.events[0].file == FileInfo("/a.md", "file", 3, '"e1"', MODIFIED)
    assert batch.events[1].type == "deleted" and batch.events[1].file is None
    fs.changes(cursor="c102")
    first, second = api.operations()
    assert first.query == {"cursor": "c100", "wait": "5"}
    assert second.query == {"cursor": "c102", "wait": "25"}


def test_c08_paths_are_encoded_per_segment(api: MockAPI, client: Mountable):
    api.route(
        "GET",
        f"{LIST}/a%20b/c%25d%23e%3Ff/%C5%BElu%C5%A5/%21%27%28%29%2A",
        Reply(200, {"entries": [], "next": None}),
    )
    fs = client.filesystem(FS)
    fs.list("/a b/c%d#e?f/žluť/!'()*/")
    assert (
        api.operations()[0].path
        == f"{LIST}/a%20b/c%25d%23e%3Ff/%C5%BElu%C5%A5/%21%27%28%29%2A"
    )
    for bad in ("a/../b", "./a", ".."):
        e = raises(lambda bad=bad: fs.read(bad))
        assert e.code == "bad_path" and e.status is None
    assert len(api.operations()) == 1


def test_c09_create_mount_ticket(api: MockAPI, client: Mountable):
    ticket = client.create_mount_ticket(
        FS, mode="ro", max_duration_seconds=30 * 86400, idempotency_key="job-42"
    )
    assert ticket.id == "s1" and ticket.ticket == "mtbltk_ticket1secret"
    assert ticket.mode == "ro" and ticket.state == "requested"
    assert ticket.filesystem_id == FS
    assert isinstance(ticket.expires_at, datetime) and ticket.ticket_expires_at
    request = api.sessions()[0]
    assert request.headers["authorization"] == f"Bearer {API_KEY}"
    assert request.json() == {
        "filesystem_id": FS,
        "mode": "ro",
        "idempotency_key": "job-42",
        "max_duration_seconds": 2592000,
    }
    client.create_mount_ticket(FS)
    body = api.sessions()[1].json()
    assert body["mode"] == "rw" and len(body["idempotency_key"]) >= 16
    assert "max_duration_seconds" not in body and "credential" not in body

    def replay(request: Request) -> Reply:
        reply = api.issue_session(request)
        assert isinstance(reply.body, dict)
        reply.body.update(ticket=None, state="active")
        return reply

    api.route("POST", SESSIONS, replay)
    assert client.create_mount_ticket(FS, idempotency_key="job-42").ticket is None


# --- conditional writes ----------------------------------------------------


def test_c10_write_if_match_sends_the_quoted_etag(api: MockAPI, client: Mountable):
    api.route("PUT", f"{FILES}/a.md", Reply(200, file_info("/a.md")))
    fs = client.filesystem(FS)
    fs.write("a.md", b"x", if_match='"e1"')
    fs.write("a.md", b"x", if_match="e1")
    assert [r.headers["if-match"] for r in api.operations()] == ['"e1"', '"e1"']


def test_c11_write_if_none_match_sends_a_star(api: MockAPI, client: Mountable):
    api.route("PUT", f"{FILES}/a.md", Reply(201, file_info("/a.md")))
    client.filesystem(FS).write("a.md", b"x", if_none_match=True)
    request = api.operations()[0]
    assert request.headers["if-none-match"] == "*" and "if-match" not in request.headers


def test_c12_delete_if_match_sends_the_etag(api: MockAPI, client: Mountable):
    api.route("DELETE", f"{FILES}/a.md", Reply(204))
    client.filesystem(FS).delete("a.md", if_match='"e1"')
    assert api.operations()[0].headers["if-match"] == '"e1"'


def test_c13_precondition_failed_carries_the_current_etag(
    api: MockAPI, client: Mountable
):
    api.route(
        "PUT", f"{FILES}/a.md", error(412, "precondition_failed", current_etag='"e9"')
    )
    api.route(
        "PUT", f"{FILES}/new.md", error(412, "precondition_failed", current_etag=None)
    )
    fs = client.filesystem(FS)
    e = raises(lambda: fs.write("a.md", b"x", if_match='"e1"'))
    assert (e.status, e.code, e.current_etag) == (412, "precondition_failed", '"e9"')
    e = raises(lambda: fs.write("new.md", b"x", if_match='"e1"'))
    assert e.code == "precondition_failed" and e.current_etag is None


# --- token sessions --------------------------------------------------------


def test_c14_first_operation_creates_a_token_session(api: MockAPI, client: Mountable):
    api.route("GET", f"{LIST}/", Reply(200, {"entries": [], "next": None}))
    fs = client.filesystem(FS, mode="ro")
    assert api.requests == []
    fs.list()
    session, operation = api.requests
    assert session.method == "POST" and session.path == SESSIONS
    assert session.headers["authorization"] == f"Bearer {API_KEY}"
    body = session.json()
    assert len(body.pop("idempotency_key")) >= 16
    assert body == {
        "filesystem_id": FS,
        "mode": "ro",
        "max_duration_seconds": 3600,
        "credential": "token",
    }
    assert operation.headers["authorization"] == "Bearer mtblst_token1secret"


def test_c15_the_session_is_reused(api: MockAPI, client: Mountable):
    api.route("GET", f"{LIST}/", Reply(200, {"entries": [], "next": None}))
    fs = client.filesystem(FS)
    fs.list()
    fs.list()
    fs.list()
    assert len(api.sessions()) == 1
    assert {r.headers["authorization"] for r in api.operations()} == {
        "Bearer mtblst_token1secret"
    }


def test_c16_a_401_refreshes_the_session_and_retries_once(
    api: MockAPI, client: Mountable
):
    def refuse(token: str, reply: Reply) -> Any:
        def handle(request: Request) -> Reply:
            if request.headers["authorization"] == f"Bearer {token}":
                return error(401, "unauthenticated")
            return reply

        return handle

    api.route("GET", f"{FILES}/a.md", refuse("mtblst_token1secret", READ_REPLY))
    api.route(
        "PUT",
        f"{FILES}/a.md",
        refuse("mtblst_token2secret", Reply(200, file_info("/a.md"))),
    )
    fs = client.filesystem(FS)
    assert fs.read("a.md").data == b"abc"
    assert fs.write("a.md", b"payload").path == "/a.md"
    assert len(api.sessions()) == 3
    assert [
        (r.method, r.headers["authorization"][-12:], r.body) for r in api.operations()
    ] == [
        ("GET", "token1secret", b""),
        ("GET", "token2secret", b""),
        ("PUT", "token2secret", b"payload"),
        ("PUT", "token3secret", b"payload"),
    ]


def test_c17_one_refresh_per_call(api: MockAPI, client: Mountable):
    api.route("GET", f"{FILES}/a.md", error(401, "unauthenticated"))
    e = raises(lambda: client.filesystem(FS).read("a.md"))
    assert (e.status, e.code) == (401, "unauthenticated")
    assert len(api.sessions()) == 2 and len(api.operations()) == 2

    # A network retry does not grant another refresh: 401, drop, 401 fails.
    answers = iter(
        [error(401, "unauthenticated"), Reply(0, fault="drop")]
        + [error(401, "unauthenticated"), READ_REPLY]
    )
    api.route("GET", f"{FILES}/b.md", lambda _: next(answers))
    e = raises(lambda: client.filesystem(FS).read("b.md"))
    assert (e.status, e.code) == (401, "unauthenticated")
    assert len(api.sessions()) == 4
    assert sum(r.path == f"{FILES}/b.md" for r in api.operations()) == 3


def test_c18_a_session_near_expiry_is_replaced(api: MockAPI, client: Mountable):
    api.route("GET", f"{LIST}/", Reply(200, {"entries": [], "next": None}))
    api.session_lifetime = timedelta(seconds=59)
    fs = client.filesystem(FS)
    fs.list()
    fs.list()
    assert len(api.sessions()) == 2
    assert [r.headers["authorization"] for r in api.operations()] == [
        "Bearer mtblst_token1secret",
        "Bearer mtblst_token2secret",
    ]
    api.session_lifetime = timedelta(seconds=120)
    fs.list()
    fs.list()
    assert len(api.sessions()) == 3


def test_c19_a_failed_session_creation_raises(api: MockAPI, client: Mountable):
    api.route("POST", SESSIONS, error(403, "no_grant"))
    e = raises(lambda: client.filesystem(FS).write("a.md", b"x"))
    assert (e.status, e.code) == (403, "no_grant")
    assert api.operations() == []


# --- errors ----------------------------------------------------------------

ERROR_CODES = [
    (400, "bad_path"),
    (401, "unauthenticated"),
    (403, "read_only"),
    (403, "token_needs_key_or_cli"),
    (404, "not_found"),
    (409, "is_a_directory"),
    (409, "not_a_regular_file"),
    (409, "not_a_directory"),
    (409, "directory_not_empty"),
    (409, "conflict"),
    (409, "quota_exceeded"),
    (410, "cursor_expired"),
    (412, "precondition_failed"),
    (413, "too_large"),
    (429, "rate_limited"),
    (429, "download_quota"),
    (501, "unsupported_file"),
    (503, "changes_unavailable"),
]


@pytest.mark.parametrize(("status", "code"), ERROR_CODES)
def test_c20_every_error_code_raises_mountable_error(
    api: MockAPI, client: Mountable, status: int, code: str
):
    api.route("PUT", f"{FILES}/nested.md", error(status, code, message="it failed"))
    api.route("PUT", f"{FILES}/flat.md", Reply(status, {"code": code}))
    fs = client.filesystem(FS)
    e = raises(lambda: fs.write("nested.md", b"x"))
    assert (e.status, e.code, e.message) == (status, code, "it failed")
    e = raises(lambda: fs.write("flat.md", b"x"))
    assert (e.status, e.code) == (status, code) and e.message


def test_c21_retry_after_is_read(api: MockAPI, client: Mountable):
    api.route(
        "PUT",
        f"{FILES}/a.md",
        Reply(429, {"detail": {"code": "rate_limited"}}, {"Retry-After": "7"}),
    )
    e = raises(lambda: client.filesystem(FS).write("a.md", b"x"))
    assert (e.code, e.retry_after) == ("rate_limited", 7)


def test_c22_network_failure_is_network_error(api: MockAPI, client: Mountable):
    api.route("PUT", f"{FILES}/a.md", Reply(0, fault="drop"))
    e = raises(lambda: client.filesystem(FS).write("a.md", b"x"))
    assert (e.status, e.code) == (None, "network_error")
    closed = Mountable(api_key=API_KEY, base_url="http://127.0.0.1:9")
    e = raises(lambda: closed.filesystem(FS).list())
    assert (e.status, e.code) == (None, "network_error")


def test_c23_secrets_never_appear_in_errors(api: MockAPI, client: Mountable):
    echo = f"refused {API_KEY} and mtblst_token1secret"
    api.route("GET", f"{FILES}/a.md", error(403, "read_only", message=echo))
    api.route(
        "POST",
        SESSIONS,
        lambda r: (
            api.issue_session(r)
            if r.json().get("credential")
            else error(401, "unauthenticated", message=echo)
        ),
    )
    fs = client.filesystem(FS)
    errors = [
        raises(lambda: fs.read("a.md")),
        raises(lambda: client.create_mount_ticket(FS)),
    ]
    for e in errors:
        for text in (
            str(e),
            repr(e),
            e.message,
            repr(fs),
            repr(fs._session),
            repr(client),
        ):
            assert API_KEY not in text and "token1secret" not in text
    api.route("POST", SESSIONS, api.issue_session)
    ticket = client.create_mount_ticket(FS)
    assert ticket.ticket and ticket.ticket.startswith("mtbltk_")
    for text in (str(ticket), repr(ticket)):
        assert "mtbltk_" not in text


def test_c24_errors_without_a_code(api: MockAPI, client: Mountable):
    api.route("PUT", f"{FILES}/a.md", Reply(502, "<html>bad gateway</html>"))
    invalid = {
        "detail": [{"loc": ["body", "max_duration_seconds"], "msg": "too large"}]
    }
    api.route(
        "POST",
        SESSIONS,
        lambda r: (
            api.issue_session(r) if r.json().get("credential") else Reply(422, invalid)
        ),
    )
    e = raises(lambda: client.filesystem(FS).write("a.md", b"x"))
    assert (e.status, e.code) == (502, "api_error") and e.message
    e = raises(lambda: client.create_mount_ticket(FS, max_duration_seconds=10**9))
    assert (e.status, e.code) == (422, "invalid_request")
    assert "max_duration_seconds" in e.message


# --- retries ---------------------------------------------------------------


def fail_first(n: int, fault: str, reply: Reply) -> Any:
    calls = {"n": 0}

    def handle(_: Request) -> Reply:
        calls["n"] += 1
        return Reply(0, b"x" * 10, fault=fault) if calls["n"] <= n else reply

    return handle


READ_REPLY = Reply(
    200, b"abc", {"ETag": '"e1"', "Last-Modified": "Sat, 17 Oct 2026 12:00:00 GMT"}
)


def test_c25_reads_are_retried_once_after_a_network_failure(
    api: MockAPI, client: Mountable
):
    fs = client.filesystem(FS)
    api.route("GET", f"{FILES}/a.md", fail_first(1, "drop", READ_REPLY))
    assert fs.read("a.md").data == b"abc"
    api.route("GET", f"{FILES}/short.md", fail_first(1, "short", READ_REPLY))
    assert fs.read("short.md").data == b"abc"
    # The fault is a body cut short after the headers arrived.
    api.route("GET", "/short", Reply(200, b"x" * 10, fault="short"))
    with httpx.stream("GET", api.url + "/short") as response:
        assert response.status_code == 200
        with pytest.raises(httpx.RemoteProtocolError):
            response.read()
    api.route(
        "GET",
        f"{LIST}/",
        fail_first(1, "drop", Reply(200, {"entries": [], "next": None})),
    )
    assert fs.list().entries == []
    api.route(
        "GET",
        CHANGES,
        fail_first(1, "drop", Reply(200, {"events": [], "cursor": "c1"})),
    )
    assert fs.changes(cursor="c0", wait=0).cursor == "c1"
    counts = {
        p: sum(r.path == p for r in api.operations())
        for p in (f"{FILES}/a.md", f"{FILES}/short.md", f"{LIST}/", CHANGES)
    }
    assert set(counts.values()) == {2}
    api.route("GET", f"{FILES}/b.md", fail_first(2, "drop", READ_REPLY))
    e = raises(lambda: fs.read("b.md"))
    assert e.code == "network_error"
    assert sum(r.path == f"{FILES}/b.md" for r in api.operations()) == 2


def test_c26_writes_and_deletes_are_never_retried(api: MockAPI, client: Mountable):
    fs = client.filesystem(FS)
    api.route(
        "PUT", f"{FILES}/a.md", fail_first(1, "drop", Reply(200, file_info("/a.md")))
    )
    api.route("DELETE", f"{FILES}/a.md", fail_first(1, "drop", Reply(204)))
    assert raises(lambda: fs.write("a.md", b"x")).code == "network_error"
    assert raises(lambda: fs.delete("a.md")).code == "network_error"
    assert [f"{r.method}" for r in api.operations()] == ["PUT", "DELETE"]


def test_c27_http_errors_are_not_retried(api: MockAPI, client: Mountable):
    api.route("GET", f"{FILES}/a.md", Reply(500, {"detail": {"code": "internal"}}))
    api.route("GET", f"{LIST}/", error(503, "retry"))
    fs = client.filesystem(FS)
    assert raises(lambda: fs.read("a.md")).status == 500
    assert raises(lambda: fs.list()).status == 503
    assert len(api.operations()) == 2


# --- change feed -----------------------------------------------------------


def test_c28_the_cursor_is_passed_on(api: MockAPI, client: Mountable):
    event: dict[str, Any] = {"type": "updated", "path": "/a", "file": file_info("/a")}
    pages: list[dict[str, Any]] = [
        {"events": [], "cursor": "c1"},
        {"events": [event], "cursor": "c2"},
        {"events": [], "cursor": "c2"},
    ]
    answers = iter(pages)
    api.route("GET", CHANGES, lambda _: Reply(200, next(answers)))
    fs = client.filesystem(FS)
    cursor = fs.changes().cursor
    cursor = fs.changes(cursor=cursor, wait=0).cursor
    assert fs.changes(cursor=cursor, wait=0).cursor == "c2"
    assert [r.query.get("cursor") for r in api.operations()] == [None, "c1", "c2"]


def test_c29_an_expired_cursor_is_recovered_by_relisting(
    api: MockAPI, client: Mountable
):
    def changes(request: Request) -> Reply:
        if request.query.get("cursor") == "old":
            return error(410, "cursor_expired")
        if "cursor" not in request.query:
            return Reply(200, {"events": [], "cursor": "fresh"})
        return Reply(200, {"events": [], "cursor": request.query["cursor"]})

    api.route("GET", CHANGES, changes)
    api.route(
        "GET", f"{LIST}/", Reply(200, {"entries": [file_info("/a")], "next": None})
    )
    fs = client.filesystem(FS)
    e = raises(lambda: fs.changes(cursor="old", wait=0))
    assert (e.status, e.code) == (410, "cursor_expired")
    cursor = fs.changes().cursor
    assert [x.path for x in fs.list().entries] == ["/a"]
    assert fs.changes(cursor=cursor, wait=0).cursor == "fresh"
    assert [f"{r.path}?{r.query.get('cursor')}" for r in api.operations()] == [
        f"{CHANGES}?old",
        f"{CHANGES}?None",
        f"{LIST}/?None",
        f"{CHANGES}?fresh",
    ]


# --- configuration ---------------------------------------------------------


def test_c30_configuration_defaults(api: MockAPI, monkeypatch: pytest.MonkeyPatch):
    monkeypatch.delenv("MOUNTABLE_API_KEY", raising=False)
    monkeypatch.delenv("MOUNTABLE_API_URL", raising=False)
    e = raises(lambda: Mountable())
    assert e.code == "unauthenticated" and e.status is None
    monkeypatch.setenv("MOUNTABLE_API_KEY", API_KEY)
    assert Mountable().base_url == "https://mountable.io"
    monkeypatch.setenv("MOUNTABLE_API_URL", api.url + "/")
    client = Mountable()
    assert client.base_url == api.url
    assert Mountable(base_url="https://example.test").base_url == "https://example.test"
    client.create_mount_ticket(FS)
    assert api.sessions()[0].headers["authorization"] == f"Bearer {API_KEY}"
    Mountable(api_key="mtbl_otherkeysecret").create_mount_ticket(FS)
    assert api.sessions()[1].headers["authorization"] == "Bearer mtbl_otherkeysecret"


def test_c31_timeouts(api: MockAPI, client: Mountable, monkeypatch: pytest.MonkeyPatch):
    assert _client.TIMEOUT == 60
    # Shortened: 0.1 s, and 1.1 s for changes with wait=1.
    monkeypatch.setattr(_client, "TIMEOUT", 0.1)
    fs = client.filesystem(FS)
    api.route("GET", f"{LIST}/", Reply(200, {"entries": [], "next": None}))
    fs.list()  # creates the session before anything is slow
    api.route("GET", f"{FILES}/slow.md", Reply(200, b"abc", delay=0.5))
    api.route("PUT", f"{FILES}/slow.md", Reply(200, file_info("/slow.md"), delay=0.5))
    api.route("GET", CHANGES, Reply(200, {"events": [], "cursor": "c"}, delay=0.5))
    e = raises(lambda: fs.read("slow.md"))
    assert (e.status, e.code) == (None, "network_error")
    e = raises(lambda: fs.write("slow.md", b"x"))
    assert (e.status, e.code) == (None, "network_error")
    assert fs.changes(cursor="c", wait=1).cursor == "c"
    e = raises(lambda: fs.changes(cursor="c", wait=0))
    assert (e.status, e.code) == (None, "network_error")
    calls = [f"{r.method} {r.path}" for r in api.operations()]
    assert calls.count(f"GET {FILES}/slow.md") == 2  # a read is retried once
    assert calls.count(f"PUT {FILES}/slow.md") == 1  # a write never
    assert calls.count(f"GET {CHANGES}") == 3  # one wait=1, two wait=0


def test_c32_a_redirect_is_an_error(api: MockAPI, client: Mountable):
    moved = {"Location": f"{api.url}{FILES}/elsewhere.md"}
    api.route("DELETE", f"{FILES}/a.md", Reply(302, b"", moved))
    api.route("GET", f"{FILES}/a.md", Reply(307, b"", moved))
    api.route(
        "POST",
        SESSIONS,
        lambda r: (
            api.issue_session(r)
            if r.json().get("credential")
            else Reply(303, b"", moved)
        ),
    )
    fs = client.filesystem(FS)
    e = raises(lambda: fs.delete("a.md"))
    assert (e.status, e.code) == (302, "api_error")
    assert raises(lambda: fs.read("a.md")).status == 307
    assert raises(lambda: client.create_mount_ticket(FS)).status == 303
    assert not any("elsewhere" in r.path for r in api.requests)


# --- management ------------------------------------------------------------


def test_c33_the_organisation_is_resolved_once(
    api: MockAPI, client: Mountable, monkeypatch: pytest.MonkeyPatch
):
    api.route("GET", FILESYSTEMS, Reply(200, []))
    client.list_filesystems()
    client.list_filesystems()
    assert api.calls() == [f"GET {ME}", f"GET {FILESYSTEMS}", f"GET {FILESYSTEMS}"]
    assert api.requests[0].headers["authorization"] == f"Bearer {API_KEY}"
    monkeypatch.setenv("MOUNTABLE_ORG", "org_env")
    api.route("GET", "/api/v1/orgs/org_env/filesystems", Reply(200, []))
    api.route("GET", "/api/v1/orgs/org_arg/filesystems", Reply(200, []))
    with Mountable(api_key=API_KEY, base_url=api.url) as c:
        c.list_filesystems()
    with Mountable(api_key=API_KEY, base_url=api.url, org_id="org_arg") as c:
        c.list_filesystems()
    assert api.calls()[3:] == [
        "GET /api/v1/orgs/org_env/filesystems",
        "GET /api/v1/orgs/org_arg/filesystems",
    ]


def test_c34_no_organisation_raises_org_required(api: MockAPI, client: Mountable):
    api.route("GET", ME, Reply(200, me(api_key=None)))
    e = raises(client.list_filesystems)
    assert (e.status, e.code) == (None, "org_required")
    assert api.calls() == [f"GET {ME}"]
    api.route("GET", ME, error(401, "unauthenticated"))
    e = raises(lambda: client.get_filesystem(FS))
    assert (e.status, e.code) == (401, "unauthenticated")


def test_c35_list_and_get_filesystems(api: MockAPI, client: Mountable):
    capped = filesystem_view(
        id="fs_two", spending_cap_cents=500, mutations_per_minute=60
    )
    api.route("GET", FILESYSTEMS, Reply(200, [filesystem_view(), capped]))
    api.route("GET", f"{FILESYSTEMS}/{FS}", Reply(200, filesystem_view()))
    first, second = client.list_filesystems()
    assert first == FilesystemInfo(FS, "notes", 2**30, 10_000, None, None, CREATED)
    assert (second.id, second.spending_cap_cents, second.mutations_per_minute) == (
        "fs_two",
        500,
        60,
    )
    assert client.get_filesystem(FS) == first
    assert api.calls()[1:] == [f"GET {FILESYSTEMS}", f"GET {FILESYSTEMS}/{FS}"]
    assert {r.headers["authorization"] for r in api.requests} == {f"Bearer {API_KEY}"}
    assert api.sessions() == []


def test_c36_create_filesystem_sends_only_the_limits_given(
    api: MockAPI, client: Mountable
):
    api.route("POST", FILESYSTEMS, lambda r: Reply(201, filesystem_view(**r.json())))
    assert client.create_filesystem("notes").name == "notes"
    info = client.create_filesystem("capped", spending_cap_cents=0, byte_quota=2**32)
    assert (info.spending_cap_cents, info.byte_quota) == (0, 2**32)
    client.create_filesystem(
        "all", spending_cap_cents=1, mutations_per_minute=2, byte_quota=3, entry_quota=4
    )
    assert [(r.method, r.json()) for r in api.operations()] == [
        ("POST", {"name": "notes"}),
        ("POST", {"name": "capped", "spending_cap_cents": 0, "byte_quota": 2**32}),
        (
            "POST",
            {
                "name": "all",
                "spending_cap_cents": 1,
                "mutations_per_minute": 2,
                "byte_quota": 3,
                "entry_quota": 4,
            },
        ),
    ]


def test_c37_update_filesystem_sends_only_the_fields_passed(
    api: MockAPI, client: Mountable
):
    api.route(
        "PATCH",
        f"{FILESYSTEMS}/{FS}",
        lambda r: Reply(200, filesystem_view(**r.json())),
    )
    info = client.update_filesystem(FS, spending_cap_cents=None, entry_quota=20_000)
    assert info.spending_cap_cents is None and info.entry_quota == 20_000
    client.update_filesystem(FS, mutations_per_minute=None)
    info = client.update_filesystem(
        FS, spending_cap_cents=1000, mutations_per_minute=120, byte_quota=2**31
    )
    assert info == FilesystemInfo(FS, "notes", 2**31, 10_000, 1000, 120, CREATED)
    assert [(r.method, r.json()) for r in api.operations()] == [
        ("PATCH", {"spending_cap_cents": None, "entry_quota": 20_000}),
        ("PATCH", {"mutations_per_minute": None}),
        (
            "PATCH",
            {
                "spending_cap_cents": 1000,
                "mutations_per_minute": 120,
                "byte_quota": 2**31,
            },
        ),
    ]


USAGE: dict[str, Any] = {
    "retained_bytes": 10,
    "byte_quota": 2**30,
    "entries": 2,
    "entry_quota": 10_000,
    "bytes_read_30d": 30,
    "bytes_written_30d": 40,
    "reserved_cents": 0,
    "currency": None,
}


def test_c38_filesystem_usage(api: MockAPI, client: Mountable):
    api.route("GET", f"{FILESYSTEMS}/{FS}/usage", Reply(200, USAGE))
    assert client.filesystem_usage(FS) == FilesystemUsage(
        10, 2**30, 2, 10_000, 30, 40, 0, None
    )
    assert api.calls()[-1] == f"GET {FILESYSTEMS}/{FS}/usage"


def test_c39_list_and_revoke_mount_sessions(api: MockAPI, client: Mountable):
    session = {
        "id": "s9",
        "filesystem_id": FS,
        "mode": "ro",
        "credential": "token",
        "state": "active",
        "created_by": "api key test",
        "expires_at": "2026-10-17T13:00:00Z",
        "created_at": "2026-10-17T12:00:00Z",
    }
    api.route("GET", f"{FILESYSTEMS}/{FS}/mount-sessions", Reply(200, [session]))
    api.route("DELETE", f"{SESSIONS}/s9", Reply(204))
    assert client.revoke_mount_session("s9") is None
    assert client.list_mount_sessions(FS) == [
        MountSession(
            "s9",
            FS,
            "ro",
            "token",
            "active",
            "api key test",
            datetime(2026, 10, 17, 13, tzinfo=timezone.utc),
            datetime(2026, 10, 17, 12, tzinfo=timezone.utc),
        )
    ]
    assert api.calls() == [
        f"DELETE {SESSIONS}/s9",
        f"GET {ME}",
        f"GET {FILESYSTEMS}/{FS}/mount-sessions",
    ]


def test_c40_create_and_list_webhooks(api: MockAPI, client: Mountable):
    api.route(
        "POST",
        WEBHOOKS,
        lambda r: Reply(201, webhook_view(secret=WEBHOOK_SECRET, **r.json())),
    )
    hook = client.create_webhook(FS, "https://example.test/hook")
    assert hook == Webhook(
        "wh_one",
        FS,
        "https://example.test/hook",
        "/",
        "active",
        None,
        "api key test",
        CREATED,
        None,
        None,
        None,
        None,
        WEBHOOK_SECRET,
    )
    hook = client.create_webhook(FS, "https://example.test/t", path_prefix="/tickets")
    assert hook.path_prefix == "/tickets"
    assert [r.json() for r in api.operations()] == [
        {"url": "https://example.test/hook"},
        {"url": "https://example.test/t", "path_prefix": "/tickets"},
    ]
    failing = webhook_view(
        id="wh_two",
        state="disabled",
        disabled_reason="failing",
        last_success_at="2026-10-02T00:00:00Z",
        last_failure_at="2026-10-03T00:00:00Z",
        last_failure="HTTP 500",
        failing_since="2026-10-02T12:00:00Z",
    )
    api.route("GET", WEBHOOKS, Reply(200, [webhook_view(), failing]))
    first, second = client.list_webhooks(FS)
    assert first.secret is None and first.last_failure_at is None
    assert (second.state, second.disabled_reason, second.last_failure) == (
        "disabled",
        "failing",
        "HTTP 500",
    )
    assert second.last_success_at == datetime(2026, 10, 2, tzinfo=timezone.utc)
    assert second.last_failure_at == datetime(2026, 10, 3, tzinfo=timezone.utc)
    assert second.failing_since == datetime(2026, 10, 2, 12, tzinfo=timezone.utc)
    assert second.secret is None


def test_c41_delete_rotate_and_enable_webhooks(api: MockAPI, client: Mountable):
    hook = f"{WEBHOOKS}/wh_one"
    rotated = "whsec_bmV3c2VjcmV0"
    api.route("DELETE", hook, Reply(204))
    api.route("POST", f"{hook}:rotate-secret", Reply(200, webhook_view(secret=rotated)))
    api.route("POST", f"{hook}:enable", Reply(200, webhook_view()))
    assert client.delete_webhook(FS, "wh_one") is None
    assert client.rotate_webhook_secret(FS, "wh_one").secret == rotated
    enabled = client.enable_webhook(FS, "wh_one")
    assert enabled.state == "active" and enabled.secret is None
    assert [f"{r.method} {r.path}" for r in api.operations()] == [
        f"DELETE {hook}",
        f"POST {hook}:rotate-secret",
        f"POST {hook}:enable",
    ]


def test_c42_ids_are_encoded_as_path_segments(api: MockAPI):
    fs = "/api/v1/orgs/org%2F1/filesystems/a%20b%2F%C5%BE"
    api.route("GET", fs, Reply(200, filesystem_view()))
    api.route("POST", f"{fs}/webhooks/wh%3F1:enable", Reply(200, webhook_view()))
    api.route("DELETE", f"{SESSIONS}/s%231", Reply(204))
    with Mountable(api_key=API_KEY, base_url=api.url, org_id="org/1") as c:
        c.get_filesystem("a b/ž")
        c.enable_webhook("a b/ž", "wh?1")
        c.revoke_mount_session("s#1")
    assert api.calls() == [
        f"GET {fs}",
        f"POST {fs}/webhooks/wh%3F1:enable",
        f"DELETE {SESSIONS}/s%231",
    ]


def test_c43_webhook_secrets_are_hidden(api: MockAPI, client: Mountable):
    api.route("POST", WEBHOOKS, Reply(201, webhook_view(secret=WEBHOOK_SECRET)))
    hook = client.create_webhook(FS, "https://example.test/hook")
    assert hook.secret == WEBHOOK_SECRET
    for text in (str(hook), repr(hook)):
        assert "whsec_" not in text
    echo = f"refused {WEBHOOK_SECRET}"
    api.route(
        "POST", f"{WEBHOOKS}/wh_one:rotate-secret", error(409, "conflict", message=echo)
    )
    e = raises(lambda: client.rotate_webhook_secret(FS, "wh_one"))
    assert e.code == "conflict"
    for text in (str(e), repr(e), e.message):
        assert WEBHOOK_SECRET[6:] not in text and "whsec_[redacted]" in text


def test_c44_repeatable_management_calls_are_retried_once(
    api: MockAPI, client: Mountable
):
    fs = f"{FILESYSTEMS}/{FS}"
    routes = [
        ("GET", ME, Reply(200, me())),
        ("GET", FILESYSTEMS, Reply(200, [])),
        ("GET", fs, Reply(200, filesystem_view())),
        ("GET", f"{fs}/usage", Reply(200, USAGE)),
        ("GET", f"{fs}/mount-sessions", Reply(200, [])),
        ("GET", WEBHOOKS, Reply(200, [])),
        ("DELETE", f"{SESSIONS}/s9", Reply(204)),
        ("POST", f"{WEBHOOKS}/wh_one:enable", Reply(200, webhook_view())),
    ]
    for method, path, reply in routes:
        api.route(method, path, fail_first(1, "drop", reply))
    client.list_filesystems()
    client.get_filesystem(FS)
    client.filesystem_usage(FS)
    client.list_mount_sessions(FS)
    client.list_webhooks(FS)
    client.revoke_mount_session("s9")
    client.enable_webhook(FS, "wh_one")
    calls = api.calls()
    assert [calls.count(f"{method} {path}") for method, path, _ in routes] == [2] * 8
    api.route("GET", FILESYSTEMS, fail_first(2, "drop", Reply(200, [])))
    assert raises(client.list_filesystems).code == "network_error"
    assert api.calls().count(f"GET {FILESYSTEMS}") == 4


def test_c45_management_writes_are_never_retried(api: MockAPI, client: Mountable):
    fs = f"{FILESYSTEMS}/{FS}"
    hook = f"{WEBHOOKS}/wh_one"
    routes = [
        ("POST", FILESYSTEMS, Reply(201, filesystem_view())),
        ("PATCH", fs, Reply(200, filesystem_view())),
        ("POST", WEBHOOKS, Reply(201, webhook_view(secret=WEBHOOK_SECRET))),
        ("POST", f"{hook}:rotate-secret", Reply(200, webhook_view(secret="whsec_x"))),
        ("DELETE", hook, Reply(204)),
    ]
    for method, path, reply in routes:
        api.route(method, path, fail_first(1, "drop", reply))
    calls: list[Any] = [
        lambda: client.create_filesystem("notes"),
        lambda: client.update_filesystem(FS, entry_quota=20_000),
        lambda: client.create_webhook(FS, "https://example.test/hook"),
        lambda: client.rotate_webhook_secret(FS, "wh_one"),
        lambda: client.delete_webhook(FS, "wh_one"),
    ]
    for call in calls:
        assert raises(call).code == "network_error"
    assert [f"{r.method} {r.path}" for r in api.operations()] == [
        f"{method} {path}" for method, path, _ in routes
    ]


def test_c46_management_errors_propagate(api: MockAPI, client: Mountable):
    fs = f"{FILESYSTEMS}/{FS}"
    invalid = {"detail": [{"loc": ["body", "spending_cap_cents"], "msg": "too low"}]}
    api.route("POST", FILESYSTEMS, error(403, "key_limited_to_filesystem"))
    api.route(
        "PATCH",
        fs,
        lambda r: (
            Reply(422, invalid)
            if r.json().get("spending_cap_cents") == -1
            else error(403, "key_limited_to_filesystem")
        ),
    )
    api.route("GET", WEBHOOKS, error(403, "forbidden", message="needs read-write"))
    api.route("DELETE", f"{WEBHOOKS}/wh_gone", error(404, "not_found"))
    e = raises(lambda: client.create_filesystem("notes"))
    assert (e.status, e.code) == (403, "key_limited_to_filesystem")
    e = raises(lambda: client.update_filesystem(FS, byte_quota=2**31))
    assert (e.status, e.code) == (403, "key_limited_to_filesystem")
    e = raises(lambda: client.update_filesystem(FS, spending_cap_cents=-1))
    assert (e.status, e.code) == (422, "invalid_request")
    assert "spending_cap_cents" in e.message
    e = raises(lambda: client.list_webhooks(FS))
    assert (e.status, e.code, e.message) == (403, "forbidden", "needs read-write")
    e = raises(lambda: client.delete_webhook(FS, "wh_gone"))
    assert (e.status, e.code) == (404, "not_found")


def test_c47_dot_segment_ids_are_refused(api: MockAPI):
    calls: list[Any] = []
    with Mountable(api_key=API_KEY, base_url=api.url) as client:
        for bad in ("", ".", ".."):
            calls += [
                lambda bad=bad: client.get_filesystem(bad),
                lambda bad=bad: client.update_filesystem(bad, byte_quota=1),
                lambda bad=bad: client.filesystem_usage(bad),
                lambda bad=bad: client.list_mount_sessions(bad),
                lambda bad=bad: client.revoke_mount_session(bad),
                lambda bad=bad: client.list_webhooks(bad),
                lambda bad=bad: client.create_webhook(bad, "https://x.example"),
                lambda bad=bad: client.delete_webhook(FS, bad),
                lambda bad=bad: client.rotate_webhook_secret(FS, bad),
                lambda bad=bad: client.enable_webhook(FS, bad),
            ]
        for call in calls:
            e = raises(call)
            assert (e.status, e.code) == (None, "invalid_request")
    with Mountable(api_key=API_KEY, base_url=api.url, org_id="..") as client:
        assert raises(lambda: client.list_webhooks(FS)).code == "invalid_request"
    assert api.requests == []  # nothing sent, not even the /me lookup


# --- webhook deliveries ----------------------------------------------------

HOOK = "33333333-3333-3333-3333-333333333333"
VECTOR: dict[str, Any] = json.loads(
    (Path(__file__).parents[2] / "testdata" / "webhook.json").read_text()
)


def signed_headers(signature: str = VECTOR["signature"]) -> dict[str, str]:
    return {
        "Webhook-Id": VECTOR["id"],
        "WEBHOOK-TIMESTAMP": str(VECTOR["timestamp"]),
        "webhook-signature": signature,
    }


@pytest.fixture
def at_vector_time(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setattr(time, "time", lambda: VECTOR["timestamp"] + 10)


@pytest.mark.usefixtures("at_vector_time")
def test_c48_verify_webhook_accepts_a_signed_delivery():
    message = verify_webhook(
        VECTOR["secret"], signed_headers(), VECTOR["body"].encode()
    )
    assert message.id == VECTOR["id"]
    assert message.type == "files.changed"
    assert message.timestamp == datetime(
        2021, 2, 25, 15, 2, 10, 123000, tzinfo=timezone.utc
    )
    assert message.webhook_id == HOOK and message.filesystem_id == FS
    assert message.events == [
        Change(
            "created",
            "/tickets/AB-42.md",
            FileInfo(
                "/tickets/AB-42.md",
                "file",
                1234,
                '"3f2a"',
                datetime(2021, 2, 25, 15, 2, 9, tzinfo=timezone.utc),
            ),
        ),
        Change("deleted", "/tickets/AB-41.md", None),
    ]
    assert message.cursor == "48213"
    # A string body, and a rotation's two signatures with ours second.
    other = "v1," + "A" * 43 + "="
    rotation = f"{other} {VECTOR['signature']}"
    again = verify_webhook(VECTOR["secret"], signed_headers(rotation), VECTOR["body"])
    assert again == message
    # Ours first, and a repeated header joined with ", ".
    for joined in (f"{VECTOR['signature']} {other}", f"{VECTOR['signature']}, {other}"):
        assert verify_webhook(VECTOR["secret"], signed_headers(joined), VECTOR["body"])


@pytest.mark.usefixtures("at_vector_time")
def test_c49_verify_webhook_refuses_anything_else(monkeypatch: pytest.MonkeyPatch):
    secret, body = VECTOR["secret"], VECTOR["body"]
    headers = signed_headers()
    missing = {k: v for k, v in headers.items() if k != "Webhook-Id"}
    unversioned = signed_headers(VECTOR["signature"].removeprefix("v1,"))
    cases: list[tuple[str, dict[str, str], str]] = [
        (secret, headers, body + " "),
        ("whsec_" + "A" * 43 + "=", headers, body),
        (secret, missing, body),
        (secret, unversioned, body),
        ("MfKQ9r8GKYqrTwjUPD8ILPZIo2LaLaSw", headers, body),
        ("whsec_not base64!", headers, body),
    ]
    for digits in (13, 400, 5000):  # overflows a float, then int's digit limit
        cases.append(
            (secret, dict(headers, **{"WEBHOOK-TIMESTAMP": "9" * digits}), body)
        )
    for case_secret, case_headers, case_body in cases:
        with pytest.raises(MountableError) as caught:
            verify_webhook(case_secret, case_headers, case_body)
        e = caught.value
        assert (e.status, e.code) == (None, "invalid_webhook")
        assert "MfKQ9r8" not in str(e) and "MfKQ9r8" not in repr(e)
    # Signed, but its events are not change events.
    malformed: list[list[Any]] = [
        [{}],
        [42],
        [{"type": 1, "path": "/a"}],
        [{"type": "x", "path": "/a", "file": [1]}],
    ]
    for events in malformed:
        message = json.loads(body)
        message["data"]["events"] = events
        signed_body = json.dumps(message)
        key = base64.b64decode(secret.removeprefix("whsec_"))
        content = f"{VECTOR['id']}.{VECTOR['timestamp']}.{signed_body}".encode()
        digest = base64.b64encode(hmac.new(key, content, hashlib.sha256).digest())
        signed = signed_headers("v1," + digest.decode())
        with pytest.raises(MountableError) as caught:
            verify_webhook(secret, signed, signed_body)
        assert caught.value.code == "invalid_webhook"
        assert "not a mountable.io webhook" in caught.value.message
    # The gateway's own signature of a body that is not a message: the
    # signature passes, the body does not.
    reference = dict(headers, **{"webhook-signature": VECTOR["reference_signature"]})
    e = raises(lambda: verify_webhook(secret, reference, VECTOR["reference_body"]))
    assert e.code == "invalid_webhook" and "not a mountable.io webhook" in e.message
    for offset, ok in ((-300, True), (300, True), (-301, False), (301, False)):
        stamp = VECTOR["timestamp"] + offset
        monkeypatch.setattr(time, "time", lambda stamp=stamp: stamp)
        if ok:
            assert verify_webhook(secret, headers, body).cursor == "48213"
        else:
            assert raises(lambda: verify_webhook(secret, headers, body)).code == (
                "invalid_webhook"
            )


# --- parity ----------------------------------------------------------------


def test_every_contract_case_has_a_test():
    contract = (Path(__file__).parents[2] / "contract.md").read_text()
    cases = set(re.findall(r"^\| (C\d\d) \|", contract, re.MULTILINE))
    tested = {name[5:8].upper() for name in globals() if re.match(r"test_c\d\d_", name)}
    assert cases and cases == tested
