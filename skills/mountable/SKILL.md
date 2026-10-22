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

**5. Unmount** when done, so pending writes are committed:

```sh
mountable unmount /mnt/work      # or send SIGTERM to the mount process
```

**6. Backend (optional): end access early.**

```sh
mountable sessions list FS_ID --json
mountable sessions revoke SESSION_ID --json
```

A revoked session's mount stops working within seconds.

Other commands: `mountable orgs list`, `mountable fs usage FS_ID` (stored
bytes and files against quotas, 30-day traffic).

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
| `network_error`, `outcome_unknown` | Retry reads; for `ticket create`, retry with the error's `idempotency_key`. |

## Retries

- **Reads** (`orgs list`, `fs list`, `fs usage`, `sessions list`) are safe to
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

A backend that only reads and writes files can skip mounting and use the
SDKs: conditional writes with ETags, listings, and a change feed (take a
cursor first, then list, then follow changes). Install them as
`mountable-sdk` (`pip install mountable-sdk`, `npm install mountable-sdk`).

## Limits per end user

Give each end user of your app a filesystem with its own monthly spend cap and
change rate, set through the REST API when you create it (the CLI, MCP server
and SDKs cannot set limits yet; `ORG_ID` is in `mountable orgs list`). curl
reads the key from stdin, never from an argument:

```sh
printf 'header = "Authorization: Bearer %s"\n' "$MOUNTABLE_API_KEY" |
  curl -q -sS --fail-with-body --config - -H "Content-Type: application/json" \
    "https://mountable.io/api/v1/orgs/$ORG_ID/filesystems" \
    -d '{"name": "user-42", "spending_cap_cents": 500, "mutations_per_minute": 120}'
```

`PATCH` the filesystem's URL to change a limit; `null` removes a cap. Past the
cap, its mounts and the files API (`402 filesystem_spending_cap_reached`)
refuse billable work; other end users keep working. A key limited to one
filesystem cannot raise its own limits. See
https://mountable.io/docs/end-user-limits

## Webhooks when files change

Create one with the same curl call, sending
`{"url": "https://example.com/hooks/mountable", "path_prefix": "/"}` to
`…/filesystems/$FS_ID/webhooks` with a key that can write the filesystem. The
response shows the signing `secret` (`whsec_…`) once; store it like the key.
Every change, from mounts or the files API, then arrives as a `files.changed`
message signed per Standard Webhooks. Verify the signature on the raw body,
answer `2xx` within 10 seconds, apply a message only if its `data.cursor`,
compared as an integer, exceeds the last one applied (delivery is at least
once), and on `files.resync` list again. See https://mountable.io/docs/webhooks

## Limitations

- Linux sandboxes need FUSE: `/dev/fuse` must exist and be usable. Providers
  without FUSE cannot mount.
- macOS sandboxes are not supported. On a Mac, `mountable mount` works
  without installing anything; writes are slower than on Linux and macOS
  adds `._` files (see the CLI README).
- Deleting a filesystem is done in the console, not from the CLI.
