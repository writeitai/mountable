"""xAI Grok tool calling.

Grok's Responses API uses the same flat function tool as OpenAI. Point the
OpenAI client at xAI and keep MOUNTABLE_API_KEY on the orchestrator:

    from openai import OpenAI
    client = OpenAI(base_url="https://api.x.ai/v1", api_key=os.environ["XAI_API_KEY"])
    response = client.responses.create(model="grok-4.7", input=messages, tools=[TOOL])

The xAI SDK accepts the same parameters through xai_sdk.chat.tool(name=...,
description=..., parameters=TOOL["parameters"]). Handle the call with
create_mount_session. The sandbox receives only sandbox.stdin.

Before dispatching tools, use workspace.create_workspace on the backend to
set this end user's monthly cap. Reuse a mountable-sdk filesystem handle
to read/write without a mount. Register with workspace.register_webhook
and authenticate raw deliveries with workspace.verify_webhook. See README.md
for the shared lifecycle; keep provisioning and webhook secrets outside tools.
"""

from __future__ import annotations

from openai_responses import TOOL as OPENAI_TOOL
from openai_responses import create_mount_session

CLIENT_BASE_URL = "https://api.x.ai/v1"

# xAI accepts the flat Responses tool, but rejects additionalProperties.
# Strictness is implicit. Do not copy OpenAI's strict schema through.
TOOL = {
    "type": "function",
    "name": OPENAI_TOOL["name"],
    "description": OPENAI_TOOL["description"],
    "parameters": {
        "type": "object",
        "properties": OPENAI_TOOL["parameters"]["properties"],
        "required": list(OPENAI_TOOL["parameters"]["required"]),
    },
}

__all__ = ["CLIENT_BASE_URL", "TOOL", "create_mount_session"]
