# Persistent agent notes and Mem0 record exports

These examples use the mountable.io Files API through the Python SDK. No mount,
FUSE or administrator access is needed. mountable.io is a paid hosted service;
the examples need a saved payment method and an existing filesystem for live
use. They do not create accounts, filesystems, API keys or spending limits.

## Set up

Use Python 3.11+ and `uv` from this directory:

```sh
uv sync --locked
```

Your application must choose the authenticated user's filesystem and Mem0 user
ID from its own mapping. Provision a filesystem for each end user, set its
spending cap and quotas, and use a filesystem-limited key. Keep credentials in
your backend or the sandbox provider's secret store. Never send them to a model
or commit them here. See [per-user limits](https://mountable.io/docs/end-user-limits)
and [API keys](https://mountable.io/docs/permissions).

## Pydantic AI: notes that survive a worker restart

`pydantic_workspace.py` registers `read_note` and `create_note` using Pydantic
AI's public tool API. The application passes a fixed filesystem handle through
`RunContext`; the model can choose only a Markdown filename and its text.
The tools work under `notes/`, allow UTF-8 notes up to 16 KiB, and use a
create-only conditional write so another worker's existing note is preserved.

```python
import os

from mountable_sdk import Mountable
from pydantic_workspace import build_agent

with Mountable() as client:
    fs = client.filesystem("YOUR_FILESYSTEM_ID")
    agent = build_agent(os.environ["PYDANTIC_AI_MODEL"])
    result = agent.run_sync("Create summary.md about this task.", deps=fs)
    print(result.output)
```

For a model call, install the appropriate Pydantic AI provider extra in your
own application, such as `pydantic-ai-slim[openai]==2.60.0`, and supply that
provider's key. `PYDANTIC_AI_MODEL` selects the model when running the script
directly. Provider usage has its own bill; mountable.io's filesystem cap does not
cap model spending. A subsequent process can read the same note with the same
filesystem ID and an authorized key.

The cap error is returned to the agent as an error, not silently retried. Your
application should also enforce its own overall model-call and retry budget.
Read-only reviewers should receive a read-only limited key and handle.
A `network_error` on create has an unknown outcome; read the note before
creating it again. A read downloads the file before checking its size, so set
the filesystem's storage quota for your workload as well as a spending cap.

## Mem0: keep a file copy of one user's memory records

`mem0_snapshot.py` calls the hosted Mem0 client's paginated `get_all` API with
an explicit user filter. It collects the returned records, then writes one
new JSON file to mountable.io. A later worker can read that file through the SDK.
This complements Mem0's retrieval; it does not change Mem0's storage backend.

Set `MEM0_API_KEY`, `MEM0_USER_ID`, `MOUNTABLE_API_KEY` and
`MOUNTABLE_FILESYSTEM_ID` in your environment, then run:

```sh
MEM0_TELEMETRY=false uv run mem0_snapshot.py
```

The script prints the file path and record count, not the memories. It never
deletes or changes Mem0 records. Each run chooses a new filename and uses
`If-None-Match: *`; a file-write timeout has an unknown outcome, so inspect that
path before retrying. The example allows at most 1,000 records and a 1 MiB
output. It aborts before writing if pagination repeats a record, returns a
different explicit user ID, or exceeds those bounds.

This is an export of the returned records, not a full database backup or a
restore workflow. Mem0 edits during pagination can change the exported set.
Use only records you are authorized to copy, and apply your application's
retention and deletion policy to these additional copies.

## Verification

```sh
MEM0_TELEMETRY=false uv run pytest -q
uv run ruff check .
uv run ruff format --check .
```

The tests call real Pydantic AI 2.60.0, Mem0 2.2.1 and mountable.io SDK 0.1.0
over fake HTTP transports. They cover tool dispatch, filesystem binding,
path rejection, UTF-8 reads, create-only writes, spending-cap refusal,
paginated exports and failures before an export is written. They make no paid
cloud or model calls. This is not evidence of a live Mem0 or sandbox deployment.

These are community examples maintained by mountable.io, not upstream-approved
integrations. Upstream requests follow each project's contribution process.
