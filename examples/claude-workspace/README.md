# Claude Code → Claude Agent SDK on mountable.io

Claude Code writes a handoff note to a persistent mountable.io filesystem.
A fresh Agent SDK reviewer reads the same note, even from another machine.
It also checks storage usage through mountable.io's existing MCP server.
We maintain this community example for mountable.io.

## Prerequisites

- Python 3.11+, uv, Node.js/npm, and Claude Code.
- An existing mountable.io account with a saved payment method, a filesystem,
  and an API key granted access to it. File operations use the normal
  [usage prices](https://mountable.io/docs/billing). This example creates no
  account, free allowance or credits.
- Your own Anthropic API credentials, following
  [Anthropic's setup](https://code.claude.com/docs/en/agent-sdk/quickstart).
  Model usage is billed separately. The reviewer uses Haiku, at most five
  turns, and a $0.50 SDK budget guard. A final request can exceed that guard.
- Set MOUNTABLE_API_KEY through your secret manager or private shell
  environment. Never put it in a prompt, argument or checked-in file.
  Set MOUNTABLE_ORG to your organisation ID. FS_ID and ORG_ID below are
  identifiers you choose, never model-supplied arguments.

## 1. Let Claude Code write the note

Install the CLI and existing skill in your agent project:

```sh
npm install -g mountable-cli@0.8.0
npx skills add writeitai/mountable --skill mountable --agent claude-code
mountable fs list --json
claude
```

Ask Claude Code, substituting your filesystem ID:

> Use the mountable skill to write "Parser review complete. Check empty input
> next." plus a newline to /handoff.txt on filesystem FS_ID. Use
> mountable files write with --if-none-match and stdin. Read it back with
> mountable files read. Do not mount or print credentials.

Approve the specific CLI commands when asked. Choose a fresh note path for
each run: --if-none-match refuses to replace an existing note. After a
timeout, read back before retrying. CLI file commands revoke their token
session when they finish.

## 2. Run a fresh SDK reviewer

From this directory in a checkout of writeitai/mountable, set FS_ID and
ORG_ID to your filesystem and organisation IDs:

```sh
uv sync --locked
uv run python review.py --filesystem "$FS_ID" --org "$ORG_ID" --note /handoff.txt
```

Run that same command in another runtime with the same key, IDs and note
path to read the persistent handoff there. No filesystem mount is required.
The reviewer prints tool names, its summary and model cost. It fails if
either required tool reports an error.

The backend selects the filesystem and note path. The model's read_note
tool has no arguments and uses a read-only mountable-sdk handle.
The management server is the released npx -y mountable-cli@0.8.0 mcp.
A hook checks every call, allowing only read_note and filesystem_usage for
the chosen filesystem and organisation. Built-in tools are disabled and
project/user settings are not loaded.

This is a tool boundary, not process isolation. The trusted host keeps the
API key. For production, use a filesystem-limited read-only key for the
reviewer and a separate runtime that never inherits the writer's environment.
Only send notes you intend to share with Anthropic. Treat note text as
untrusted data, and never store credentials in it.

## MCP and skills in interactive Claude Code

For interactive filesystem/session management, use the
[existing MCP setup](https://mountable.io/docs/mcp). MCP manages the
filesystem; file contents in this example use the CLI and SDK.
The skill explains both mount and Files API workflows. For agents that
need ordinary local file operations, follow the
[mount guide](https://mountable.io/docs/sandbox).

## Offline verification

```sh
uv run ruff format --check .
uv run ruff check .
uv run pytest -q
```

Tests use the real Agent SDK tool wrapper with a fake filesystem.
They check fixed-path access, rejected cross-filesystem/organisation calls,
error redaction, and the text-size bound. No model or production requests.
