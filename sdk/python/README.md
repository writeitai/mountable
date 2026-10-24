# mountable.io SDK for Python

Read, write, delete, list and follow the files of a
[mountable.io](https://mountable.io) filesystem over HTTPS from a backend,
without mounting it, create one-time mount tickets for sandboxes, and manage
and verify the webhooks that call your backend when files change.

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

## Webhooks

A webhook calls your HTTPS endpoint with a filesystem's changes. Creating one
needs an API key with read-write access to the filesystem.

```python
hook = client.create_webhook(
    "fs_…", "https://example.com/hooks/mountable", path_prefix="/tickets"
)
store_secret(hook.id, hook.secret)  # whsec_…, shown only now; not in repr(hook)

client.list_webhooks("fs_…")  # [Webhook]: .state, .disabled_reason, .last_failure
rotated = client.rotate_webhook_secret("fs_…", hook.id)
store_secret(hook.id, rotated.secret)  # the old secret signs for 24 more hours
client.enable_webhook("fs_…", hook.id)  # after it was disabled as "failing"
client.delete_webhook("fs_…", hook.id)
```

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

`verify_webhook` accepts any of the signatures a rotation sends and refuses
a timestamp more than five minutes from now. Answer within 10 seconds; a
message can arrive more than once, so apply it only when its `cursor`,
compared as an integer, is greater than the last one applied.

## Behaviour

- The handle creates a one-hour token session on its first call and keeps
  the token in memory only. A `401`, or a session within 60 seconds of its
  expiry, makes it create a new one and send the call once more.
- Every failure raises `MountableError` with `status`, the API's stable
  `code`, `message`, `current_etag` (for `precondition_failed`) and
  `retry_after`. Network failures have `code == "network_error"`. API keys and
  tokens never appear in errors.
- Only a `2xx` answer is a success; redirects are not followed.
- Only reads (`read`, `list`, `changes`, `list_webhooks`) are retried, once,
  after a network failure. `write` and `delete` are never retried: after a
  timeout the outcome is unknown, and a conditional write makes your own retry
  safe. Nor are the webhook calls that change something: after an unknown
  outcome of `create_webhook`, look for the URL in `list_webhooks` first;
  after one of `rotate_webhook_secret`, the secret may have changed and is
  not shown again, so rotate again and store the new one.
- The webhook calls look up the API key's organisation once per client
  (`GET /api/v1/me`).
- A request fails with `network_error` after 60 seconds without an answer;
  `changes` waits 60 seconds plus `wait`.
- `create_mount_ticket` returns the ticket once, in `.ticket`; it is left out
  of the result's `repr`. Reuse your
  `idempotency_key` to retry after an unknown outcome; a replay returns the
  session with `ticket` `None`.
- `create_webhook` and `rotate_webhook_secret` return the signing secret
  once, in `.secret`, left out of `repr`; webhook secrets never appear in
  errors.

Documentation: <https://mountable.io/docs>. The behaviour shared with the
TypeScript SDK is listed in
[`sdk/contract.md`](https://github.com/writeitai/mountable/blob/main/sdk/contract.md).

## License

[Apache-2.0](LICENSE).
