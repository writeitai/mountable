# A Deno note the next process can read

A Deno script saves one Markdown note with the released `mountable-sdk` package.
A second process reads the same file. The first process can exit before the
second one starts. The note is not a return value passed between them.

mountable.io maintains this community example. It is not an official Deno
integration or a Deno Deploy app. It makes no model call. The client code is
Apache-2.0. mountable.io storage is a separate paid dependency.

## Run it

Use Deno 2.9.8, an existing mountable.io account with a saved payment method, an
existing filesystem, and an API key that can access it. Set `MOUNTABLE_API_KEY`
in your private environment or secret manager. Never put it in source, command
arguments, or logs. Normal [usage prices](https://mountable.io/docs/billing)
apply. This example creates no account, filesystem, or credits.

```bash
git clone https://github.com/writeitai/mountable.git
cd mountable/examples/deno-note
export MOUNTABLE_FILESYSTEM_ID=your-filesystem-id
export MOUNTABLE_NOTE_PATH="notes/deno-$(date +%s).md"
deno task write --text "Deno note stays."
deno task read
```

`write` prints `stored`. The separate `read` prints `Deno note stays.` The path
is one Markdown file directly under `notes/`, such as
`notes/deno-1710000000.md`. The text is nonempty UTF-8 up to 16 KiB.

Pinned versions: Deno 2.9.8 and `mountable-sdk` 0.3.0. `deno task test` runs the
offline checks against a loopback fake Files API. The lockfile is `deno.lock`.

The first `deno task` may download the pinned npm package through Deno's package
installer. That download is Deno's own installer step. The script's later
requests still need the network permission below.

## Permissions

Your backend selects the filesystem and the note path before it starts the
script. The script accepts only `--text` for `write`, `cleanup`, and
`readonly-write`. `read` takes no arguments. It cannot take a filesystem id, a
path, or an API key from its arguments.

`write`, `read`, `cleanup`, and `readonly-write` run with:

```text
--no-prompt --allow-env=MOUNTABLE_API_KEY,MOUNTABLE_FILESYSTEM_ID,MOUNTABLE_NOTE_PATH --allow-net=mountable.io
```

There is no `--allow-all`. The script reads the API key before it opens a
connection. If `MOUNTABLE_API_KEY` is not permitted, Deno stops the process
before any request. The refusal names the variable and does not print the value.

If network access to `mountable.io` is missing, the SDK reports `network_error`
and sends no request. A read-only storage handle that tries to write receives
HTTP 403 `read_only` from the service. A second create of the same path sends
`If-None-Match: *`, receives HTTP 412 `precondition_failed`, and leaves the
first note in place.

The four tasks above do not grant `MOUNTABLE_API_URL`, so a value sitting in the
environment is not used. The script then talks to `https://mountable.io`.

`deno task test` is the offline check. It allows network access to `127.0.0.1`,
`--allow-run`, and env access for `MOUNTABLE_API_KEY`,
`MOUNTABLE_FILESYSTEM_ID`, `MOUNTABLE_NOTE_PATH`, and `MOUNTABLE_API_URL`. That
last name lets the checks point at a loopback server. It is not `--allow-all`.

## Cleanup

Remove this example's note with the exact original text:

```bash
deno task cleanup --text "Deno note stays."
```

Cleanup reads the file, compares the bytes, and deletes with that file's ETag.
It refuses a different body. It does not remove `notes/` or delete a filesystem.
A `402` with `filesystem_spending_cap_reached` stops billable work on that
filesystem. Other filesystems continue. See
[limits per end user](https://mountable.io/docs/end-user-limits).

To see the read-only refusal without replacing the note:

```bash
deno task readonly-write --text "Deno note stays."
```

That prints `read_only`.
