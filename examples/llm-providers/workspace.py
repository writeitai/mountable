"""Backend helpers shared by all four provider starters.

Install mountable-sdk==0.1.0. Provisioning and webhook registration use REST;
the SDK handles file access. Keep these clients and webhook secrets in your
backend, outside model messages and sandbox environments.
"""

from __future__ import annotations

import base64
import binascii
import hashlib
import hmac
import json
import time
from collections.abc import Mapping
from urllib.parse import quote

import httpx


def filesystem_path(org_id: str, filesystem_id: str | None = None) -> str:
    path = f"/api/v1/orgs/{quote(org_id, safe='')}/filesystems"
    return path if filesystem_id is None else path + "/" + quote(filesystem_id, safe="")


def create_workspace(
    http: httpx.Client, org_id: str, name: str, spending_cap_cents: int
) -> str:
    """Create one end user's filesystem with limits in the same request.

    http is authenticated with an unrestricted owner/admin API key. Cents are
    in the organisation's billing currency. No retry: a timeout is ambiguous.
    """
    if (
        type(spending_cap_cents) is not int
        or not 0 <= spending_cap_cents <= 1_000_000_000
    ):
        raise ValueError("spending_cap_cents must be an integer from 0 to 1000000000")
    response = http.post(
        filesystem_path(org_id),
        json={
            "name": name,
            "spending_cap_cents": spending_cap_cents,
            "mutations_per_minute": 60,
            "byte_quota": 1073741824,
            "entry_quota": 10000,
        },
    )
    response.raise_for_status()
    return response.json()["id"]


def register_webhook(
    http: httpx.Client, org_id: str, filesystem_id: str, url: str
) -> dict:
    """Return the registration, including its one-time secret; never log it.

    Start your public HTTPS receiver before calling this. Its initial
    files.resync may arrive before this call returns; respond 503 until your
    endpoint has stored the secret and registration ID, so Mountable retries.
    """
    response = http.post(
        filesystem_path(org_id, filesystem_id) + "/webhooks",
        json={"url": url, "path_prefix": "/"},
    )
    response.raise_for_status()
    return response.json()


def verify_webhook(
    secret: str,
    headers: Mapping[str, str],
    body: bytes,
    *,
    filesystem_id: str,
    webhook_id: str,
) -> dict:
    """Authenticate raw bytes before parsing; reject wrong or stale deliveries.

    Header names are case insensitive. Accept either signature during secret
    rotation. The caller must persist cursor/deduplication state; see README.
    """
    try:
        headers = {k.lower(): v for k, v in headers.items()}
        msg_id = headers["webhook-id"]
        stamp = headers["webhook-timestamp"]
        if not msg_id or not stamp.isascii() or not stamp.isdigit():
            raise ValueError
        if abs(time.time() - int(stamp)) > 300:
            raise ValueError
        key = base64.b64decode(secret.removeprefix("whsec_"), validate=True)
        if not key:
            raise ValueError
        signed = f"{msg_id}.{stamp}.".encode() + body
        expected = base64.b64encode(hmac.new(key, signed, hashlib.sha256).digest())
        if not any(
            hmac.compare_digest(signature[3:].encode(), expected)
            for signature in headers["webhook-signature"].split()
            if signature.startswith("v1,")
        ):
            raise ValueError
        message = json.loads(body)
        data = message["data"]
        if (
            data["filesystem_id"] != filesystem_id
            or data["webhook_id"] != webhook_id
            or not isinstance(data["cursor"], str)
            or not data["cursor"].isascii()
            or not data["cursor"].isdigit()
        ):
            raise ValueError
        return message
    except (KeyError, TypeError, ValueError, AttributeError, binascii.Error):
        raise ValueError("invalid Mountable webhook") from None
