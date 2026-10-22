# Mountable CLI

`mountable` mounts a [Mountable](https://mountable.io) filesystem, a shared
POSIX filesystem for agents and teams, on any Linux machine or agent sandbox
and on a Mac (macOS 26, Apple Silicon), and manages filesystems, mount
tickets and sessions. It is also an MCP server.

```sh
npx -y mountable-cli login                          # npm
uvx mountable login                                 # PyPI
curl -fsSL https://mountable.io/install.sh | sh     # standalone binary
```

The CLI is one self-contained binary: it downloads nothing at mount time and
keeps the session's credentials in memory. Mounting needs FUSE (`/dev/fuse`)
on Linux; on macOS it needs nothing installed and no `sudo` (see
[macOS](#macos)).

## Authentication

- **People:** `mountable login` signs this machine in with a device flow.
- **Backends and agents:** set `MOUNTABLE_API_KEY=mtbl_…`. An API key belongs
  to one organisation, and the commands use it.
- **Sandboxes:** need no credentials, only a one-time ticket on stdin.

Commands that work inside an organisation take `--org ORG_ID`, then
`MOUNTABLE_ORG`, then the only organisation the caller has; with several,
they fail with `org_required`.

## Commands

| Command | Does |
| --- | --- |
| `mountable orgs list` | the caller's organisations |
| `mountable fs list` | filesystems |
| `mountable fs create NAME` | create a filesystem; prints its ID |
| `mountable fs usage FS_ID` | stored bytes and files against quotas, 30-day traffic |
| `mountable ticket create FS_ID [--ro] [--idempotency-key KEY] [--max-duration D]` | a one-time mount ticket for a sandbox; the session lasts `D` (such as `12h` or `720h`; default 12 hours, at most the API key's or login's expiry) |
| `mountable sessions list FS_ID` | live mount sessions |
| `mountable sessions revoke SESSION_ID` | revoke a session; its mount stops |
| `mountable mount [--ro] FS_ID DIR` | mount with your login |
| `mountable mount --ticket-stdin DIR` | mount with a ticket read from stdin |
| `mountable unmount DIR` | unmount; on Linux, pending writes are committed |
| `mountable mcp` | MCP server over stdio |
| `mountable login`, `logout`, `version`, `licenses` | |

Deleting a filesystem is done in the console.

### Output, errors and exit codes

- `--json` on every command writes one JSON document to stdout, with the
  API's field names (`help --json` writes `{"usage": …}`). Diagnostics go to
  stderr. Nothing ever prompts. The exception is `mountable mcp`, whose stdout
  is the MCP protocol.
- `mountable mount --json` writes JSON lines as the mount progresses:
  `{"event":"mounted","path":…,"filesystem_id":…,"mode":…}` once the mount is
  usable, and `{"event":"unmounted","reason":"signal|unmount|revoked|expired|error","detail":…}`
  when it ends. Wait for `mounted` before using the directory.
- Exit codes: `0` success; `1` an error from the API, network, mount or
  credentials; `2` wrong usage.
- Errors carry a stable code and a next step: on stderr, or with `--json` as
  `{"error":{"code":…,"message":…,"hint":…}}` on stdout. For example,
  `unauthenticated` (run `mountable login` or set `MOUNTABLE_API_KEY`),
  `org_required` (pass `--org`), `not_found` (check the ID with
  `mountable fs list`), `payment_required` (the hint is the console's billing
  link), `rate_limited` (wait `retry_after` seconds).
- The one-time ticket appears only in the result of `ticket create`. API keys,
  login tokens, certificates and tickets never appear in errors or logs.

### Retries

- Reads (`list`, `usage`) are safe to retry.
- `ticket create` sends an idempotency key: the one given with
  `--idempotency-key`, or a random one it prints to stderr before sending.
  Retry with the same key after a timeout. When the outcome is unknown
  (`network_error` or `outcome_unknown`), the error's `idempotency_key` is the
  key that request was sent with: retry with exactly that key. A replay
  returns the existing
  session without a ticket:
  - still `requested`: its ticket was lost. The CLI revokes that session,
    creates a new one with a fresh key and reports both session IDs
    (`replaced_session_id`).
  - `active` or later: the ticket was used. The CLI revokes nothing and fails
    with `ticket_already_used`; use a new key for another mount.
- `fs create` has no idempotency key, and names need not be unique. After a
  timeout, run `fs list` and look for the name before creating again.
- `sessions revoke` is idempotent.
- On HTTP 429 (`rate_limited`), wait `retry_after` seconds.

## Agent workflow

The backend holds the API key; the sandbox gets only a ticket.

```sh
# Backend
export MOUNTABLE_API_KEY=mtbl_…
FS_ID=$(mountable fs create agent-workspace)
mountable ticket create "$FS_ID" --idempotency-key job-42 --json   # → {"ticket": "mtbltk_…", …}

# Sandbox, with the ticket in MOUNTABLE_TICKET
mkdir -p /mnt/work
printf '%s\n' "$MOUNTABLE_TICKET" | mountable mount --ticket-stdin --json /mnt/work &
# wait for {"event":"mounted",…}, use /mnt/work, then:
mountable unmount /mnt/work
```

Any existing, writable directory works as the mount point; `mount` checks it
before it uses the ticket. The ticket is read from stdin, so it never appears
in an argument list.

## macOS

On macOS 26 (Apple Silicon) there is nothing else to install: no macFUSE,
no kernel extension and no `sudo`. The CLI serves your filesystem to macOS's
own NFS client, so it works like any other folder:

```sh
FS_ID=$(mountable fs list --json | jq -r '.[0].id')   # or the ID from `mountable fs list`
mkdir -p ~/mnt/work
mountable mount "$FS_ID" ~/mnt/work                   # add --ro for read-only
```

`mountable mount` stays running while the folder is mounted; use the folder
from another terminal or any app, and stop with Ctrl-C or
`mountable unmount ~/mnt/work`.

Your mount is private to your account: the CLI serves it over a socket only
you can open and checks your user ID on every request. Revoking the session,
or letting it expire, takes the mount away. Every write the Mac sends is
saved by the service before macOS hears OK, so a crash of the CLI never loses
a write macOS was told succeeded. Changes from other machines show up within
moments.

### Limits

- Writes are write-through, at about 7 MiB/s; each MiB written uses one unit
  of the filesystem's metadata rate.
- Files reach about 9.76 GiB written sequentially, less when written in many
  small append-and-`fsync` cycles.
- A failed write is reported once per file, to the first `write`, `fsync` or
  `close` of it.
- Writes through `mmap` can fail without an error.
- Other mounts are coordinated per write, not for the whole time a file is
  open.
- Finish writing (close or `fsync` your files) before you unmount: a forced
  unmount of a busy mount can drop writes still in flight.
- macOS creates `._` files on some copies; to keep them out of git, run
  `mkdir -p ~/.config/git && echo '._*' >> ~/.config/git/ignore`.
- No Spotlight indexing, and locks (`flock`, `fcntl`) are local to the mount.

## Skill

A skill teaches coding agents this workflow, the error codes and the retry
rules:

```sh
npx skills add writeitai/mountable
```

It lives in [`skills/mountable/SKILL.md`](skills/mountable/SKILL.md).

## MCP server

`mountable mcp` serves the tools `list_organizations`, `list_filesystems`,
`create_filesystem`, `filesystem_usage`, `create_mount_ticket`,
`list_mount_sessions` and `revoke_mount_session` over stdio
(`create_mount_ticket` takes an optional `max_duration_seconds`). It
authenticates like the CLI. Results are the API's JSON; errors return the
code and hint. Mounting stays a process in the sandbox:
`create_mount_ticket` returns the ticket and the command that mounts with it.

Give the server `MOUNTABLE_API_KEY` through its environment: inherited from
the environment the MCP client starts in, or set in the client's MCP
configuration under `env`. Never pass the key as a command-line argument,
where it would end up in process listings and shell history.

**Claude Code** (`.mcp.json` in the project; `${MOUNTABLE_API_KEY}` is taken
from the environment Claude Code runs in, so the key stays out of the file):

```json
{
  "mcpServers": {
    "mountable": {
      "command": "npx",
      "args": ["-y", "mountable-cli", "mcp"],
      "env": { "MOUNTABLE_API_KEY": "${MOUNTABLE_API_KEY}" }
    }
  }
}
```

**Claude Desktop** (`claude_desktop_config.json`) and **Cursor**
(`~/.cursor/mcp.json`); keep these files private, since they hold the key:

```json
{
  "mcpServers": {
    "mountable": {
      "command": "npx",
      "args": ["-y", "mountable-cli", "mcp"],
      "env": { "MOUNTABLE_API_KEY": "mtbl_…" }
    }
  }
}
```

## SDKs

Backends that read and write files without mounting use the files API through
the SDKs, which create and refresh their own short token sessions from an API
key.

Both are published as `mountable-sdk`, Python 3.10+ on PyPI and TypeScript on
npm (Node 18+, Bun, or Deno):

```sh
pip install mountable-sdk      # import mountable_sdk
npm install mountable-sdk      # import { Mountable } from "mountable-sdk"
```

```python
from mountable_sdk import Mountable

fs = Mountable().filesystem("fs_…", mode="rw")   # MOUNTABLE_API_KEY
info = fs.write("notes/a.md", "hello")
fs.write("notes/a.md", "hello again", if_match=info.etag)
print(fs.read("notes/a.md").data)
```

They live in [`sdk/python`](sdk/python) and [`sdk/typescript`](sdk/typescript),
behave the same way (the shared cases are in [`sdk/contract.md`](sdk/contract.md)),
and will be released together on tags `sdk-vX.Y.Z`.

## LLM provider starters

[`examples/llm-providers`](examples/llm-providers) has stdlib-only tools for
the OpenAI Agents SDK and Responses API, Anthropic Messages API and Claude
Code, Gemini ADK, and xAI Grok. The orchestrator holds `MOUNTABLE_API_KEY`
and the sandbox receives the one-time ticket on stdin. The guide is
<https://mountable.io/docs/integrations>.

## Showcase demos

[`examples/showcase`](examples/showcase) runs two demos: a planner sandbox
and a coder sandbox sharing one directory, then a read-only laptop mount;
and a revocation kill switch. `python3 examples/showcase/relay.py --dry-run`
prints the sequence without calling the API. The same illustrations play at
<https://mountable.io/showcase> and do not create a filesystem.

## Notebook starters

[`examples/notebooks`](examples/notebooks) has a Jupyter notebook for Colab,
Kaggle, Deepnote and Hugging Face Spaces and a Marimo notebook. They keep
datasets, feature caches and checkpoints in a Mountable folder with
`mountable-sdk`, so the files stay when the kernel restarts.

## Multi-agent coding harness

[`examples/research-showcases/multi-agent-harness`](examples/research-showcases/multi-agent-harness)
runs a planner, coder and verifier on one shared folder. A signed file-change
webhook triggers a read-only verifier mount. Run `harness.py` offline by
default, or use `--live` to create a filesystem with a monthly spend cap and
rate limit.

## Licenses

`mountable licenses` prints the licenses and notices of the third-party code
compiled into the CLI, which also ship as
[`THIRD_PARTY_NOTICES`](THIRD_PARTY_NOTICES) with every package and release
archive.

Documentation: <https://mountable.io/docs>.

## Development

```sh
go test ./...
go run ./tools/notices > THIRD_PARTY_NOTICES   # after changing dependencies
packaging/build.sh 0.0.0    # binaries, release archives, npm tarball and wheel in dist/

# SDKs
(cd sdk/python && uv run ruff format --check . && uv run ruff check . && uv run pyright && uv run pytest)
(cd sdk/typescript && npm ci && npm run typecheck && npm test && npm run build)
```

Releases are cut by pushing a tag `vX.Y.Z` (CLI) or `sdk-vX.Y.Z` (SDKs); see
[`packaging/README.md`](packaging/README.md).
The Mountable service itself is operated by WriteIt.ai s.r.o.; this repository
contains the open-source client only.

## License

[Apache-2.0](LICENSE).
