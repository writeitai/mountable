# LLM provider starters

Give each end user a shared folder with a hard monthly spending cap.
Use the Python SDK to read and write files from your backend, or let an
agent work on the same files inside a sandbox mount. Signed webhooks tell
your backend when either one changes a file.

The mount helpers use the standard library. The shared backend helpers in
`workspace.py` need Python 3.10+ and `mountable-sdk==0.1.0` (which installs
`httpx`). The provider libraries are only needed when calling a model.

`MOUNTABLE_API_KEY` stays in the orchestrator environment. `prepare` runs
`mountable ticket create --json` there. The sandbox receives the one-time
ticket on stdin of `mountable mount --ticket-stdin --json /mnt/work`. The
ticket is single-use and must be exchanged within five minutes.

| Provider | Module | Registration |
| --- | --- | --- |
| OpenAI Responses API and Agents SDK | `openai_responses.py` | `TOOL`, or `function_tool(create_mount_session)` |
| Anthropic Messages API and Claude Code | `anthropic_tools.py` | `TOOL`; Claude Code uses MCP `create_mount_ticket` |
| Gemini function calling and ADK | `gemini_adk.py` | `tools=[create_mount_session]` |
| xAI Grok | `xai_grok.py` | `TOOL` with `base_url="https://api.x.ai/v1"` |

In the sandbox, run `mkdir -p /mnt/work` before starting `sandbox["argv"]`.
Start that argv as a child and leave it running. Write `sandbox["stdin"]` to
that child's stdin. Use `mount.env` from `prepare`, or `sandbox_env` when
you only have the tool result, so the API key is absent. Read stdout until
the `mounted` event before using the directory. Do not wait for the mount
process to exit; it stays up to renew the session certificate. Revoke from
the orchestrator with `mountable sessions revoke SESSION_ID`.

## One lifecycle for all four providers

Run these snippets from this directory after installing
`pip install mountable-sdk==0.1.0`. Keep `MOUNTABLE_API_KEY` on your backend.
Provisioning requires an owner/admin key that is not limited to one
filesystem, and a saved payment method. The SDK does file access; limits
and webhook registration use REST.

### 1. Create an end user's capped workspace

Your backend chooses the user, cap, and filesystem ID. Store that mapping;
do not let model arguments select another user's workspace or raise a cap.
Create a workspace once per user, rather than once per model call.

```python
import os
import httpx
from workspace import create_workspace

control = httpx.Client(
    base_url="https://mountable.io", timeout=30, follow_redirects=False,
    headers={"Authorization": "Bearer " + os.environ["MOUNTABLE_API_KEY"]},
)
fs_id = create_workspace(control, os.environ["MOUNTABLE_ORG_ID"], "user-42", 500)
```

This caps monthly usage charges at 500 cents in your organisation's billing
currency ($5 or €5). The example also sets 60 changes/minute, 1 GiB storage
and 10,000 entries. The organisation cap still applies on top. After a
create timeout, inspect the filesystem list before retrying: it may already
exist. [Full limits reference](https://mountable.io/docs/end-user-limits).

### 2. Read and write without FUSE, then hand off to a sandbox

```python
from mountable_sdk import Mountable
import openai_responses as provider  # or anthropic_tools, gemini_adk, xai_grok

with Mountable(base_url="https://mountable.io") as client:
    fs = client.filesystem(fs_id, mode="rw")  # reuse this handle across calls
    fs.write("/task.md", "Write a short report.")
    task = fs.read("/task.md").data.decode("utf-8")
# If a sandbox is needed, use the existing provider tool:
mount = provider.create_mount_session(fs_id, "job-42-mount-1", False)
# Start mount["sandbox"] as described above; the same /task.md is in /mnt/work.
```

Reuse the filesystem handle so calls share one token session. For a reader
that must not write, create a separate handle with `mode="ro"`.
On an SDK `MountableError` with status `402` and code
`filesystem_spending_cap_reached`, stop this user's paid work. A
`spending_cap_reached` error means the organisation cap was reached.
On `429 rate_limited`, wait `error.retry_after` seconds before trying more
work. A write timeout has an unknown outcome; read back or use conditional
writes rather than repeating it blindly. [SDK reference](https://mountable.io/docs/sdks).

The provider registration table above stays the same. Use these backend
steps before OpenAI Agents SDK/Responses dispatch, Anthropic tool-use or
Claude MCP dispatch, Gemini ADK dispatch, or Grok dispatch. Send only the
needed task/result text to the model. Choose a fresh idempotency key for
each new mount session; reuse it only to retry that ticket creation.

### 3. Receive signed file changes

Start a public HTTPS receiver first. Register it before the agent writes
outputs, then save the returned ID and one-time secret in your backend's
secret store. Do not print the registration or include it in model messages.

```python
from workspace import register_webhook

hook = register_webhook(
    control, os.environ["MOUNTABLE_ORG_ID"], fs_id,
    "https://your-app.example/hooks/mountable",
)
# Store hook["id"] and hook["secret"] before accepting deliveries.
control.close()  # also close this client on errors in your application
```

Your HTTP handler passes the untouched request bytes and headers to the
verifier. Return 503 while registration is finishing so the initial
`files.resync` can be retried.

```python
from workspace import verify_webhook

# raw_body and request_headers come from your HTTP framework.
try:
    message = verify_webhook(
        hook["secret"], request_headers, raw_body,
        filesystem_id=fs_id, webhook_id=hook["id"],
    )
except ValueError:
    # Reject the request with 400/401; do not parse or process it.
    raise
# Durably enqueue message, then answer 2xx within 10 seconds.
```

The worker applies a message only if `int(message["data"]["cursor"])` is
greater than its stored cursor for that webhook. Persist that cursor with
the applied result, so retries and late deliveries cannot repeat work.
For `files.changed`, reconcile `data.events[]` paths to each event's
`file` value. A new file is `created`, a write is `updated`, and a rename is
`deleted` at the old path plus `created` at the new one. Mount writes may
produce several events. Treat these as current-state invalidations.
Ignore message or event types you don't handle, but still store their cursor
and acknowledge the delivery so it cannot block later changes.

For `files.resync`, relist the files you track through the SDK. Queue
messages arriving during the listing and apply them by cursor afterward.
The helper verifies signatures and routing; your application supplies the
HTTP server, durable queue, and cursor storage.
[Webhook delivery reference](https://mountable.io/docs/webhooks).

## Offline tests

No model keys, network, FUSE, or billing are needed. From the repository root:

```sh
cd sdk/python
uv run --locked python -m unittest discover -s ../../examples/llm-providers -p 'test_*.py'
```

CI runs this same suite against the local SDK. It covers all four provider
mount tools, real SDK requests over fake HTTP, atomic limits, cap refusal,
signed create/update/rename deliveries, resyncs, tampering and stale messages.
The website guide is <https://mountable.io/docs/integrations>.
