# mountable.io CLI

`mountable` mounts a [mountable.io](https://mountable.io) filesystem, a shared
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
on Linux; on macOS it needs nothing else installed and no `sudo` (see
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
| `mountable fs create NAME [--spending-cap-cents N] [--mutations-per-minute N] [--byte-quota BYTES] [--entry-quota N]` | create a filesystem, with the limits given; prints its ID |
| `mountable fs get FS_ID` | a filesystem and its limits |
| `mountable fs update FS_ID [--spending-cap-cents N\|none] [--mutations-per-minute N\|none] [--byte-quota BYTES] [--entry-quota N]` | change only the limits given (at least one); `none` removes the spending cap or restores the default mutation rate |
| `mountable fs usage FS_ID` | stored bytes and files against quotas, 30-day traffic |
| `mountable ticket create FS_ID [--ro] [--idempotency-key KEY] [--max-duration D]` | a one-time mount ticket for a sandbox; the session lasts `D` (such as `12h` or `720h`; default 12 hours, at most the API key's or login's expiry) |
| `mountable sessions list FS_ID` | live mount sessions |
| `mountable sessions revoke SESSION_ID` | revoke a session; its mount stops |
| `mountable files write FS_ID PATH [--from FILE] [--if-match ETAG] [--if-none-match]` | write a whole file from FILE or stdin; prints `path etag size` |
| `mountable files read FS_ID PATH [--to FILE]` | the file's bytes on stdout, or into FILE (then prints `path etag size`) |
| `mountable files delete FS_ID PATH [--if-match ETAG]` | delete a file, symlink or empty directory |
| `mountable files list FS_ID [PATH] [--after NAME] [--limit N]` | one page of a directory; a note on stderr gives the `--after` for the next |
| `mountable files changes FS_ID [--cursor CURSOR] [--wait SECONDS]` | without `--cursor`, the current cursor at once; with it, the changes after it, waiting up to `--wait` seconds (default 25) for one |
| `mountable webhooks create FS_ID URL [--path-prefix PREFIX]` | a webhook for changes under `PREFIX` (default `/`); prints its signing secret |
| `mountable webhooks list FS_ID` | webhooks, their state and last failure |
| `mountable webhooks delete FS_ID WEBHOOK_ID` | delete a webhook |
| `mountable webhooks rotate-secret FS_ID WEBHOOK_ID` | a new signing secret; prints it |
| `mountable webhooks enable FS_ID WEBHOOK_ID` | turn a disabled webhook back on |
| `mountable mount [--ro] FS_ID DIR` | mount with your login |
| `mountable mount --ticket-stdin DIR` | mount with a ticket read from stdin |
| `mountable unmount DIR` | unmount; on Linux, pending writes are committed |
| `mountable mcp` | MCP server over stdio |
| `mountable login`, `logout`, `version`, `licenses` | |

Deleting a filesystem is done in the console.

The API checks the limits' ranges: `--spending-cap-cents` 0 to 1,000,000,000
(cents a month), `--mutations-per-minute` 1 to 1,000, `--byte-quota` 1 GiB to
100 GiB, `--entry-quota` 10,000 to 1,000,000; the quotas cannot be removed.
`fs create` and `fs update` refuse an API key limited to one filesystem
(`key_limited_to_filesystem`) and need the key's creator to be an owner or
admin. `fs update` and every `webhooks` command need read-write access to the
filesystem. A webhook URL must be `https://` on port 443 with a public host
(`bad_url`), a path prefix `/` or a directory (`bad_path`); a filesystem has at
most 5 webhooks (`webhook_limit`), and a webhook whose creator lost access
cannot be enabled again (`access_revoked`): create a new one. An API key lists and revokes only its own sessions.

The `files` commands use the files API without mounting, with your login or
the API key: each creates a token session for itself (`ro` for `read`, `list`
and `changes`, `rw` for `write` and `delete`), keeps its token in memory and
revokes it at the end; if that fails, a note says so and the session expires
within the hour. Paths are encoded per segment; a `.` or `..` segment fails
with `bad_path` before any request.

### Output, errors and exit codes

- `--json` on every command writes one JSON document to stdout, with the
  API's field names (`help --json` writes `{"usage": …}`). Diagnostics go to
  stderr. Nothing ever prompts. The exception is `mountable mcp`, whose stdout
  is the MCP protocol.
- `files read` without `--to` writes the file's bytes to stdout, so it takes
  `--json` only with `--to` (then it prints the file's `path`, `type`, `size`,
  `etag` and `modified_at`). `files list --json` writes
  `{"entries":[…],"next":…}`, `files changes --json`
  `{"events":[…],"cursor":…}`, `files delete --json`
  `{"path":…,"deleted":true}`.
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
- `precondition_failed` (an `--if-match` or `--if-none-match` that did not
  hold) carries `current_etag`: the file's ETag, or `null` when nothing exists
  there; the hint names it.
- The one-time ticket appears only in the result of `ticket create`, and a
  webhook's signing secret (`whsec_…`) only in the result of `webhooks create`
  and `webhooks rotate-secret`: alone on stdout, with the webhook's ID on
  stderr, or in the JSON answer. API keys, login and session tokens,
  certificates, tickets and webhook secrets never appear in errors or logs.

### Retries

- The CLI never retries on its own.
- Reads (`list`, `get`, `usage`, `files read`, `files list`, `files changes`,
  `webhooks list`) are safe to retry.
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
- `fs update` sets the values given, so repeating it changes nothing more.
- `files write` and `files delete` with `--if-match` or `--if-none-match`
  fail with `precondition_failed` rather than undo a change made since; a
  repeat after an unknown outcome can fail that way because the first one
  succeeded, so compare `current_etag` with the file you wrote.
- `webhooks create` is not idempotent: after an unknown outcome, run
  `webhooks list` before creating again. `webhooks rotate-secret` issues a new
  secret each time; after an unknown outcome, rotate again and use the secret
  it prints. `webhooks delete` after a lost answer answers `not_found`.
  `webhooks enable` is safe to repeat.
- `sessions revoke` is idempotent.
- On HTTP 429 (`rate_limited`), wait `retry_after` seconds.

### Files limits

- `files read` to stdout streams the file: if the download breaks off, the
  command fails (exit 1) and the bytes already written are incomplete. With
  `--to FILE`, FILE is replaced only once the whole file arrived.
- `files write` reads stdin fully before sending, so its length is known;
  `--from FILE` streams a regular file with its size.
- Each request has 60 seconds, the transfer included (`files changes`: 60
  seconds plus `--wait`).

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
a write macOS was told succeeded. Changes made on another Mac or a Linux
machine show up here in under a second, and the other way round.

### Limits

- Writes are write-through, at about 7 MiB/s; each write macOS sends, up to
  1 MiB, uses one unit of the filesystem's metadata rate.
- Databases, logs and files patched in place work in the mount. SQLite works
  in its default rollback-journal mode (WAL falls back to it on the mount),
  `integrity_check` clean, and a 10,500-page-write run stayed clean. Each
  transaction creates and deletes its journal file, which counts toward the
  metadata rate, so expect up to a few hundred transactions a minute. `git`:
  100 commits, `git fsck` clean. A file the service stores in more than about
  8,000 pieces, such as a log appended and `fsync`ed record by record or a
  file rewritten in small patches, keeps taking writes past the 9,999-piece
  limit: each write there also merges earlier pieces into one. Checked with
  10,500 appends and 10,500 in-place rewrites, every byte read back.
- Merging is best effort. It pauses while the filesystem is above 80% of its
  storage quota, after one mount has merged a tenth of the quota in a day,
  or when the service is too slow to answer in time; a file that then
  reaches 9,999 pieces fails with "file too large". Merged data counts
  toward storage, downloads and spending caps like any other data.
- Near that limit a file writes more slowly: small patches went from about 330
  writes a minute to about 75. A file written start to end runs at about a
  third of the usual speed past about 7.8 GiB, and reaches about 15 GiB in a
  day at the default quota, about 19.5 GiB at most.
- A failed write is reported once per file, to the first `write`, `fsync` or
  `close` of it by any program: if another program, such as a Finder
  preview, closes the file first, your `fsync` returns success for data that
  was not stored. Open the file with `O_SYNC` to get each write's error.
- Writes through `mmap` can fail without an error.
- Other mounts are coordinated per write, not for the whole time a file is
  open.
- Finish writing (close or `fsync` your files) before you unmount: a forced
  unmount of a busy mount, a revoked or expired session, or a crash of the
  CLI drops writes macOS has not sent yet, and an `fsync` running at that
  moment can still return success.
- macOS creates `._` files beside most files; to keep them out of git, run
  `mkdir -p ~/.config/git && echo '._*' >> ~/.config/git/ignore` (or append
  `._*` to the file `git config --global core.excludesFile` names, if set).
- No Spotlight indexing, and locks (`flock`, `fcntl`) are local to the mount:
  open a database through one mount at a time, even on the same machine.

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
`list_mount_sessions`, `revoke_mount_session`, `list_webhooks`,
`delete_webhook` and `enable_webhook` over stdio (`create_mount_ticket`
takes an optional `max_duration_seconds`). It authenticates like the CLI.
Results are the API's JSON; errors return the code and hint. Mounting stays
a process in the sandbox: `create_mount_ticket` returns the ticket and the
command that mounts with it. Creating a webhook and rotating its secret are
not tools: their result is the signing secret, which belongs in the backend
that receives the deliveries, not in a model's context. Run
`mountable webhooks create` there, or use the SDKs.

Give the server `MOUNTABLE_API_KEY` through its environment: inherited from
the environment the MCP client starts in, or set in the client's MCP
configuration under `env`. Never pass the key as a command-line argument,
where it would end up in process listings and shell history.

Root [`mcp.json`](mcp.json) is the public manifest Cursor Directory reads.
It pins the verified CLI and uses the placeholder `REPLACE_WITH_YOUR_KEY`.
Replace that placeholder in your private editor config. Do not commit a real key.
[`.cursor-plugin/plugin.json`](.cursor-plugin/plugin.json) supplies the
directory listing name mountable.io and the guide URL.

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
key. In a webhook receiver, `verify_webhook` / `verifyWebhook` checks a
delivery's signature and returns its message.

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

## Framework and memory examples

[`examples/llamaindex-snapshot`](examples/llamaindex-snapshot) saves a small
LlamaIndex index as checked JSON files and loads it in a fresh process. Follow
[the snapshot guide](https://mountable.io/docs/llamaindex).

[`examples/external-integrations`](examples/external-integrations) has Pydantic
AI tools for persistent Markdown notes and a paginated Mem0 record export.
Both use the Files API through the SDK. Their offline tests exercise the real
frameworks and SDKs without paid cloud or model calls.

[`examples/langgraph-workspace`](examples/langgraph-workspace) has a LangGraph
write node and a read node on one mountable.io filesystem. A later process
reads the same note with a new in-memory checkpoint.
[`examples/langgraph-js-workspace`](examples/langgraph-js-workspace) is the
same handoff in LangGraph.js. The guide is
<https://mountable.io/docs/langgraph>.

[`examples/crewai-workspace`](examples/crewai-workspace) has a CrewAI writer
and reader on one mountable.io filesystem. The writer answers `stored`, and
the reader reads the file. A later process reads the same note. The guide is
<https://mountable.io/docs/crewai>.

[`examples/smolagents-workspace`](examples/smolagents-workspace) has two
smolagents tools on one mountable.io filesystem. `write_note` stores a note
and `read_note` reads it from a later process. The example calls the
framework's tool interface and does not call a model. The guide is
<https://mountable.io/docs/smolagents>.

[`examples/playwright-artifacts`](examples/playwright-artifacts) saves a
screenshot and a Playwright trace of a local fixture, uploads the closed
files with the TypeScript SDK, and has a later process check the same bytes.
The guide is <https://mountable.io/docs/playwright>.

[`examples/slack-file-webhook`](examples/slack-file-webhook) checks a signed
`files.changed` webhook and posts the path to a Slack incoming webhook. The
guide is <https://mountable.io/docs/slack-file-webhook>.

[`examples/git-and-output`](examples/git-and-output) keeps a source change in
local Git history and hands its generated report to a fresh read-only worker
through the SDK. Its offline tests need no credentials.

[`examples/fastapi-reports`](examples/fastapi-reports) generates a small
report in a FastAPI process, uploads it, and returns it from a second
process. The caller receives an id only after the upload succeeds. The guide
is <https://mountable.io/docs/fastapi>.

[`examples/csv-parquet-batches`](examples/csv-parquet-batches) commits small
CSV-to-Parquet batches one at a time through the SDK. A manifest records a
batch only after its upload reads back with the right checksum, so a fresh
process resumes without redoing or duplicating a batch. Its offline tests
need no credentials.

## Showcase demos

[`examples/showcase`](examples/showcase) runs two demos: a planner sandbox
and a coder sandbox sharing one directory, then a read-only laptop mount;
and a revocation kill switch. `python3 examples/showcase/relay.py --dry-run`
prints the sequence without calling the API. The same illustrations play at
<https://mountable.io/showcase> and do not create a filesystem.

## Notebook starters

[`examples/notebooks`](examples/notebooks) has a Jupyter notebook for Colab,
Kaggle, Deepnote and Hugging Face Spaces and a Marimo notebook. They keep
datasets, feature caches and checkpoints in a mountable.io folder with
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
The mountable.io service itself is operated by WriteIt.ai s.r.o.; this repository
contains the open-source client only.

## License

[Apache-2.0](LICENSE).
