---
title: mountable.io — a filesystem that survives restarts
emoji: 📁
colorFrom: blue
colorTo: green
sdk: gradio
app_file: app.py
pinned: false
---

# A filesystem that survives restarts

[mountable.io](https://mountable.io) is a persistent filesystem for AI agents.
This Space writes a note to mountable.io, not to its own disk. Restart the
Space and the note is still here.

This community example is maintained by mountable.io. The hosted mountable.io
service is paid. The example and client are open source.

## How it works

`app.py` saves the note with the Python SDK (`mountable-sdk`) to
`demo/agent-note.txt` in one mountable.io filesystem. On every load it reads
that file back. The Space container can restart or be replaced.
The file stays in mountable.io.

## Space secrets (owner only)

Visitors never see a key. The Space owner sets two Space secrets:

| Secret | Value |
| --- | --- |
| `MOUNTABLE_API_KEY` | An API key limited to the demo filesystem |
| `MOUNTABLE_FS_ID` | The demo filesystem ID |

Create both in the [console](https://mountable.io/app): create a filesystem
for this demo, then create an API key limited to that filesystem under
**API keys**. Give the demo filesystem a hard monthly spend cap in the
console, so public traffic cannot run up a bill. File operations use the
normal [usage prices](https://mountable.io/docs/billing).

## Test without a filesystem

```sh
python3 -m unittest test_space -v
```

The test uses a fake filesystem.
