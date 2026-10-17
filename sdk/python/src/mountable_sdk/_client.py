"""The Mountable files API and mount tickets over HTTPS.

The behaviour is the contract in sdk/contract.md, shared with the TypeScript
SDK.
"""

from __future__ import annotations

import os
import re
import secrets
from dataclasses import dataclass, field
from datetime import datetime, timedelta, timezone
from email.utils import parsedate_to_datetime
from types import TracebackType
from typing import TYPE_CHECKING, Any, Literal, cast
from urllib.parse import quote

import httpx

if TYPE_CHECKING:
    from typing_extensions import Self

DEFAULT_BASE_URL = "https://mountable.io"
# Seconds a request may take; a change-feed poll may take its wait longer.
TIMEOUT = 60.0
# A token session lasts an hour and is replaced this close to its expiry.
SESSION_SECONDS = 3600
REFRESH_BEFORE = timedelta(seconds=60)

Mode = Literal["ro", "rw"]
FileType = Literal["file", "directory", "symlink"]
ChangeType = Literal["created", "updated", "deleted"]

# Every secret the API issues starts with "mtbl": API keys (mtbl_), session
# tokens (mtblst_), tickets (mtbltk_) and login tokens.
_SECRET = re.compile(r"(mtbl[a-z]*)_[A-Za-z0-9_.-]+")


def _redact(text: str) -> str:
    return _SECRET.sub(r"\1_[redacted]", text)


class MountableError(Exception):
    """A request failed: a non-2xx answer or a network failure.

    ``status`` is the HTTP status, ``None`` when no answer arrived. ``code``
    is the API's stable error code, or ``network_error``.
    """

    def __init__(
        self,
        status: int | None,
        code: str,
        message: str,
        *,
        current_etag: str | None = None,
        retry_after: int | None = None,
    ) -> None:
        self.status = status
        self.code = _redact(code)
        self.message = _redact(message)
        self.current_etag = current_etag
        self.retry_after = retry_after
        super().__init__(self.message)

    def __repr__(self) -> str:
        return (
            f"MountableError(status={self.status!r}, code={self.code!r}, "
            f"message={self.message!r})"
        )


@dataclass(frozen=True)
class FileInfo:
    path: str
    type: FileType
    size: int
    etag: str | None
    modified_at: datetime


@dataclass(frozen=True)
class File:
    data: bytes
    info: FileInfo


@dataclass(frozen=True)
class ListPage:
    entries: list[FileInfo]
    next: str | None


@dataclass(frozen=True)
class Change:
    type: ChangeType
    path: str
    file: FileInfo | None


@dataclass(frozen=True)
class ChangeBatch:
    events: list[Change]
    cursor: str


@dataclass(frozen=True)
class MountTicket:
    id: str
    filesystem_id: str
    mode: Mode
    state: str
    ticket: str | None
    ticket_expires_at: datetime | None
    expires_at: datetime


class Mountable:
    """A client authenticated with an API key.

    ``api_key`` defaults to ``MOUNTABLE_API_KEY``; ``base_url`` to
    ``MOUNTABLE_URL``, then https://mountable.io.
    """

    def __init__(self, api_key: str | None = None, base_url: str | None = None):
        key = api_key or os.environ.get("MOUNTABLE_API_KEY")
        if not key:
            raise MountableError(
                None, "unauthenticated", "pass api_key or set MOUNTABLE_API_KEY"
            )
        self._api_key = key
        url = base_url or os.environ.get("MOUNTABLE_URL") or DEFAULT_BASE_URL
        self.base_url = url.rstrip("/")
        self._http = httpx.Client(timeout=TIMEOUT)

    def __repr__(self) -> str:
        return f"Mountable(base_url={self.base_url!r})"

    def __enter__(self) -> Self:
        return self

    def __exit__(
        self,
        exc_type: type[BaseException] | None,
        exc: BaseException | None,
        traceback: TracebackType | None,
    ) -> None:
        self.close()

    def close(self) -> None:
        self._http.close()

    def filesystem(self, id: str, mode: Mode = "rw") -> Filesystem:
        """A handle on one filesystem. It sends nothing until it is used."""
        return Filesystem(self, id, mode)

    def create_mount_ticket(
        self,
        fs_id: str,
        mode: Mode = "rw",
        max_duration_seconds: int | None = None,
        idempotency_key: str | None = None,
    ) -> MountTicket:
        """A one-time ticket for ``mountable mount --ticket-stdin``.

        Pass your own ``idempotency_key`` and reuse it to retry after an
        unknown outcome. A replay returns the existing session with ``ticket``
        ``None``.
        """
        body: dict[str, Any] = {
            "filesystem_id": fs_id,
            "mode": mode,
            "idempotency_key": idempotency_key or secrets.token_hex(16),
        }
        if max_duration_seconds is not None:
            body["max_duration_seconds"] = max_duration_seconds
        session = self._post_session(body)
        ticket_expires_at = session.get("ticket_expires_at")
        return MountTicket(
            id=session["id"],
            filesystem_id=session["filesystem_id"],
            mode=session["mode"],
            state=session["state"],
            ticket=session.get("ticket"),
            ticket_expires_at=_parse_time(ticket_expires_at)
            if ticket_expires_at
            else None,
            expires_at=_parse_time(session["expires_at"]),
        )

    def _post_session(self, body: dict[str, Any]) -> dict[str, Any]:
        response = self._send(
            "POST", "/api/v1/mount-sessions", self._api_key, json=body
        )
        if response.is_error:
            raise _api_error(response)
        return _json(response)

    def _create_token_session(self, fs_id: str, mode: Mode) -> _Session:
        session = self._post_session(
            {
                "filesystem_id": fs_id,
                "mode": mode,
                "max_duration_seconds": SESSION_SECONDS,
                "idempotency_key": secrets.token_hex(16),
                "credential": "token",
            }
        )
        token = session.get("token")
        if not isinstance(token, str):
            raise MountableError(201, "api_error", "the API returned no session token")
        return _Session(token, _parse_time(session["expires_at"]))

    def _send(
        self,
        method: str,
        path: str,
        bearer: str,
        *,
        headers: dict[str, str] | None = None,
        timeout: float = TIMEOUT,
        **kwargs: Any,
    ) -> httpx.Response:
        try:
            return self._http.request(
                method,
                self.base_url + path,
                headers={"Authorization": f"Bearer {bearer}", **(headers or {})},
                timeout=timeout,
                **kwargs,
            )
        except httpx.TransportError as error:
            raise MountableError(
                None, "network_error", f"the API could not be reached: {error}"
            ) from None


@dataclass
class _Session:
    token: str = field(repr=False)
    expires_at: datetime


class Filesystem:
    """One filesystem, used through a token session of its mode.

    The first operation creates the session; every later one reuses it. A
    ``401`` or a session within 60 seconds of its expiry makes the handle
    create a new one, and a refused call is sent once more.
    """

    def __init__(self, client: Mountable, id: str, mode: Mode):
        self._client = client
        self.id = id
        self.mode: Mode = mode
        self._session: _Session | None = None

    def __repr__(self) -> str:
        return f"Filesystem(id={self.id!r}, mode={self.mode!r})"

    def write(
        self,
        path: str,
        data: bytes | str,
        *,
        if_match: str | None = None,
        if_none_match: bool = False,
    ) -> FileInfo:
        """Replace or create the whole file. Never retried by the SDK."""
        headers = {"Content-Type": "application/octet-stream"}
        if if_match is not None:
            headers["If-Match"] = _quote_etag(if_match)
        if if_none_match:
            headers["If-None-Match"] = "*"
        content = data.encode() if isinstance(data, str) else data
        response = self._request(
            "PUT",
            self._url("files", path),
            retry=False,
            content=content,
            headers=headers,
        )
        return _file_info(_json(response))

    def read(self, path: str) -> File:
        response = self._request("GET", self._url("files", path), retry=True)
        data = response.content
        etag = response.headers.get("ETag")
        length = response.headers.get("Content-Length")
        modified = response.headers.get("Last-Modified")
        if modified is None:
            raise MountableError(
                response.status_code, "api_error", "the API sent no Last-Modified"
            )
        info = FileInfo(
            path="/" + path.strip("/"),
            type="file",
            size=int(length) if length is not None else len(data),
            etag=etag,
            modified_at=parsedate_to_datetime(modified).astimezone(timezone.utc),
        )
        return File(data=data, info=info)

    def delete(self, path: str, *, if_match: str | None = None) -> None:
        """Delete a file, symlink or empty directory. Never retried."""
        headers = {} if if_match is None else {"If-Match": _quote_etag(if_match)}
        self._request("DELETE", self._url("files", path), retry=False, headers=headers)

    def list(
        self, path: str = "", *, after: str | None = None, limit: int | None = None
    ) -> ListPage:
        params: dict[str, str | int] = {}
        if after is not None:
            params["after"] = after
        if limit is not None:
            params["limit"] = limit
        response = self._request(
            "GET", self._url("list", path), retry=True, params=params
        )
        body = _json(response)
        return ListPage(
            entries=[_file_info(entry) for entry in body["entries"]],
            next=body.get("next"),
        )

    def changes(self, cursor: str | None = None, wait: int = 25) -> ChangeBatch:
        """Changes after ``cursor``, waiting up to ``wait`` seconds for one.

        Without a cursor, returns no events and the current cursor at once.
        To stay in sync: take a cursor first, then list, then follow changes
        from it. ``cursor_expired`` (410) means: do that again.
        """
        params: dict[str, str | int] = {}
        if cursor is not None:
            params = {"cursor": cursor, "wait": wait}
        response = self._request(
            "GET",
            f"/api/v1/filesystems/{quote(self.id, safe='')}/changes",
            retry=True,
            params=params,
            timeout=TIMEOUT + wait,
        )
        body = _json(response)
        return ChangeBatch(
            events=[
                Change(
                    type=event["type"],
                    path=event["path"],
                    file=_file_info(event["file"]) if event.get("file") else None,
                )
                for event in body["events"]
            ],
            cursor=body["cursor"],
        )

    def _url(self, kind: str, path: str) -> str:
        segments = path.strip("/").split("/") if path.strip("/") else []
        if any(segment in (".", "..") for segment in segments):
            # URL normalisation would resolve them before the API sees them.
            raise MountableError(None, "bad_path", f"invalid path: {path!r}")
        encoded = "/".join(quote(segment, safe="") for segment in segments)
        return f"/api/v1/filesystems/{quote(self.id, safe='')}/{kind}/{encoded}"

    def _token(self) -> _Session:
        session = self._session
        if session is None or session.expires_at - _now() <= REFRESH_BEFORE:
            session = self._client._create_token_session(self.id, self.mode)
            self._session = session
        return session

    def _request(
        self, method: str, path: str, *, retry: bool, **kwargs: Any
    ) -> httpx.Response:
        """Sends one call; a read is sent once more after a network error."""
        try:
            return self._authorized(method, path, **kwargs)
        except MountableError as error:
            if not retry or error.code != "network_error":
                raise
        return self._authorized(method, path, **kwargs)

    def _authorized(self, method: str, path: str, **kwargs: Any) -> httpx.Response:
        session = self._token()
        response = self._client._send(method, path, session.token, **kwargs)
        if response.status_code == 401:
            if self._session is session:
                self._session = None
            response = self._client._send(method, path, self._token().token, **kwargs)
        if response.is_error:
            raise _api_error(response)
        return response


def _now() -> datetime:
    return datetime.now(timezone.utc)


def _quote_etag(etag: str) -> str:
    if etag.startswith(('"', "W/")):
        return etag
    return f'"{etag}"'


_FRACTION = re.compile(r"\.(\d+)")


def _parse_time(value: str) -> datetime:
    """Parses RFC 3339, which Python 3.10's fromisoformat only partly reads."""
    value = value.replace("Z", "+00:00").replace("z", "+00:00")
    value = _FRACTION.sub(lambda m: "." + m.group(1)[:6].ljust(6, "0"), value, 1)
    parsed = datetime.fromisoformat(value)
    if parsed.tzinfo is None:
        parsed = parsed.replace(tzinfo=timezone.utc)
    return parsed


def _file_info(raw: dict[str, Any]) -> FileInfo:
    return FileInfo(
        path=raw["path"],
        type=raw["type"],
        size=raw["size"],
        etag=raw.get("etag"),
        modified_at=_parse_time(raw["modified_at"]),
    )


def _json(response: httpx.Response) -> dict[str, Any]:
    try:
        return cast(dict[str, Any], response.json())
    except ValueError:
        raise MountableError(
            response.status_code, "api_error", "the API's answer is not JSON"
        ) from None


def _api_error(response: httpx.Response) -> MountableError:
    """Reads the API's error forms: {"detail": {"code": …}}, the same fields
    at the top level, and {"detail": [validation errors]}."""
    status = response.status_code
    try:
        body: Any = response.json()
    except ValueError:
        body = None
    fields: dict[str, Any] = {}
    if isinstance(body, dict):
        body = cast(dict[str, Any], body)
        detail = body.get("detail")
        fields = cast(dict[str, Any], detail) if isinstance(detail, dict) else body
    code = fields.get("code")
    message = fields.get("message")
    if status == 422 and isinstance(body, dict):
        invalid = cast(dict[str, Any], body).get("detail")
        if isinstance(invalid, list) and invalid:
            first = cast(dict[str, Any], cast(list[Any], invalid)[0])
            code = "invalid_request"
            location = ".".join(str(part) for part in first.get("loc", []))
            message = f"{location}: {first.get('msg', '')}"
    if not isinstance(code, str) or not code:
        code = "api_error"
    if not isinstance(message, str) or not message:
        message = f"the API answered HTTP {status} ({code})"
    current_etag = fields.get("current_etag")
    return MountableError(
        status,
        code,
        message,
        current_etag=current_etag if isinstance(current_etag, str) else None,
        retry_after=_retry_after(response, fields),
    )


def _retry_after(response: httpx.Response, fields: dict[str, Any]) -> int | None:
    header = response.headers.get("Retry-After")
    if header is not None and header.strip().isdigit():
        return int(header)
    value = fields.get("retry_after")
    return value if isinstance(value, int) else None
