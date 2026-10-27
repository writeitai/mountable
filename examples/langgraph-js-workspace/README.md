# LangGraph.js nodes that share a mountable.io filesystem

A write node stores a note on mountable.io. This example uses an in-memory
checkpointer. The writer returns empty text, so the next node receives no
note body in its input and reads the file. A later process, with a new
in-memory checkpointer, reads the same note.

The nodes are deterministic. They do not call a model, so a passing run
does not check model behavior.

This community example is maintained by mountable.io, independently of
LangChain. The hosted mountable.io service is paid. The example and client
are open source.

## Setup

Use Node.js 20 or newer. Set `MOUNTABLE_API_KEY` in your private environment
or secret manager. Use an existing mountable.io account with a saved payment
method, and a filesystem the key can access. The example creates no account,
filesystem or credits. File operations use the normal
[usage prices](https://mountable.io/docs/billing?utm_source=trending-ecosystem-integrations&utm_medium=other&utm_content=langgraph-js-01-shared-notes).

```sh
git clone https://github.com/writeitai/mountable.git
cd mountable/examples/langgraph-js-workspace
npm ci
```

Your backend chooses the filesystem and the note path. Do not take either
from graph state that a model can set. In a real app, map the authenticated
user to a filesystem id and pass that id here. If state names a different
filesystem, the nodes refuse the call and do not touch the file.

## One run: the next node reads the file

`handoff` writes `notes/brief.md` with `If-None-Match: *`, then a second node
reads it back. The writer returns empty text, so the reader receives no note
body in its input state. Earlier checkpoint steps still hold the input text.
The command opens the filesystem read-write.

```sh
npx tsx graph.ts handoff \
  --filesystem "$FS_ID" \
  --path notes/brief.md \
  --text "Parser review complete. Check empty input next."
```

The command prints a new thread id, the path, and the note:

```text
thread_id=7c1e0a2b4d5f6789a0b1c2d3e4f56789
path=notes/brief.md
Parser review complete. Check empty input next.
```

The thread id changes on every run. Use a new path for each note. An existing
file is left unchanged. After a timeout, read the path before writing it
again. A `402` with `filesystem_spending_cap_reached` stops that user's
billable mountable.io work. Other filesystems keep working.

## A later run: a new in-memory checkpoint still sees the note

`resume` starts a new process and a new in-memory checkpointer. It only
reads, through a read-only filesystem handle. A persistent LangGraph
checkpointer is a separate choice. This example does not use one.

```sh
npx tsx graph.ts resume \
  --filesystem "$FS_ID" \
  --path notes/brief.md
```

The command prints the note. It does not print the API key. LangGraph is
not given a model here. If you add a model node, that provider bills you
separately, and the filesystem cap does not limit that bill.

## Tests

```sh
npm run typecheck
npm test
```

The tests compile real LangGraph graphs over a fake filesystem. They check
that the reader returns the stored bytes, that `read_note` is entered with
empty text, that a second graph with a new checkpointer still reads the file,
that a create-only write leaves an existing note in place, and that a
different filesystem id, a read-only handle, a rejected path, an oversized
note or a spending-cap error never retries the write. They make no paid
cloud or model calls.

Sources: [LangGraph graph API](https://docs.langchain.com/oss/javascript/langgraph/graph-api),
[mountable.io Files API](https://mountable.io/docs/files-api?utm_source=trending-ecosystem-integrations&utm_medium=other&utm_content=langgraph-js-01-shared-notes).
