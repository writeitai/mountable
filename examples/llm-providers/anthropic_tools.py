"""Anthropic Messages API tool, and the Claude Code handoff rule.

Claude Code on the trusted machine should use the Mountable MCP server
(`mountable mcp`) with MOUNTABLE_API_KEY in that server's environment, and
call create_mount_ticket. Pass only the returned ticket to the sandbox on
stdin. Do not copy the MCP env block into the sandbox.

Messages API callers use TOOL and create_mount_session on the orchestrator.
"""

from __future__ import annotations

from handoff import DESCRIPTION, PARAMETERS, prepare

MCP_TOOL_NAME = "create_mount_ticket"

TOOL = {
    "name": "create_mount_session",
    "description": DESCRIPTION,
    "input_schema": {
        "type": "object",
        "properties": PARAMETERS["properties"],
        "required": ["filesystem_id", "idempotency_key"],
    },
}


def create_mount_session(
    filesystem_id: str,
    idempotency_key: str,
    read_only: bool = False,
) -> dict[str, object]:
    """Create a one-time Mountable mount ticket for one sandbox.

    Runs on the orchestrator, which holds MOUNTABLE_API_KEY. In the sandbox,
    run mkdir -p /mnt/work before starting sandbox.argv. Give the sandbox only
    sandbox.stdin on mountable mount --ticket-stdin. Leave that process
    running and wait for the mounted event. Never send MOUNTABLE_API_KEY into the
    sandbox. Keep session_id and revoke with mountable sessions revoke.
    """
    return prepare(filesystem_id, idempotency_key, read_only).tool_result()
