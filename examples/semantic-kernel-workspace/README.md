# Semantic Kernel: keep a note for the next worker

Two native functions use mountable.io to preserve a note after the Python
process exits. `Kernel.add_plugin` registers the real `@kernel_function`
methods and `Kernel.invoke` dispatches them. No model service is registered.
This checks native-function dispatch and storage wiring, not AI planning or a
Semantic Kernel memory backend. mountable.io maintains this community example
separately from Microsoft.

## Run in two processes

Use Python 3.11+ and uv. Dependencies are pinned to Semantic Kernel 1.45.0 and
mountable-sdk 0.3.0 in `pyproject.toml`; `uv.lock` pins the dependency tree.
Use an existing mountable.io account with a saved payment method, filesystem,
and API key in your private `MOUNTABLE_API_KEY` environment. Hosted storage
and downloads are paid usage at https://mountable.io/docs/billing. This example
creates no account, filesystem, free credits or model requests.

```sh
git clone https://github.com/writeitai/mountable.git
cd mountable/examples/semantic-kernel-workspace
uv sync --locked
export FS_ID=your-filesystem-id
export NOTE_PATH="sk-note-$(uv run python -c 'import uuid; print(uuid.uuid4().hex)').md"
export NOTE_TEXT="Parser review complete. Open the stored note."
uv run python notes.py write --filesystem "$FS_ID" --path "$NOTE_PATH" --text "$NOTE_TEXT"
uv run python notes.py read --filesystem "$FS_ID" --path "$NOTE_PATH"
```

The first command prints `stored`. The second, separate process prints:

```text
Parser review complete. Open the stored note.
```

Your trusted backend chooses `FS_ID` and `NOTE_PATH` from the authenticated
user before constructing `NotesPlugin`. Do not take them from model arguments.
`write_note` exposes only `text`; `read_note` exposes no parameters. The
`invoke_note` wrapper refuses unknown functions and extra arguments because
`KernelArguments` accepts extra keys. Metadata alone is not authorization.

## Bounds and failures

Only root filenames like `sk-note-review.md` are accepted. Each note is nonempty
UTF-8 text up to 16 KiB. The write sends `If-None-Match: *`; repeating it gets
HTTP 412 and leaves the first file intact. The reader opens a read-only SDK
session, which refuses writes with HTTP 403. SDK sessions scope the whole
filesystem, not an individual note path. Use a filesystem per user and an
appropriately scoped API key.

Storage errors raise sanitized exceptions; they never become a successful
`stored` result. Writes are not retried. A network error can leave the outcome
unknown: read the chosen path before another write. A spending-cap error stops
billable work on that filesystem; it does not limit a model provider's bill if
you later add a model. Configure caps through your backend, as described in
https://mountable.io/docs/end-user-limits.

## Cleanup

The example leaves the note for the next worker. Once finished, remove only
that note with a hash of the original bytes:

```sh
NOTE_SHA256=$(uv run python -c 'import hashlib,os; print(hashlib.sha256(os.environ["NOTE_TEXT"].encode()).hexdigest())')
uv run python notes.py cleanup --filesystem "$FS_ID" --path "$NOTE_PATH" --sha256 "$NOTE_SHA256"
```

Cleanup checks the hash and deletes with the file's current ETag. Changed bytes
or a concurrent replacement refuse deletion. Deleted bytes remain normally
billable until reclamation. SDK client close closes HTTP connections, not the
server's token session; sessions expire after one hour. An application that
needs earlier cutoff should revoke only sessions it created via the management
API. Never revoke another worker's sessions.

## Offline verification

```sh
uv run --locked ruff format --check .
uv run --locked ruff check .
uv run --locked pytest -q
```

The tests exercise real Kernel metadata/dispatch and the released SDK against
fake HTTP. No cloud or model calls. For the production check, use a unique
filename and your own paid test filesystem, then clean up the note.

Sources: [native plugins](https://learn.microsoft.com/en-us/semantic-kernel/concepts/plugins/?tabs=python),
[Kernel.invoke](https://learn.microsoft.com/en-us/python/api/semantic-kernel/semantic_kernel.kernel?view=semantic-kernel-python),
[Files API](https://mountable.io/docs/files-api).
