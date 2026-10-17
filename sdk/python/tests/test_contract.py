"""The cases of sdk/contract.md, one test per case ID."""

from __future__ import annotations

import re
from collections.abc import Iterator
from datetime import datetime, timedelta, timezone
from pathlib import Path
from typing import Any

import httpx
import pytest

from mountable_sdk import File, FileInfo, Mountable, MountableError, _client
from tests.mock_api import (
    API_KEY,
    CHANGES,
    FILES,
    FS,
    LIST,
    SESSIONS,
    MockAPI,
    Reply,
    Request,
    error,
    file_info,
)

MODIFIED = datetime(2026, 10, 17, 12, 0, 0, 123456, tzinfo=timezone.utc)


@pytest.fixture
def api() -> Iterator[MockAPI]:
    server = MockAPI()
    yield server
    server.close()


@pytest.fixture
def client(api: MockAPI) -> Iterator[Mountable]:
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


# --- parity ----------------------------------------------------------------


def test_every_contract_case_has_a_test():
    contract = (Path(__file__).parents[2] / "contract.md").read_text()
    cases = set(re.findall(r"^\| (C\d\d) \|", contract, re.MULTILINE))
    tested = {name[5:8].upper() for name in globals() if re.match(r"test_c\d\d_", name)}
    assert cases and cases == tested
