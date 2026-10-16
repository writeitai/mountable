"""Mint a Mountable ticket on the orchestrator and describe the sandbox mount.

The orchestrator process holds MOUNTABLE_API_KEY. The sandbox process receives
the one-time ticket on stdin and does not receive the API key.
"""

from __future__ import annotations

import json
import os
import subprocess
from collections.abc import Callable, Mapping
from dataclasses import dataclass

API_KEY_ENV = "MOUNTABLE_API_KEY"
# In the sandbox, run `mkdir -p /mnt/work` before starting this argv. Start it
# as a child and leave it running. It renews the session certificate until
# unmount, revoke, or expiry. Read stdout for the mounted event before using
# the directory.
MOUNT_ARGV = ("mountable", "mount", "--ticket-stdin", "--json", "/mnt/work")

DESCRIPTION = (
    "Create a one-time Mountable mount ticket for one sandbox. "
    "Runs on the orchestrator, which holds MOUNTABLE_API_KEY. "
    "In the sandbox, run mkdir -p /mnt/work before starting sandbox.argv. "
    "Give the sandbox only sandbox.stdin, on mountable mount --ticket-stdin. "
    "Leave that process running and wait for the mounted event. "
    "Never send MOUNTABLE_API_KEY into the sandbox. "
    "Keep session_id and revoke with mountable sessions revoke."
)

PARAMETERS = {
    "type": "object",
    "properties": {
        "filesystem_id": {
            "type": "string",
            "description": "Filesystem ID to mount.",
        },
        "idempotency_key": {
            "type": "string",
            "description": (
                "Key for this mount, such as the job ID. "
                "Reuse it only to retry this ticket create."
            ),
        },
        "read_only": {
            "type": "boolean",
            "description": "True for a read-only mount.",
        },
    },
    "required": ["filesystem_id", "idempotency_key", "read_only"],
    "additionalProperties": False,
}

Runner = Callable[[list[str], dict[str, str]], tuple[int, str, str]]


class TicketError(Exception):
    def __init__(
        self,
        code: str,
        message: str,
        *,
        hint: str = "",
        idempotency_key: str = "",
        retry_after: int = 0,
    ) -> None:
        super().__init__(message)
        self.code = code
        self.message = message
        self.hint = hint
        self.idempotency_key = idempotency_key
        self.retry_after = retry_after

    def __str__(self) -> str:
        parts = [f"{self.code}: {self.message}"]
        if self.hint:
            parts.append(f"hint: {self.hint}")
        if self.retry_after:
            parts.append(f"retry_after: {self.retry_after}")
        if self.idempotency_key:
            parts.append(f"idempotency_key: {self.idempotency_key}")
        return "; ".join(parts)


@dataclass(frozen=True)
class PreparedMount:
    session_id: str
    argv: tuple[str, ...]
    stdin: str
    env: dict[str, str]

    def tool_result(self) -> dict[str, object]:
        """Fields the model may see. The API key is not among them."""
        return {
            "session_id": self.session_id,
            "sandbox": {"argv": list(self.argv), "stdin": self.stdin},
        }


def ticket_create_argv(filesystem_id: str, idempotency_key: str, read_only: bool) -> list[str]:
    argv = [
        "mountable",
        "ticket",
        "create",
        filesystem_id,
        "--idempotency-key",
        idempotency_key,
    ]
    if read_only:
        argv.append("--ro")
    argv.append("--json")
    return argv


def revoke_argv(session_id: str) -> list[str]:
    return ["mountable", "sessions", "revoke", session_id]


def sandbox_env(parent: Mapping[str, str]) -> dict[str, str]:
    """Copy of parent with MOUNTABLE_API_KEY removed, including embedded copies."""
    api_key = parent.get(API_KEY_ENV, "")
    env: dict[str, str] = {}
    for key, value in parent.items():
        if key == API_KEY_ENV:
            continue
        if api_key and api_key in value:
            continue
        env[key] = value
    return env


def _contains_key(value: str, api_key: str) -> bool:
    return bool(api_key) and api_key in value


def _default_runner(argv: list[str], env: dict[str, str]) -> tuple[int, str, str]:
    completed = subprocess.run(
        argv,
        env=env,
        capture_output=True,
        text=True,
        check=False,
    )
    return completed.returncode, completed.stdout, completed.stderr


def prepare(
    filesystem_id: str,
    idempotency_key: str,
    read_only: bool = False,
    *,
    runner: Runner | None = None,
    env: Mapping[str, str] | None = None,
) -> PreparedMount:
    parent = dict(os.environ if env is None else env)
    api_key = parent.get(API_KEY_ENV, "")
    if not api_key:
        raise TicketError(
            "unauthenticated",
            "MOUNTABLE_API_KEY is not set on the orchestrator",
            hint="set MOUNTABLE_API_KEY in the orchestrator environment",
        )
    if not filesystem_id or not idempotency_key:
        raise TicketError(
            "invalid_argument",
            "filesystem_id and idempotency_key are required",
        )
    if _contains_key(filesystem_id, api_key) or _contains_key(idempotency_key, api_key):
        raise TicketError("invalid_argument", "arguments must not contain the API key")

    argv = ticket_create_argv(filesystem_id, idempotency_key, read_only)
    code, stdout, stderr = (runner or _default_runner)(argv, parent)
    if code != 0:
        raise _error_from_cli(stdout, stderr, idempotency_key)

    try:
        session = json.loads(stdout)
    except json.JSONDecodeError as exc:
        raise TicketError("invalid_response", "ticket create did not return JSON") from exc
    if not isinstance(session, dict):
        raise TicketError("invalid_response", "ticket create did not return an object")

    session_id = session.get("id")
    ticket = session.get("ticket")
    if not isinstance(session_id, str) or not session_id:
        raise TicketError("invalid_response", "ticket create returned no session id")
    if not isinstance(ticket, str) or not ticket:
        raise TicketError(
            "ticket_unavailable",
            "ticket create returned no ticket",
            hint="follow the ticket create retry rules; do not mount",
            idempotency_key=idempotency_key,
        )
    if _contains_key(ticket, api_key):
        raise TicketError("invalid_response", "refusing to hand the API key to the sandbox")

    child_env = sandbox_env(parent)
    if API_KEY_ENV in child_env or any(_contains_key(value, api_key) for value in child_env.values()):
        raise TicketError("invalid_argument", "sandbox environment still contains the API key")

    stdin = ticket if ticket.endswith("\n") else f"{ticket}\n"
    mount = PreparedMount(session_id=session_id, argv=MOUNT_ARGV, stdin=stdin, env=child_env)
    if api_key in json.dumps(mount.tool_result()):
        raise TicketError("invalid_response", "tool result contains the API key")
    return mount


def _error_from_cli(stdout: str, stderr: str, idempotency_key: str) -> TicketError:
    for blob in (stdout, stderr):
        try:
            parsed = json.loads(blob)
        except json.JSONDecodeError:
            continue
        error = parsed.get("error") if isinstance(parsed, dict) else None
        if isinstance(error, dict) and isinstance(error.get("code"), str):
            retry_after = error.get("retry_after") or 0
            if isinstance(retry_after, bool) or not isinstance(retry_after, int):
                retry_after = 0
            return TicketError(
                error["code"],
                str(error.get("message") or error["code"]),
                hint=str(error.get("hint") or ""),
                idempotency_key=str(error.get("idempotency_key") or idempotency_key),
                retry_after=retry_after,
            )
    return TicketError(
        "ticket_create_failed",
        "mountable ticket create failed",
        idempotency_key=idempotency_key,
    )
