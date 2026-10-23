# mountable.io SDK for Python

Read, write, delete, list and follow the files of a
[mountable.io](https://mountable.io) filesystem over HTTPS from a backend,
without mounting it, create one-time mount tickets for sandboxes, and manage
filesystems, their limits, mount sessions and webhooks.

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

## Managing filesystems

```python
client = Mountable()  # org_id: MOUNTABLE_ORG, else the API key's organisation

info = client.create_filesystem(
    "agent-workspace", spending_cap_cents=2_000, byte_quota=5 * 2**30
)
client.update_filesystem(info.id, mutations_per_minute=None)  # the platform default
print(client.filesystem_usage(info.id).retained_bytes)

hook = client.create_webhook(
    info.id, "https://example.com/hooks/mountable", path_prefix="/tickets"
)
save_secret(hook.secret)  # whsec_…, returned only here and by rotate_webhook_secret
```

| Method | Returns |
| --- | --- |
| `list_filesystems()` | `list[FilesystemInfo]` |
| `create_filesystem(name, *, spending_cap_cents, mutations_per_minute, byte_quota, entry_quota)` | `FilesystemInfo`; limits not given take the API's defaults |
| `get_filesystem(fs_id)` | `FilesystemInfo` |
| `update_filesystem(fs_id, *, spending_cap_cents, mutations_per_minute, byte_quota, entry_quota)` | `FilesystemInfo`; only the limits passed change, `None` removes the spending cap or the mutation rate |
| `filesystem_usage(fs_id)` | `FilesystemUsage` |
| `list_mount_sessions(fs_id)` | `list[MountSession]` |
| `revoke_mount_session(session_id)` | `None` |
| `create_webhook(fs_id, url, *, path_prefix)` | `Webhook` with `.secret` |
| `list_webhooks(fs_id)` | `list[Webhook]` (`.secret` `None`) |
| `delete_webhook(fs_id, webhook_id)` | `None` |
| `rotate_webhook_secret(fs_id, webhook_id)` | `Webhook` with the new `.secret` |
| `enable_webhook(fs_id, webhook_id)` | `Webhook` |

These calls use the API key, and the API checks what it may do:

- An API key limited to one filesystem cannot create or update filesystems
  (`403 key_limited_to_filesystem`); creating and updating also need the
  key's creator to be an owner or admin of the organisation.
- Updating a filesystem and every webhook call, listing included, need
  read-write access to the filesystem.
- A key lists only its own live sessions and revokes only its own sessions.
- The API checks the ranges: `spending_cap_cents` 0–1,000,000,000 or `None`
  (no cap), `mutations_per_minute` 1–1,000 or `None` (the platform default),
  `byte_quota` 1 GiB–100 GiB, `entry_quota` 10,000–1,000,000. The two quotas
  cannot be removed.

## Behaviour

- The handle creates a one-hour token session on its first call and keeps
  the token in memory only. A `401`, or a session within 60 seconds of its
  expiry, makes it create a new one and send the call once more.
- Every failure raises `MountableError` with `status`, the API's stable
  `code`, `message`, `current_etag` (for `precondition_failed`) and
  `retry_after`. Network failures have `code == "network_error"`. API keys,
  tokens and webhook secrets never appear in errors.
- Only a `2xx` answer is a success; redirects are not followed.
- Only reads (`read`, `list`, `changes`, and the `get_`, `list_` and
  `filesystem_usage` calls) and the repeatable `revoke_mount_session` and
  `enable_webhook` are retried, once, after a network failure. `write`,
  `delete`, `create_filesystem`, `update_filesystem`, `create_webhook`,
  `rotate_webhook_secret` and `delete_webhook` are never retried: after a
  timeout the outcome is unknown, and a conditional write makes your own retry
  safe.
- A request fails with `network_error` after 60 seconds without an answer;
  `changes` waits 60 seconds plus `wait`.
- `create_mount_ticket` returns the ticket once, in `.ticket`; it is left out
  of the result's `repr`. Reuse your
  `idempotency_key` to retry after an unknown outcome; a replay returns the
  session with `ticket` `None`. A `Webhook`'s `.secret` is left out of its
  `repr` the same way.

Documentation: <https://mountable.io/docs>. The behaviour shared with the
TypeScript SDK is listed in
[`sdk/contract.md`](https://github.com/writeitai/mountable/blob/main/sdk/contract.md).

## License

[Apache-2.0](LICENSE).
