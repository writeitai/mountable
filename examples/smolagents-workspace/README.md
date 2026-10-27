# smolagents tools that keep a note on mountable.io

`write_note` stores one Markdown note. `read_note` reads that file. A later
process, with a read-only handle, reads the same path.

The application chooses the filesystem and the path. Neither is a tool
argument, so a model cannot point the tools at another user's filesystem.
This example calls `ToolCallingAgent.execute_tool_call`. It does not call a
model. If you add a model later, that provider bills you separately, and the
filesystem cap does not limit that bill.

smolagents 1.26.0 marks its API as experimental. This example pins that
version. mountable.io maintains it, independently of Hugging Face. The hosted
mountable.io service is paid. The example and client are open source.

## Setup

Use Python 3.11+ and uv. Set `MOUNTABLE_API_KEY` in your private environment
or secret manager. Use an existing mountable.io account with a saved payment
method, and a filesystem the key can access. The example creates no account,
filesystem or credits. File operations use the normal
[usage prices](https://mountable.io/docs/billing?utm_source=trending-ecosystem-integrations&utm_medium=other&utm_content=smolagents-01-note-tools).

```sh
git clone https://github.com/writeitai/mountable.git
cd mountable/examples/smolagents-workspace
uv sync --locked
```

Your backend chooses the filesystem and the note path. Do not take either
from a model. In a real app, map the authenticated user to a filesystem id
and pass that id here.

## Store a note

`write` opens the filesystem read-write and calls `write_note` through
smolagents. The write uses `If-None-Match: *`. An existing file is left
unchanged.

```sh
uv run python notes.py write \
  --filesystem "$FS_ID" \
  --path notes/brief.md \
  --text "Parser review complete. Check the file next."
```

Use a new path for each note. The path has to look like `notes/brief.md`.
The note is UTF-8 text up to 16 KiB. After a timeout, read the path before
writing it again. A `402` with `filesystem_spending_cap_reached` stops that
user's billable mountable.io work. Other filesystems keep working.

## Read it from a fresh process

`read` is a new process. It opens the filesystem read-only and calls
`read_note`.

```sh
uv run python notes.py read \
  --filesystem "$FS_ID" \
  --path notes/brief.md
```

The command prints the note. It does not print the API key. A read-only
session cannot replace the file.

## Tests

```sh
uv run ruff format --check .
uv run ruff check .
uv run pytest -q
```

The tests call real smolagents tools over a fake filesystem. They check
schema rejection, create-only writes, a read-only refusal, and that a second
process still reads the file. They make no paid cloud or model calls.

Sources: [smolagents tools](https://huggingface.co/docs/smolagents/en/reference/tools),
[mountable.io Files API](https://mountable.io/docs/files-api?utm_source=trending-ecosystem-integrations&utm_medium=other&utm_content=smolagents-01-note-tools).
