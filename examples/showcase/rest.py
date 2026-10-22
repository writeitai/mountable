"""REST calls for the showcase live runs.

Stdlib only. Mutations are not retried. Redirects are not followed, so a
credential is not sent to another host.
"""

from __future__ import annotations

import http.client
import json
import re
from typing import Any
from urllib.parse import urlsplit

_SECRET = re.compile(r"mtbl[a-z]*_[A-Za-z0-9_.-]+|whsec_[A-Za-z0-9+/=_-]+")


class ApiError(Exception):
    def __init__(self, status: int, code: str, message: str = "") -> None:
        self.status = status
        self.code = code
        self.message = redact(message)
        super().__init__(self.message or f"HTTP {status} {code}")


def redact(text: str) -> str:
    return _SECRET.sub("[redacted]", text)


def _error(status: int, raw: bytes) -> ApiError:
    code, message = "request_failed", ""
    try:
        payload = json.loads(raw)
    except json.JSONDecodeError:
        payload = None
    detail = payload.get("detail", payload) if isinstance(payload, dict) else None
    if isinstance(detail, dict):
        found = detail.get("code")
        if isinstance(found, str) and found:
            code = found
        text = detail.get("message")
        if isinstance(text, str):
            message = text
    return ApiError(status, redact(code), redact(message))


class Rest:
    def __init__(self, api_key: str, base_url: str = "https://mountable.io") -> None:
        self.api_key = api_key
        self.base_url = base_url.rstrip("/")

    def request(
        self,
        method: str,
        path: str,
        *,
        json_body: dict[str, Any] | None = None,
        data: bytes | None = None,
        bearer: str | None = None,
        content_type: str | None = None,
    ) -> tuple[int, bytes]:
        if json_body is not None and data is not None:
            raise ValueError("pass json_body or data, not both")
        if not path.startswith("/"):
            raise ValueError("path must start with /")
        payload = data
        header_type = content_type
        if json_body is not None:
            payload = json.dumps(json_body).encode()
            header_type = "application/json"
        parts = urlsplit(self.base_url)
        if parts.scheme == "https":
            connection: http.client.HTTPConnection = http.client.HTTPSConnection(
                parts.hostname or "",
                parts.port or 443,
                timeout=30,
            )
        elif parts.scheme == "http":
            connection = http.client.HTTPConnection(parts.hostname or "", parts.port or 80, timeout=30)
        else:
            raise ApiError(0, "request_failed", "unsupported URL scheme")
        headers = {
            "Authorization": "Bearer " + (bearer or self.api_key),
            "User-Agent": "mountable-showcase",
        }
        if header_type is not None:
            headers["Content-Type"] = header_type
        target = (parts.path or "") + path
        try:
            connection.request(method, target, body=payload, headers=headers)
            response = connection.getresponse()
            raw = response.read()
            status = response.status
        except (OSError, http.client.HTTPException):
            raise ApiError(0, "network_error", "the API could not be reached") from None
        finally:
            connection.close()
        if status in (301, 302, 303, 307, 308):
            raise ApiError(status, "redirect", "refusing to follow a redirect")
        if not 200 <= status < 300:
            raise _error(status, raw)
        return status, raw

    def json(
        self,
        method: str,
        path: str,
        json_body: dict[str, Any] | None = None,
        *,
        bearer: str | None = None,
    ) -> dict[str, Any]:
        status, raw = self.request(method, path, json_body=json_body, bearer=bearer)
        if status == 204 or not raw:
            return {}
        try:
            parsed = json.loads(raw)
        except json.JSONDecodeError as exc:
            raise ApiError(status, "request_failed", "the API did not return JSON") from exc
        if not isinstance(parsed, dict):
            raise ApiError(status, "request_failed", "the API did not return an object")
        return parsed
