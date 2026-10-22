# Showcase demos

Demos for a shared folder across sandboxes, revoking a mount, a monthly
spend cap per end user, and signed webhooks when files change. They are
stdlib Python. The mount demos shell out to the `mountable` CLI.

Anonymous playback on [mountable.io/showcase](https://mountable.io/showcase) uses
the illustrated sequences from `--dry-run`. That page does not create a
filesystem or call the API.

## Preview without a filesystem

```sh
python3 examples/showcase/relay.py --dry-run
python3 examples/showcase/kill_switch.py --dry-run
python3 examples/showcase/spend_cap.py --dry-run
python3 examples/showcase/webhooks.py --dry-run
python3 examples/showcase/test_showcase.py
python3 examples/showcase/test_limits.py
```

`--dry-run` prints the terminal panes and can write the same JSON the website
plays (`--trace path.json`). The clock in that JSON is an illustration. It is
not a measurement. The kill-switch contract stays the published one: no new
operation is admitted more than 15 seconds after revocation, a mutation
already admitted may finish, and the CLI aborts the mount within about 20
seconds. An E2B production check on CLI 0.3.0 (2026-10-07) saw the mount stop
in 2–4 seconds. CLI 0.4.0 has not been re-run there.

## Run it against Mountable

Needs Linux or macOS, FUSE, the `mountable` CLI on `PATH`, and
`MOUNTABLE_API_KEY` on this machine. Sandbox mounts receive only a one-time
ticket. The laptop step uses `mountable mount --ro` with that key.

```sh
export MOUNTABLE_API_KEY=mtbl_…
export MOUNTABLE_FS_ID=your_filesystem_id   # or pass --create
python3 examples/showcase/relay.py --fs "$MOUNTABLE_FS_ID" --trace /tmp/relay.json
python3 examples/showcase/kill_switch.py --fs "$MOUNTABLE_FS_ID" --trace /tmp/kill-switch.json
```

`--create` makes a filesystem when `--fs` is omitted. The CLI cannot delete
it; delete it in the console when you are done, or it continues to count as
stored usage. A live run also leaves mount sessions until they expire or you
revoke them (`mountable sessions list`, `mountable sessions revoke`).

`--trace` from a live run has `"source": "recorded"` and, for the kill switch,
`measured_abort_ms`. Do not present that clock as the 2026-10-07 E2B result.

## One user hits a monthly spend cap

Mountable lets your backend set a monthly spend cap on each end user's
filesystem. When that user reaches the cap, their billable work stops with
`402 filesystem_spending_cap_reached`. Other users keep reading and writing.

```sh
python3 examples/showcase/spend_cap.py --org "$MOUNTABLE_ORG_ID" --trace /tmp/spend-cap.json
```

Needs `MOUNTABLE_API_KEY` for an organisation with a saved payment method,
and `MOUNTABLE_ORG_ID` (`mountable orgs list`). The CLI cannot set a spend
cap, so this demo calls the REST API.

User A's filesystem is created with a 0-cent monthly cap. The first billable
write is refused, and the loop stops. User B's filesystem is created with a
500-cent cap. One small file is written and read back. That write is billed.

Delete both filesystems in the console when you are done. Stored files keep
counting as usage until you delete them. The API cannot delete a filesystem.

Guide: [Limits per end user](https://mountable.io/docs/end-user-limits?utm_source=viral-showcase-demos&utm_medium=showcase_pr&utm_content=showcase-02-spend-cap).

## Signed webhook when a file changes

A signed webhook tells your backend when a file is created, updated, or
deleted. A rename arrives as `deleted` for the old path and `created` for
the new path.

```sh
python3 examples/showcase/webhooks.py --org "$MOUNTABLE_ORG_ID" \
  --webhook-url https://YOUR-HOST/hooks/mountable \
  --trace /tmp/webhooks.json
```

`--dry-run` also checks a locally signed delivery. It creates no filesystem
and makes no network call.

A live run needs Linux, FUSE, the `mountable` CLI on `PATH`, the same API
key and organisation, and a public HTTPS URL on port 443 that forwards to
`127.0.0.1:8787/hooks/mountable` on this machine. `--port` changes the local
port. The proxy must pass the raw body and the `webhook-id`,
`webhook-timestamp`, and `webhook-signature` headers unchanged. The receiver
listens on loopback only.

The demo creates one filesystem with a 100-cent monthly cap. Mountable's
first message is a `files.resync`. The script applies that message, then
mounts the filesystem and creates `notes/draft.txt`, writes it again, and
renames it to `notes/report.txt`. Those writes are billed.

The receiver checks the signature on the raw body. It applies a message
only when `data.cursor`, compared as an integer, is greater than the last
cursor it applied. Delete the filesystem in the console when you are done.

Guide: [Webhooks](https://mountable.io/docs/webhooks?utm_source=viral-showcase-demos&utm_medium=showcase_pr&utm_content=showcase-03-file-webhooks).
