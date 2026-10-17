# Showcase demos

Two demos for the Multi-Sandbox Agent Relay and the revocation kill switch.
They are stdlib Python and shell out to the `mountable` CLI.

Anonymous playback on [mountable.io/showcase](https://mountable.io/showcase) uses
the illustrated sequences from `--dry-run`. That page does not create a
filesystem or call the API.

## Preview without a filesystem

```sh
python3 examples/showcase/relay.py --dry-run
python3 examples/showcase/kill_switch.py --dry-run
python3 examples/showcase/test_showcase.py
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
