# A CrewAI crew that shares a mountable.io filesystem

A writer agent stores a note on mountable.io. Its answer is the word `stored`,
so the reader does not receive the note body from the previous task. The
reader calls its file tool and returns the file. A later process reads the
same path.

This example does not call a model provider. A scripted stand-in tells each
agent to use its one file tool. If you add a model later, that provider bills
you separately, and the filesystem cap does not limit that bill.

This community example is maintained by mountable.io, independently of CrewAI.
The hosted mountable.io service is paid. The example and client are open
source.

## Setup

Use Python 3.11+ and uv. Set `MOUNTABLE_API_KEY` in your private environment
or secret manager. Use an existing mountable.io account with a saved payment
method, and a filesystem the key can access. The example creates no account,
filesystem or credits. File operations use the normal
[usage prices](https://mountable.io/docs/billing?utm_source=trending-ecosystem-integrations&utm_medium=other&utm_content=crewai-01-shared-note).

```sh
git clone https://github.com/writeitai/mountable.git
cd mountable/examples/crewai-workspace
uv sync --locked
```

Your backend chooses the filesystem and the note path. Do not take either
from a model. In a real app, map the authenticated user to a filesystem id
and pass that id here.

## One crew: the reader reads the file

`handoff` runs a writer and a reader. The writer creates `notes/brief.md`
with `If-None-Match: *` and answers `stored`. The reader receives that word
as context and reads the file.

```sh
uv run python crew.py handoff \
  --filesystem "$FS_ID" \
  --path notes/brief.md \
  --text "Parser review complete. Check the file next."
```

Use a new path for each note. An existing file is left unchanged. After a
timeout, read the path before writing it again. A `402` with
`filesystem_spending_cap_reached` stops that user's billable mountable.io
work. Other filesystems keep working.

## A later run: a new process still sees the note

`resume` starts a new process. It only reads, through a read-only filesystem
handle.

```sh
uv run python crew.py resume \
  --filesystem "$FS_ID" \
  --path notes/brief.md
```

The command prints the note. It does not print the API key.

## Tests

```sh
uv run ruff format --check .
uv run ruff check .
uv run pytest -q
```

The tests run a real CrewAI crew over a fake filesystem. They check that the
reader returns the stored bytes, that a second process still reads the file,
and that rejected paths, oversized notes and a spending-cap error never retry
the write. They make no paid cloud or model calls.

Sources: [CrewAI tools](https://docs.crewai.com/en/concepts/tools),
[mountable.io Files API](https://mountable.io/docs/files-api?utm_source=trending-ecosystem-integrations&utm_medium=other&utm_content=crewai-01-shared-note).
