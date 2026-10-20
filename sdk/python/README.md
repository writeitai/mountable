# Mountable SDK for Python

Read, write, delete, list and follow the files of a
[Mountable](https://mountable.io) filesystem over HTTPS from a backend,
without mounting it, and create one-time mount tickets for sandboxes.

```sh
pip install mountable-sdk
```

Python 3.10+, synchronous, one dependency (`httpx`).

## Example

```python
from mountable_sdk import Mountable, MountableError

client = Mountable()  # MOUNTABLE_API_KEY; MOUNTABLE_API_URL or https://mountable.io
fs = client.filesystem("fs_…", mode="rw")  # sends nothing until first used

info = fs.write("tickets/AB-42.md", "# AB-42\n")
fs.write("tickets/AB-42.md", "# AB-42, edited\n", if_match=info.etag)
try:
    fs.write("tickets/AB-42.md", "lost update", if_match=info.etag)
except MountableError as e:
    if e.code == "precondition_failed":
        print("changed meanwhile; now", e.current_etag)

file = fs.read("tickets/AB-42.md")  # file.data (bytes), file.info (FileInfo)
fs.delete("tickets/AB-42.md", if_match=file.info.etag)

ticket = client.create_mount_ticket(
    "fs_…", mode="ro", max_duration_seconds=12 * 3600, idempotency_key="job-42"
)
# hand ticket.ticket to the sandbox: mountable mount --ticket-stdin DIR


# Stay in sync: take a cursor first, then list, then follow changes from it.
def resync() -> str:
    cursor = fs.changes().cursor  # 1. a cursor first
    page = fs.list("tickets")  # 2. then list (page.next pages on)
    for entry in page.entries:
        print(entry.path, entry.etag)
    return cursor


cursor = resync()
while True:  # 3. then follow changes from the cursor
    try:
        batch = fs.changes(cursor=cursor, wait=25)
    except MountableError as e:
        if e.code != "cursor_expired":
            raise
        cursor = resync()  # history is gone: start over
        continue
    for event in batch.events:  # invalidations: reconcile each path to event.file
        print(event.type, event.path, event.file and event.file.etag)
    cursor = batch.cursor
```

## Behaviour

- The handle creates a one-hour token session on its first call and keeps
  the token in memory only. A `401`, or a session within 60 seconds of its
  expiry, makes it create a new one and send the call once more.
- Every failure raises `MountableError` with `status`, the API's stable
  `code`, `message`, `current_etag` (for `precondition_failed`) and
  `retry_after`. Network failures have `code == "network_error"`. API keys and
  tokens never appear in errors.
- Only a `2xx` answer is a success; redirects are not followed.
- Only reads (`read`, `list`, `changes`) are retried, once, after a network
  failure. `write` and `delete` are never retried: after a timeout the outcome
  is unknown, and a conditional write makes your own retry safe.
- A request fails with `network_error` after 60 seconds without an answer;
  `changes` waits 60 seconds plus `wait`.
- `create_mount_ticket` returns the ticket once, in `.ticket`; it is left out
  of the result's `repr`. Reuse your
  `idempotency_key` to retry after an unknown outcome; a replay returns the
  session with `ticket` `None`.

Documentation: <https://mountable.io/docs>. The behaviour shared with the
TypeScript SDK is listed in
[`sdk/contract.md`](https://github.com/writeitai/mountable/blob/main/sdk/contract.md).

## License

[Apache-2.0](LICENSE).
