---
name: mountable
description: Give an agent sandbox a shared, persistent POSIX filesystem with Mountable. Use when files must outlive a sandbox, be shared between sandboxes or agents, or be handed from a backend to a sandbox; covers creating filesystems, minting one-time mount tickets with an API key, mounting with `mountable mount --ticket-stdin --json`, error codes and safe retries, per-end-user spend caps, and signed webhooks when files change.
---

# Mountable

Mountable is a shared POSIX filesystem for agents and teams. A backend that
holds an API key creates filesystems and one-time mount tickets; a sandbox
mounts a filesystem with a ticket and uses it like a local directory.

## When to use

- Files must survive the sandbox (results, checkpoints, caches, datasets).
- Several sandboxes or agents work on the same files.
- A backend prepares files that a sandbox reads, or collects what it writes.

## Install the CLI

```sh
curl -fsSL https://mountable.io/install.sh | sh   # standalone binary
npx -y mountable-cli version                      # or through npm
uvx mountable version                             # or through PyPI
```

## Authenticate

- **Backend or agent:** set `MOUNTABLE_API_KEY=mtbl_…` (created by an owner or
  admin in the console). Commands use the key's organisation.
- **Person:** `mountable login` (device flow). With several organisations,
  pass `--org ORG_ID` or set `MOUNTABLE_ORG`.
- **Sandbox:** needs no credentials, only a one-time ticket on stdin.

Never put an API key or a ticket in a command-line argument, a log or a
prompt you echo back.

## The workflow

Add `--json` to every command: stdout then carries one JSON document with the
API's field names, and diagnostics go to stderr. Nothing prompts.

**1. Backend: pick or create a filesystem.**

```sh
mountable fs list --json
mountable fs create "agent-workspace" --json      # → {"id": "…", "name": …}
```

**2. Backend: create a ticket** with an idempotency key you choose for this
mount (for example, the job ID):

```sh
mountable ticket create FS_ID --idempotency-key job-42 --json
# → {"id": SESSION_ID, "state": "requested", "ticket": "mtbltk_…", "ticket_expires_at": …}
```

Add `--ro` for a read-only mount. The ticket works once and expires within
minutes, so hand it to the sandbox right away. The mount itself lasts 12 hours
by default; `--max-duration 720h` (any Go duration) asks for longer, up to the
API key's or login's expiry. A mount that must stay up permanently needs a
long `--max-duration`, and a new ticket whenever it restarts.

**3. Sandbox: mount** onto an existing, writable directory (any path),
giving the ticket on stdin (here from an environment
variable passed to the sandbox), and wait for the `mounted` event:

```sh
mkdir -p /mnt/work
printf '%s\n' "$MOUNTABLE_TICKET" | mountable mount --ticket-stdin --json /mnt/work &
```

`mount` keeps running while the filesystem is mounted, and writes JSON lines:

```json
{"event":"mounted","path":"/mnt/work","filesystem_id":"…","mode":"rw"}
{"event":"unmounted","reason":"signal|unmount|revoked|expired|error","detail":"…"}
```

Use the directory only after `mounted`. If the command exits first, its last
line is `{"error": …}`.

**4. Sandbox: read and write** with ordinary file operations.

**5. Unmount** when done; on Linux this commits pending writes:

```sh
mountable unmount /mnt/work      # or send SIGTERM to the mount process
```

**6. Backend (optional): end access early.**

```sh
mountable sessions list FS_ID --json
mountable sessions revoke SESSION_ID --json
```

A revoked session's mount stops working within seconds.

Other commands: `mountable orgs list`, `mountable fs get FS_ID` (the
filesystem and its limits), `mountable fs usage FS_ID` (stored bytes and
files against quotas, 30-day traffic).

## Errors

Exit code `0` is success, `1` an error (API, network, mount or credentials),
`2` wrong usage. With `--json` an error is written to stdout as:

```json
{"error": {"code": "not_found", "message": "…", "hint": "check the ID with `mountable fs list`"}}
```

| Code | What to do |
| --- | --- |
| `unauthenticated` | Run `mountable login` or set `MOUNTABLE_API_KEY`. |
| `org_required` | Pass `--org ORG_ID` (see `mountable orgs list`). |
| `not_found` | Check the ID with `mountable fs list`; an API key sees only filesystems it has a grant on. |
| `no_grant` | Ask an owner or admin for a grant, or use `--ro`. |
| `api_key_forbidden` | A person must do this (console or `mountable login`). |
| `payment_required` | The hint is the console's billing link; give it to a person. |
| `rate_limited` | Wait `retry_after` seconds, then retry. |
| `ticket_already_used` | Use a new `--idempotency-key` for another mount. |
| `ticket_invalid` | The ticket was used or expired; create a new one. |
| `idempotency_conflict` | An identical request is in progress; retry shortly with the same key. |
| `key_limited_to_filesystem` | A key limited to one filesystem cannot create filesystems or change limits; use an organisation-wide key. |
| `precondition_failed` | The file changed (or exists, with `--if-none-match`); the error's `current_etag` is its ETag, `null` when nothing exists. Read it again before writing. |
| `bad_path` | Drop `.` and `..` segments from the path. |
| `network_error`, `outcome_unknown` | Retry reads; for `ticket create`, retry with the error's `idempotency_key`. |

## Retries

- The CLI never retries on its own.
- **Reads** (`orgs list`, `fs list`, `fs get`, `fs usage`, `sessions list`,
  `files read`, `files list`, `files changes`, `webhooks list`) are safe to
  retry.
- **`ticket create`**: always pass a caller-known `--idempotency-key` (without
  one, the CLI prints a generated key to stderr before sending). After a
  timeout or an unknown outcome, retry with the key in the error's
  `idempotency_key`. A replay returns the existing session
  without a ticket, and the CLI handles it:
  - still `requested`: the ticket was lost. The CLI revokes that session,
    creates a new one under a fresh key, and returns it with
    `replaced_session_id`.
  - `active` or later: the ticket was used. Nothing is revoked; the command
    fails with `ticket_already_used`. Keep the working mount, and use a new
    key for another mount.
- **`fs create`** has no idempotency key and names need not be unique. After
  an unknown outcome, run `fs list` and look for the name before creating
  again. Never retry blindly.
- **`fs update`** sets the values given; repeating it changes nothing more.
- **`webhooks create`** is not idempotent: after an unknown outcome, run
  `webhooks list` before creating again. **`webhooks rotate-secret`** issues a
  new secret each time; after an unknown outcome, rotate again and keep the
  secret it prints. **`webhooks delete`** after a lost answer answers
  `not_found`. **`webhooks enable`** is safe to repeat.
- **`files write`** and **`files delete`** with `--if-match` or
  `--if-none-match` can fail with `precondition_failed` on a repeat because
  the first attempt succeeded: compare `current_etag` with what you wrote.
- **`sessions revoke`** is idempotent.
- **HTTP 429** (`rate_limited`): wait `retry_after` seconds.

## MCP

`mountable mcp` serves the same operations as MCP tools over stdio
(`list_organizations`, `list_filesystems`, `create_filesystem`,
`filesystem_usage`, `create_mount_ticket`, `list_mount_sessions`,
`revoke_mount_session`). Mounting stays a command in the sandbox:
`create_mount_ticket` returns the ticket and the exact `mount_command`, and
takes an optional `max_duration_seconds`.

## Files without mounting

A backend that only reads and writes files can skip mounting. The CLI does it
with the login or the API key, through a token session per command that it
revokes at the end:

```sh
printf 'hello' | mountable files write FS_ID notes/a.md --json     # → {"path": "/notes/a.md", "etag": …, …}
mountable files write FS_ID notes/a.md --from a.md --if-match ETAG --json
mountable files read FS_ID notes/a.md                              # bytes on stdout
mountable files read FS_ID notes/a.md --to a.md --json             # all or nothing
mountable files list FS_ID notes --limit 100 --json                # → {"entries": […], "next": …}
mountable files delete FS_ID notes/a.md --if-match ETAG --json
mountable files changes FS_ID --json                               # → {"events": [], "cursor": …}
mountable files changes FS_ID --cursor CURSOR --wait 25 --json
```

`--if-none-match` writes only if nothing exists at the path. `files read`
takes `--json` only with `--to`; to stdout, a download that breaks off fails
the command and leaves incomplete bytes, so prefer `--to`. To stay in sync:
take a cursor first, then list, then follow changes from it
(`cursor_expired` means start again).

The SDKs do the same from code, with conditional writes, listings and the
change feed. Install them as `mountable-sdk` (`pip install mountable-sdk`,
`npm install mountable-sdk`).

## Limits per end user

Give each end user of your app a filesystem with its own monthly spend cap and
change rate when you create it (the MCP server cannot set limits):

```sh
mountable fs create user-42 --spending-cap-cents 500 --mutations-per-minute 120 --json
mountable fs update FS_ID --spending-cap-cents 1000 --json     # only the flags given change
mountable fs update FS_ID --spending-cap-cents none --json     # removes the cap
mountable fs get FS_ID --json
```

`--mutations-per-minute none` restores the default rate. `--byte-quota` (1 GiB
to 100 GiB) and `--entry-quota` (10,000 to 1,000,000) can be changed but not
removed; the API checks every range (`invalid_request`). Past the cap, its
mounts and the files API (`402 filesystem_spending_cap_reached`) refuse
billable work; other end users keep working. Creating filesystems and
changing limits need a key whose creator is an owner or admin; a key limited
to one filesystem cannot (`key_limited_to_filesystem`). See
https://mountable.io/docs/end-user-limits

## Webhooks when files change

Create one with a key that can write the filesystem (every webhook command
needs read-write access):

```sh
mountable webhooks create FS_ID https://example.com/hooks/mountable --path-prefix / --json
# → {"id": WEBHOOK_ID, "secret": "whsec_…", …}
mountable webhooks list FS_ID --json              # state, last_failure, failing_since
mountable webhooks rotate-secret FS_ID WEBHOOK_ID --json
mountable webhooks enable FS_ID WEBHOOK_ID --json # after it was disabled
mountable webhooks delete FS_ID WEBHOOK_ID --json
```

The signing `secret` appears once, in the result of `create` or
`rotate-secret` (without `--json`, alone on stdout); store it like the key.
Every change, from mounts or the files API, then arrives as a `files.changed`
message signed per Standard Webhooks. Verify the signature on the raw body,
answer `2xx` within 10 seconds, apply a message only if its `data.cursor`,
compared as an integer, exceeds the last one applied (delivery is at least
once), and on `files.resync` list again. See https://mountable.io/docs/webhooks

## Limitations

- Linux sandboxes need FUSE: `/dev/fuse` must exist and be usable. Providers
  without FUSE cannot mount.
- macOS sandboxes are not supported. On a Mac (macOS 26, Apple Silicon),
  `mountable mount` needs nothing else installed and no `sudo`; its writes
  are write-through (about 7 MiB/s), and macOS adds `._` files beside most
  files. Databases such as SQLite work there; locks are local to one mount,
  so open a database through one mount at a time, even on the same machine. See the CLI README's macOS
  section for the limits.
- Deleting a filesystem is done in the console, not from the CLI.
