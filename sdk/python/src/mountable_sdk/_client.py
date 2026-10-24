"""The Mountable files API, mount tickets and webhooks over HTTPS.

The behaviour is the contract in sdk/contract.md, shared with the TypeScript
SDK.
"""

from __future__ import annotations

import base64
import binascii
import hashlib
import hmac
import json
import os
import re
import secrets
import time
from collections.abc import Mapping
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
# Seconds a webhook's timestamp may be from now.
WEBHOOK_TOLERANCE = 300

Mode = Literal["ro", "rw"]
FileType = Literal["file", "directory", "symlink"]
ChangeType = Literal["created", "updated", "deleted"]
WebhookState = Literal["active", "disabled"]

# Every credential the API issues starts with "mtbl": API keys (mtbl_),
# session tokens (mtblst_), tickets (mtbltk_) and login tokens. Webhook
# signing secrets are whsec_ and standard base64.
_SECRET = re.compile(r"(mtbl[a-z]*)_[A-Za-z0-9_.-]+")
_WEBHOOK_SECRET = re.compile(r"whsec_[A-Za-z0-9+/=]+")


def _redact(text: str) -> str:
    return _WEBHOOK_SECRET.sub("whsec_[redacted]", _SECRET.sub(r"\1_[redacted]", text))


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
    # A credential: kept out of repr.
    ticket: str | None = field(repr=False)
    ticket_expires_at: datetime | None
    expires_at: datetime


@dataclass(frozen=True)
class Webhook:
    """A filesystem's webhook. ``secret`` is set only in the result of
    ``create_webhook`` and ``rotate_webhook_secret``, the only time the API
    shows it."""

    id: str
    filesystem_id: str
    url: str
    path_prefix: str
    state: WebhookState
    # failing or access_revoked
    disabled_reason: str | None
    created_by: str
    created_at: datetime
    last_success_at: datetime | None
    last_failure_at: datetime | None
    # timeout, connect_failed, tls_failed, blocked_address, redirect or
    # http_<status>
    last_failure: str | None
    failing_since: datetime | None
    # A credential: kept out of repr.
    secret: str | None = field(default=None, repr=False)


@dataclass(frozen=True)
class WebhookMessage:
    """A verified webhook delivery."""

    # The webhook-id header: the same on every retry of a message.
    id: str
    # files.changed, or files.resync (no events: list what you need again).
    type: str
    timestamp: datetime
    webhook_id: str
    filesystem_id: str
    events: list[Change]
    # The ordering key: apply a message only if its cursor, compared as an
    # integer, is greater than the last one applied.
    cursor: str


class Mountable:
    """A client authenticated with an API key.

    ``api_key`` defaults to ``MOUNTABLE_API_KEY``; ``base_url`` to
    ``MOUNTABLE_API_URL``, then https://mountable.io.
    """

    def __init__(self, api_key: str | None = None, base_url: str | None = None):
        key = api_key or os.environ.get("MOUNTABLE_API_KEY")
        if not key:
            raise MountableError(
                None, "unauthenticated", "pass api_key or set MOUNTABLE_API_KEY"
            )
        self._api_key = key
        url = base_url or os.environ.get("MOUNTABLE_API_URL") or DEFAULT_BASE_URL
        self.base_url = url.rstrip("/")
        self._http = httpx.Client()
        # The API key's organisation, which webhook paths name.
        self._org_id: str | None = None

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

    def create_webhook(self, fs_id: str, url: str, path_prefix: str = "/") -> Webhook:
        """Calls ``url`` with the filesystem's changes under ``path_prefix``.

        The result's ``secret`` signs the deliveries and is shown only now:
        store it where your endpoint reads it. The first delivery, a
        ``files.resync``, may arrive before this returns. Never retried: after
        an unknown outcome, look for the URL in ``list_webhooks`` first.
        """
        response = self._call(
            "POST",
            self._webhooks(fs_id),
            retry=False,
            body={"url": url, "path_prefix": path_prefix},
        )
        return _webhook(_json(response))

    def list_webhooks(self, fs_id: str) -> list[Webhook]:
        response = self._call("GET", self._webhooks(fs_id), retry=True)
        return [_webhook(raw) for raw in cast(list[dict[str, Any]], _json(response))]

    def delete_webhook(self, fs_id: str, webhook_id: str) -> None:
        self._call("DELETE", self._webhooks(fs_id, webhook_id), retry=False)

    def rotate_webhook_secret(self, fs_id: str, webhook_id: str) -> Webhook:
        """A new secret, shown only now. For 24 hours deliveries are signed
        with the previous one too; a second rotation in that time drops the
        oldest."""
        path = self._webhooks(fs_id, webhook_id) + ":rotate-secret"
        return _webhook(_json(self._call("POST", path, retry=False)))

    def enable_webhook(self, fs_id: str, webhook_id: str) -> Webhook:
        """Resumes a webhook disabled as ``failing``; one disabled as
        ``access_revoked`` raises ``access_revoked``."""
        path = self._webhooks(fs_id, webhook_id) + ":enable"
        return _webhook(_json(self._call("POST", path, retry=False)))

    def _webhooks(self, fs_id: str, webhook_id: str | None = None) -> str:
        """The path of a filesystem's webhooks, or of one. The first call
        looks up the API key's organisation."""
        for value in (fs_id, webhook_id):
            if value is not None:
                _id(value)  # refuse a bad ID before the lookup
        if self._org_id is None:
            response = self._call("GET", "/api/v1/me", retry=True)
            key = _json(response).get("api_key")
            org = (
                cast(dict[str, Any], key).get("org_id")
                if isinstance(key, dict)
                else None
            )
            if not isinstance(org, str) or not org:
                raise MountableError(
                    response.status_code,
                    "api_error",
                    "the API did not report the API key's organisation",
                )
            self._org_id = org
        path = f"/api/v1/orgs/{_id(self._org_id)}/filesystems/{_id(fs_id)}/webhooks"
        return path if webhook_id is None else f"{path}/{_id(webhook_id)}"

    def _call(
        self, method: str, path: str, *, retry: bool, body: Any = None
    ) -> httpx.Response:
        """Sends one request with the API key; with ``retry``, once more after
        a network failure."""
        try:
            response = self._send(method, path, self._api_key, json=body)
        except MountableError as error:
            if not retry or error.code != "network_error":
                raise
            response = self._send(method, path, self._api_key, json=body)
        if not response.is_success:
            raise _api_error(response)
        return response

    def _post_session(self, body: dict[str, Any]) -> dict[str, Any]:
        response = self._call("POST", "/api/v1/mount-sessions", retry=False, body=body)
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
        timeout: float | None = None,
        **kwargs: Any,
    ) -> httpx.Response:
        try:
            return self._http.request(
                method,
                self.base_url + path,
                headers={"Authorization": f"Bearer {bearer}", **(headers or {})},
                timeout=TIMEOUT if timeout is None else timeout,
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
            events=[_change(event) for event in body["events"]],
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
        """Sends one call. A 401 creates a new session and sends the call once
        more, once per call; a read is also sent once more after a network
        error."""
        refresh, network_retry = True, retry
        while True:
            session = None
            try:
                session = self._token()
                response = self._client._send(method, path, session.token, **kwargs)
            except MountableError as error:
                if not network_retry or error.code != "network_error":
                    raise
                network_retry = False
                continue
            if response.status_code == 401 and refresh:
                refresh = False
                if self._session is session:
                    self._session = None
                continue
            if not response.is_success:
                raise _api_error(response)
            return response


def _id(value: str) -> str:
    """One ID as a path segment. An empty, "." or ".." ID would name another
    resource once the URL is normalised, so it raises ``invalid_request``."""
    if value in ("", ".", ".."):
        raise MountableError(None, "invalid_request", f"invalid ID: {value!r}")
    return quote(value, safe="")


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


def _change(raw: dict[str, Any]) -> Change:
    return Change(
        type=raw["type"],
        path=raw["path"],
        file=_file_info(raw["file"]) if raw.get("file") else None,
    )


def _optional_time(value: str | None) -> datetime | None:
    return _parse_time(value) if value else None


def _webhook(raw: dict[str, Any]) -> Webhook:
    return Webhook(
        id=raw["id"],
        filesystem_id=raw["filesystem_id"],
        url=raw["url"],
        path_prefix=raw["path_prefix"],
        state=raw["state"],
        disabled_reason=raw.get("disabled_reason"),
        created_by=raw["created_by"],
        created_at=_parse_time(raw["created_at"]),
        last_success_at=_optional_time(raw.get("last_success_at")),
        last_failure_at=_optional_time(raw.get("last_failure_at")),
        last_failure=raw.get("last_failure"),
        failing_since=_optional_time(raw.get("failing_since")),
        secret=raw.get("secret"),
    )


def verify_webhook(
    secret: str, headers: Mapping[str, str], body: bytes | str
) -> WebhookMessage:
    """Checks a delivery's signature and returns its message.

    ``headers`` are the request's (names in any case) and ``body`` its exact
    bytes, not JSON parsed and encoded again. Raises ``MountableError`` with
    code ``invalid_webhook`` unless a ``v1`` signature in ``webhook-signature``
    is by ``secret`` (Standard Webhooks) and ``webhook-timestamp`` is within
    five minutes of now. A message can arrive more than once; see
    ``WebhookMessage.cursor``.
    """

    def invalid(reason: str) -> MountableError:
        return MountableError(None, "invalid_webhook", reason)

    lower = {name.lower(): value for name, value in headers.items()}
    msg_id = lower.get("webhook-id", "")
    timestamp = lower.get("webhook-timestamp", "")
    if not msg_id or not timestamp.isascii() or not timestamp.isdigit():
        raise invalid("webhook-id or webhook-timestamp is missing")
    # More digits than any timestamp would overflow, or exceed int's limit.
    if len(timestamp) > 12 or abs(time.time() - int(timestamp)) > WEBHOOK_TOLERANCE:
        raise invalid("webhook-timestamp is more than five minutes from now")
    try:
        key = base64.b64decode(secret.removeprefix("whsec_"), validate=True)
    except (ValueError, binascii.Error):
        key = b""
    if not secret.startswith("whsec_") or not key:
        raise invalid("the secret is not a whsec_ secret")
    data = body.encode() if isinstance(body, str) else body
    signed = f"{msg_id}.{timestamp}.".encode() + data
    expected = base64.b64encode(hmac.new(key, signed, hashlib.sha256).digest())
    # Signatures are separated by spaces; a repeated header is joined with ", ".
    if not any(
        hmac.compare_digest(signature[3:].removesuffix(",").encode(), expected)
        for signature in lower.get("webhook-signature", "").split()
        if signature.startswith("v1,")
    ):
        raise invalid("no signature matches")
    try:
        message = json.loads(data)
        payload = message["data"]
        fields = (
            message["type"],
            message["timestamp"],
            payload["webhook_id"],
            payload["filesystem_id"],
            payload["cursor"],
        )
        if not all(isinstance(value, str) for value in fields) or not isinstance(
            payload["events"], list
        ):
            raise TypeError
        return WebhookMessage(
            id=msg_id,
            type=message["type"],
            timestamp=_parse_time(message["timestamp"]),
            webhook_id=payload["webhook_id"],
            filesystem_id=payload["filesystem_id"],
            events=[_change(event) for event in payload["events"]],
            cursor=payload["cursor"],
        )
    except (ValueError, KeyError, TypeError, AttributeError):
        raise invalid("the body is not a mountable.io webhook message") from None


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
