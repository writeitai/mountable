# mountable.io SDK for Python

Read, write, delete, list and follow the files of a
[mountable.io](https://mountable.io) filesystem over HTTPS from a backend,
without mounting it, create one-time mount tickets for sandboxes, and manage
filesystems, their limits, keys, grants, mount sessions and webhooks.

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

A management key (`mtblm_…`, created by an owner or admin in the console)
creates and deletes filesystems, sets their limits, and creates API keys, S3
keys and grants. An API key (`mtbl_…`) accesses files: token sessions, mount
tickets, sessions and webhooks. Both go in `api_key`; the API refuses the
other kind with `403 management_key_required` or `403 api_key_required`.

```python
# org_id: MOUNTABLE_ORG, else the key's organisation
admin = Mountable(api_key=os.environ["MOUNTABLE_MANAGEMENT_KEY"])

info = admin.create_filesystem(
    "agent-workspace", spending_cap_cents=2_000, byte_quota=5 * 2**30
)
admin.update_filesystem(info.id, mutations_per_minute=None)  # the platform default
print(admin.filesystem_usage(info.id).retained_bytes)
key = admin.create_api_key("backend", filesystem_id=info.id, mode="rw")
save_secret(key.secret)  # mtbl_…, returned only here
s3 = admin.create_s3_key(info.id, "duckdb", mode="ro", expires_in_days=90)
save_secret(s3.access_key_id, s3.secret_access_key)  # returned only here

client = Mountable(api_key=key.secret)
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
| `delete_filesystem(fs_id)` | `None`; the filesystem, its files, sessions and S3 keys go at once |
| `create_api_key(name, *, expires_in_days, filesystem_id, mode)` | `ApiKey` with `.secret`; with `filesystem_id`, limited to that filesystem |
| `list_api_keys()` | `list[ApiKey]` (`.secret` `None`) |
| `revoke_api_key(key_id)` | `None` |
| `create_s3_key(fs_id, name, *, mode, expires_in_days)` | `S3Key` with `.access_key_id` and `.secret_access_key` |
| `list_s3_keys(fs_id)` | `list[S3Key]` (`.secret_access_key` `None`) |
| `revoke_s3_key(fs_id, access_key_id)` | `None` |
| `list_grants(fs_id)` | `list[Grant]` |
| `set_grant(fs_id, principal_type, principal_id, mode)` | `Grant`; creates it or changes its mode |
| `delete_grant(fs_id, grant_id)` | `None` |

An S3 key works with any S3 client at `https://s3.mountable.io`, path-style,
with the filesystem ID as the bucket. Key secrets are kept out of `repr`.
Creating keys and deleting filesystems or grants are never retried after a
network failure; list them to see what happened.

These calls use the key, and the API checks what it may do:

- Creating, updating and deleting filesystems, keys and grants need a
  management key whose creator is still an owner or admin
  (`403 management_key_required` for an API key).
- Updating a filesystem and every webhook call, listing included, need
  read-write access to the filesystem.
- A key lists only its own live sessions and revokes only its own sessions.
- The API checks the ranges: `spending_cap_cents` 0–1,000,000,000 or `None`
  (no cap), `mutations_per_minute` 1–1,000 or `None` (the platform default),
  `byte_quota` 1 GiB–100 GiB, `entry_quota` 10,000–1,000,000. The two quotas
  cannot be removed.
- An empty, `.` or `..` filesystem, session, webhook, key or grant ID, or a `.` or `..`
  organisation ID, raises `invalid_request` before any request: URL
  normalisation would send it to another resource. An empty `org_id` counts
  as not given.

## Receiving webhooks

In the endpoint, verify the signature on the raw body before anything else:

```python
from mountable_sdk import MountableError, verify_webhook


def receive(headers, body: bytes) -> int:
    try:
        message = verify_webhook(secret, headers, body)
    except MountableError:  # code "invalid_webhook"
        return 400
    if int(message.cursor) <= last_applied_cursor():  # a duplicate or a late message
        return 204
    if message.type == "files.resync":
        ...  # list what you need again; events after message.cursor follow
    for event in message.events:  # invalidations, as from fs.changes()
        print(event.type, event.path, event.file and event.file.etag)
    save_applied_cursor(message.cursor)
    return 204
```

`verify_webhook` returns a `WebhookMessage` (`id`, the `webhook-id` that a
retry repeats; `type`; `timestamp`; `webhook_id`; `filesystem_id`; `events` as
`Change`; `cursor`). It accepts either signature a rotation sends and refuses
a timestamp more than five minutes from now. Answer within 10 seconds; a
message can arrive more than once, so apply it only when its `cursor`,
compared as an integer, is greater than the last one applied.

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
