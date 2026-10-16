"""Gemini function calling and ADK tool.

ADK registers the function directly:

    from google.adk.agents.llm_agent import Agent
    root_agent = Agent(
        name="orchestrator",
        model="gemini-flash-latest",
        instruction="Mint a Mountable ticket before starting a sandbox.",
        tools=[create_mount_session],
    )

Raw Gemini function calling uses FUNCTION_DECLARATION. The function runs on
the orchestrator. The sandbox receives only sandbox.stdin.
"""

from __future__ import annotations

from handoff import DESCRIPTION, PARAMETERS, prepare

FUNCTION_DECLARATION = {
    "name": "create_mount_session",
    "description": DESCRIPTION,
    "parameters": {
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
