"""OpenAI Responses API and Agents SDK tool.

Registration, on the orchestrator only:

    from agents import Agent, function_tool
    agent = Agent(name="orchestrator", tools=[function_tool(create_mount_session)])

Responses API tool object is TOOL. Handle a function_call by calling
create_mount_session and returning json.dumps of its result. Do not pass
MOUNTABLE_API_KEY into the sandbox process.

Before dispatching tools, use workspace.create_workspace on the backend to
set this end user's monthly cap. Reuse a mountable-sdk filesystem handle
to read/write without a mount. Register with workspace.register_webhook
and authenticate raw deliveries with workspace.verify_webhook. See README.md
for the shared lifecycle; keep provisioning and webhook secrets outside tools.
"""

from __future__ import annotations

from handoff import DESCRIPTION, PARAMETERS, prepare

TOOL = {
    "type": "function",
    "name": "create_mount_session",
    "description": DESCRIPTION,
    "parameters": PARAMETERS,
    "strict": True,
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
