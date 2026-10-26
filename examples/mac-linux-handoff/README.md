# Small files between a Mac and Linux

A Linux agent writes a small result to a mountable.io filesystem. A developer
reads it on a Mac and sends a reply through the same filesystem. The script
also checks that a deliberate rename and delete on each side reach the other.

Setup, ticket handoff, mount readiness, costs and cleanup:
[the cookbook](https://mountable.io/docs/mac-linux-handoff).

Requires Python 3.10+ and two ready read-write mounts of the same filesystem.
The Mac needs Apple Silicon and macOS 26; Linux needs FUSE and mount permission.
The script uses the standard library and no model. The hosted filesystem is a
paid dependency. Keep credentials out of the script and sandbox images.

Run from local disk, outside the mount, after both CLI processes emit their
JSON `mounted` event. Start these within 20 seconds of each other:

```bash
# Linux
python3 handoff.py linux /tmp/agent-work --run-id demo-4182

# Mac
python3 handoff.py mac ~/agent-work --run-id demo-4182
```

Use a fresh run ID each time. Each side writes only its own path and checks
the exact expected bytes. Linux refuses to overwrite an existing run directory.
Both finish with `handoff complete`. Each visibility poll has a 20-second
deadline; `--timeout` accepts 1–60 seconds. Also set an overall process deadline
in your orchestrator: a blocked OS filesystem call can exceed a polling
deadline. Stop both sides on failure.

The example leaves an empty `handoff-demo-4182/` directory, possibly containing
Mac `._*` sidecars. Inspect and remove only that directory, unmount both
machines, revoke both sessions and read back filesystem usage. Follow the
cookbook's failure cleanup if a run stops early. Coordinate writers by path.
This is a small file handoff, not a shared database.

Verified 2026-10-26 with published CLI 0.8.0, macOS 26.7.1 on Apple Silicon,
and Ubuntu 24.04 arm64 in a disposable local Colima VM. Both directions passed
with the 31-byte result and 27-byte reply, including rename/delete observation. No
current-release latency measurement or hosted sandbox qualification is claimed.

```bash
python3 -m unittest discover -s . -p 'test_*.py'
```
