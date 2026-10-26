# OpenAI Agents SDK file tools on mountable.io

Give an agent persistent notes on a filesystem selected by your backend.
This community example is maintained by mountable.io, independently of OpenAI.
The hosted mountable.io service is paid; the example and client are open source.

## Setup

Use Python 3.11+ and uv. Set `MOUNTABLE_API_KEY`, `MOUNTABLE_ORG` and
`OPENAI_API_KEY` in your private environment or secret manager. Use an existing
mountable.io account with a saved payment method. Never put credentials in
prompts or source files. OpenAI model use is billed separately.

```sh
git clone https://github.com/writeitai/mountable.git
cd mountable/examples/openai-agents
uv sync --locked
uv run python workspace.py provision --name notes-demo --spending-cap-cents 100
```

The provision command creates one filesystem with a 100-cent monthly cap,
1 GiB storage quota, 10,000 entries and 30 mutations/minute. The cap is in the
organisation's billing currency (USD or EUR). It is a ceiling on mountable.io
billable work, not a price or a grant of credit. It does not cover OpenAI.
Read [billing](https://mountable.io/docs/billing?utm_source=trending-ecosystem-integrations&utm_medium=other&utm_content=openai-agents-example-billing) and
[end-user limits](https://mountable.io/docs/end-user-limits?utm_source=trending-ecosystem-integrations&utm_medium=other&utm_content=openai-agents-example-limits).

In a real app, provision **once per authenticated user**, and store the returned
`filesystem_id` in your database against that user's ID. On every later request,
look it up using the authenticated session. Never accept this mapping from the
model or an untrusted browser field. Serialize provisioning for that user.
If creation times out, inspect the filesystem list before trying again.

Set `FS_ID` to the ID returned by provision. Reuse it on every later run:

```sh
export FS_ID="your-filesystem-id"
uv run python workspace.py run --filesystem "$FS_ID" --note /note.txt
uv run python workspace.py run --filesystem "$FS_ID" --note /note.txt
```

The first run asks the model to create and read a note. The second reads the
existing note without replacing it. `--text` chooses new note contents;
`--note` is a **trusted backend setting**, not a model argument. The script
checks that the selected filesystem has a finite cap. It uses `gpt-4.1-mini`
by default; `--model` selects another model available to your OpenAI account.

## What the model can do

`write_note(text)` creates one UTF-8 note, up to 16 KiB, with `If-None-Match: *`.
`read_note()` reads that same note. Neither tool accepts a filesystem ID or a
path. Both get a `Workspace` through the SDK's local context. A new request
receives a new context with that user's saved filesystem and the chosen path.

The SDK's schema rejects extra properties at the model API; the handler also
ignores extra identity/path fields if a provider supplies them anyway. The
filesystem selection is in application code. The API key stays in the trusted
backend. For production, use filesystem-limited credentials where practical;
this tool boundary does not isolate the host process that holds the key.

`failure_error_function=None` propagates storage errors out of the agent run.
A 402 `filesystem_spending_cap_reached` stops the run. The tools never raise
the cap, create another filesystem or keep retrying refused billable work.
The CLI prints the error code, without private provider details. Stored files are kept; the cap already reserves their storage cost for the month; read [cap behavior](https://mountable.io/docs/end-user-limits?utm_source=trending-ecosystem-integrations&utm_medium=other&utm_content=openai-agents-example-limits).
On an uncertain write, read back before retrying. A 412 simply means the note
already exists and should be read.

Only pass notes you intend to share with OpenAI. The example disables Agents
SDK tracing, sends note contents to the model when read and prints its summary.
It allows at most six agent turns. That turn limit is **not a dollar cap** on
OpenAI usage. Set your OpenAI project's own budget and limits separately.
The 16 KiB check limits what is returned to the model; the synchronous storage
SDK downloads the file before checking its size.

## Tests and verification

```sh
uv run --locked ruff format --check .
uv run --locked ruff check .
uv run --locked pytest -q
```

Offline tests invoke the real Agents SDK tool pipeline. They check two-user
separation, persistence across contexts, create-only writes, hostile identity
arguments, UTF-8/size checks and the 402 fail-stop behavior.
Production storage checks also exercised these tools through the Agents SDK's
tool interface, including separate filesystems, fresh clients and cap refusal.

Sources: [OpenAI Agents SDK quickstart](https://developers.openai.com/api/docs/guides/agents/quickstart),
[local context](https://developers.openai.com/api/docs/guides/agents/define-agents#keep-local-context-separate-from-model-context).
