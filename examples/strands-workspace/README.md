# Strands Agents: keep a note for the next worker

Two `@tool` functions use mountable.io to preserve a note after the Python
process exits. The CLI invokes them through Strands' validated tool-use
stream (`DecoratedFunctionTool.stream`) with synthetic tool-use requests.
No `Agent` or model provider is created. This checks tool dispatch and
storage wiring, not autonomous agent behaviour or a Strands session
manager. mountable.io maintains this community example separately from
AWS and the Strands project.

## Run in two processes

Use Python 3.11+ and uv. Dependencies are pinned to strands-agents 1.62.0
and mountable-sdk 0.3.0 in `pyproject.toml`; `uv.lock` pins the dependency
tree. Use an existing mountable.io account with a saved payment method,
filesystem, and API key in your private `MOUNTABLE_API_KEY` environment.
Hosted storage and downloads are paid usage at
https://mountable.io/docs/billing. This example creates no account,
filesystem, free credits or model requests.

```sh
git clone https://github.com/writeitai/mountable.git
cd mountable/examples/strands-workspace
uv sync --locked
export FS_ID=your-filesystem-id
export NOTE_PATH="strands-note-$(uv run python -c 'import uuid; print(uuid.uuid4().hex)').md"
export NOTE_TEXT="Parser review complete. Open the stored note."
uv run python notes.py write --filesystem "$FS_ID" --path "$NOTE_PATH" --text "$NOTE_TEXT"
uv run python notes.py read --filesystem "$FS_ID" --path "$NOTE_PATH"
```

The first command prints `stored`. The second, separate process prints:

```text
Parser review complete. Open the stored note.
```

Your trusted backend chooses `FS_ID` and `NOTE_PATH` from the authenticated
user before building the tools. Do not take them from model arguments.
`write_note` exposes only `text`; `read_note` exposes no parameters.
Strands' input validation silently drops unknown keys, so the
`invoke_tool` wrapper refuses extra arguments before dispatch. Schema
metadata alone is not authorization.

## Bounds and failures

Only root filenames like `strands-note-<id>.md` are accepted. Each note is
nonempty UTF-8 text up to 16 KiB. The write sends `If-None-Match: *`;
repeating it gets HTTP 412 and leaves the first file intact. The reader
opens a read-only SDK session, which refuses writes with HTTP 403. SDK
sessions scope the whole filesystem, not an individual note path. Use a
filesystem per user and an appropriately scoped API key.

Storage errors raise sanitized exceptions; they never become a successful
`stored` result. Writes are not retried. A network error can leave the
outcome unknown: read the chosen path before another write. A spending-cap
error stops billable work on that filesystem; it does not limit a model
provider's bill if you later add a model. Configure caps through your
backend, as described at https://mountable.io/docs/end-user-limits.

## Cleanup

The example leaves the note for the next worker. Once finished, remove only
that note with a hash of the original bytes:

```sh
NOTE_SHA256=$(uv run python -c 'import hashlib,os; print(hashlib.sha256(os.environ["NOTE_TEXT"].encode()).hexdigest())')
uv run python notes.py cleanup --filesystem "$FS_ID" --path "$NOTE_PATH" --sha256 "$NOTE_SHA256"
```

Cleanup checks the hash and deletes with the file's current ETag. Changed
bytes or a concurrent replacement refuse deletion. Deleted bytes remain
normally billable until reclamation. SDK client close closes HTTP
connections, not the server's token session; sessions expire after one
hour. An application that needs earlier cutoff should revoke only sessions
it created via the management API. Never revoke another worker's sessions.

## Offline verification

```sh
uv run --locked ruff format --check .
uv run --locked ruff check .
uv run --locked pytest -q
```

The tests exercise real Strands tool schemas and tool-use stream dispatch
and the released SDK against fake HTTP. No cloud or model calls. For the
production check, use a unique filename and your own paid test filesystem,
then clean up the note.

Sources: [Strands tool decorator API](https://strandsagents.com/docs/api/python/strands.tools.decorator/),
[Files API](https://mountable.io/docs/files-api).
