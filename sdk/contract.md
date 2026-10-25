# SDK contract cases

The Python SDK (`sdk/python`) and the TypeScript SDK (`sdk/typescript`) behave
the same way. Each case below is a test in both suites, named after its ID:
`test_c07_…` in `sdk/python/tests/test_contract.py`, `test("C07 …")` in
`sdk/typescript/test/contract.test.ts`. Both suites run against a local mock
HTTP server and fail when a case listed here has no test. A change to one
SDK's public behaviour lands in the other, and here, in the same pull request.

The API the SDKs wrap is the files API (`/api/v1/filesystems/{fs}/files/{path}`,
`/list/{path}`, `/changes`), `POST /api/v1/mount-sessions` and the management
calls (`/api/v1/orgs/{org}/filesystems/…`, `DELETE /api/v1/mount-sessions/{id}`).
Names below are Python's; TypeScript uses the same names in camelCase and
returns promises.

## Operations

| ID | Case |
| --- | --- |
| C01 | `write` sends `PUT /files/{path}` with the bytes as `application/octet-stream` and returns the answer's `FileInfo` (`201` created). A string is sent as UTF-8. |
| C02 | `write` over an existing file returns the answer's `FileInfo` (`200` replaced). |
| C03 | `read` sends `GET /files/{path}` and returns `File`: `.data` is the body, `.info` comes from `ETag`, `Content-Length` and `Last-Modified` (path with a leading `/`, type `file`). |
| C04 | `delete` sends `DELETE /files/{path}`; `204` returns nothing. |
| C05 | `list` sends `GET /list/{path}` with `after` and `limit` only when given (`list()` is `GET /list/`) and returns `ListPage`: `.entries` as `FileInfo` with parsed datetimes (`etag` `None` for a directory, whose JSON omits it), `.next` the API's (`None` on the last page). |
| C06 | `changes()` without a cursor sends `GET /changes` with no query and returns no events and the API's cursor. |
| C07 | `changes(cursor, wait)` sends `cursor` and `wait` (default 25) and returns the events: `type`, `path`, and `file` as `FileInfo` or `None`. |
| C08 | Paths are percent-encoded per segment (a space, `%`, `#`, `?`, non-ASCII and `!'()*` are encoded; `/` separates segments); leading and trailing `/` are dropped. A `.` or `..` segment raises `bad_path` without a request (URL normalisation would resolve it). |
| C09 | `create_mount_ticket` sends `POST /api/v1/mount-sessions` with the API key and `filesystem_id`, `mode`, `idempotency_key` (random when not given) and `max_duration_seconds` only when given, and returns `id`, `ticket`, `expires_at`, `ticket_expires_at`, `filesystem_id`, `mode` and `state`. A replay's `ticket` is `None`. |

## Conditional writes

| ID | Case |
| --- | --- |
| C10 | `write(if_match=etag)` sends `If-Match` with the ETag quoted (an ETag already quoted is sent as it is). |
| C11 | `write(if_none_match=True)` sends `If-None-Match: *`. |
| C12 | `delete(if_match=etag)` sends `If-Match`. |
| C13 | `412 precondition_failed` raises `MountableError` with `current_etag` (the API's string, or `None` when nothing exists). |

## Token sessions

| ID | Case |
| --- | --- |
| C14 | `filesystem(id, mode)` sends nothing. The first operation creates a token session: `POST /api/v1/mount-sessions` with `Authorization: Bearer <API key>` and `filesystem_id`, `mode`, `max_duration_seconds: 3600`, a random `idempotency_key` and `credential: "token"`; the operation sends `Authorization: Bearer <token>`. |
| C15 | Later operations on the handle reuse the session. In TypeScript, concurrent first operations on a new handle share one new session. |
| C16 | A `401` from an operation creates a new session and sends that operation once more with the new token — a read, and a write with its whole body. |
| C17 | A `401` after the new session raises `unauthenticated`: one refresh per public call, no loop. A read's network retry does not grant another: `401`, connection dropped, `401` raises. |
| C18 | A session within 60 seconds of its `expires_at` is replaced before the next operation. |
| C19 | A failed session creation raises its error (for example `403 no_grant`) and sends no operation. |

## Errors

| ID | Case |
| --- | --- |
| C20 | Every error code of the files API raises `MountableError` with that `status`, `code` and a `message`: `400 bad_path`, `401 unauthenticated`, `403 read_only`, `403 token_needs_key_or_cli`, `404 not_found`, `409 is_a_directory`, `409 not_a_regular_file`, `409 not_a_directory`, `409 directory_not_empty`, `409 conflict`, `409 quota_exceeded`, `410 cursor_expired`, `412 precondition_failed`, `413 too_large`, `429 rate_limited`, `429 download_quota`, `501 unsupported_file`, `503 changes_unavailable`. The code is read from `{"detail": {"code", "message"}}` or the same fields at the top level. |
| C21 | `Retry-After` sets `retry_after` (seconds). |
| C22 | A network failure raises `network_error` with `status` `None`. |
| C23 | The API key and the session token never appear in an error's message or `repr`, even when the API echoes them; the client, the handle and a returned `MountTicket` do not show a secret when printed or inspected (the ticket is read from `.ticket`). |
| C24 | An error answer without a code raises `api_error` with its status; `422` validation details raise `invalid_request`. |

## Retries

| ID | Case |
| --- | --- |
| C25 | `read`, `list` and `changes` are sent once more after a network failure, including a read whose body ends short of `Content-Length` after the headers arrived; a second failure raises `network_error`. |
| C26 | `write` and `delete` are never sent again after a network failure. |
| C27 | HTTP errors (for example `500`, `503`) are not retried. |

## Change feed

| ID | Case |
| --- | --- |
| C28 | The cursor an answer returns is the one to send next; an answer without events returns the given cursor, and the SDK passes it on unchanged. |
| C29 | `410 cursor_expired` raises `cursor_expired`; recovering takes a new cursor first, then lists, then follows changes from that cursor. |

## Configuration

| ID | Case |
| --- | --- |
| C30 | `api_key` defaults to `MOUNTABLE_API_KEY`, `base_url` to `MOUNTABLE_API_URL`, then `https://mountable.io`; arguments win. Without a key the constructor raises `unauthenticated`. |
| C31 | A request fails with `network_error` (`status` `None`) when the API is silent for 60 seconds (connecting, or waiting for the answer or more of its body); for `changes`, 60 seconds plus `wait`. A read that times out is sent once more, a write is not. The tests shorten the timeout to observe it. TypeScript's `fetch` reports no upload progress, so there the time to send a body counts toward the first 60 seconds. |

## Redirects

| ID | Case |
| --- | --- |
| C32 | Only a `2xx` answer is a success. A `3xx` answer is not followed and raises `MountableError` with its status (`api_error` without a code), for file operations and session creation alike. |

## Management

Management calls send the API key, not a token session. Below, `…` is
`/api/v1/orgs/{org_id}`.

| ID | Case |
| --- | --- |
| C33 | `org_id` defaults to `MOUNTABLE_ORG`, then to the API key's organisation (`api_key.org_id` of `GET /api/v1/me`), read once on the first organisation call and kept; in TypeScript, concurrent first calls share one request. The argument wins. |
| C34 | When `/me` has no `api_key.org_id`, an organisation call raises `org_required` (`status` `None`) and sends nothing more. An error from `/me` raises as it is. |
| C35 | `list_filesystems` sends `GET …/filesystems` and `get_filesystem` sends `GET …/filesystems/{fs_id}`, both with `Authorization: Bearer <API key>` and no token session; they return `FilesystemInfo` (`spending_cap_cents` and `mutations_per_minute` `None` when null, `created_at` parsed). |
| C36 | `create_filesystem(name, …)` sends `POST …/filesystems` with `name` and only the limits given (`0` is given), and returns the answer's `FilesystemInfo`. |
| C37 | `update_filesystem(fs_id, …)` sends `PATCH …/filesystems/{fs_id}` with only the fields passed; `None` for `spending_cap_cents` or `mutations_per_minute` sends JSON `null`. It returns the answer's `FilesystemInfo`. |
| C38 | `filesystem_usage` sends `GET …/filesystems/{fs_id}/usage` and returns `FilesystemUsage` (`currency` `None` when null). |
| C39 | `list_mount_sessions` sends `GET …/filesystems/{fs_id}/mount-sessions` and returns `MountSession`s with parsed datetimes. `revoke_mount_session` sends `DELETE /api/v1/mount-sessions/{session_id}`, which needs no organisation, and returns nothing. |
| C40 | `create_webhook(fs_id, url)` sends `POST …/filesystems/{fs_id}/webhooks` with `url`, and `path_prefix` only when given, and returns a `Webhook` with the signing secret in `.secret`. `list_webhooks` sends `GET` there and returns `Webhook`s: `secret` `None`, nullable fields `None`, datetimes parsed. |
| C41 | `delete_webhook` sends `DELETE …/webhooks/{webhook_id}` and returns nothing; `rotate_webhook_secret` sends `POST …/webhooks/{webhook_id}:rotate-secret` and returns the `Webhook` with the new `.secret`; `enable_webhook` sends `POST …/webhooks/{webhook_id}:enable` and returns the `Webhook`. |
| C42 | Organisation, filesystem, session and webhook IDs are percent-encoded as path segments. |
| C43 | A `Webhook` does not show its secret when printed or inspected. A `whsec_` secret the API echoes in an error never appears in its message or `repr`. |
| C44 | `/me`, `list_filesystems`, `get_filesystem`, `filesystem_usage`, `list_mount_sessions`, `list_webhooks`, `revoke_mount_session` and `enable_webhook` are sent once more after a network failure; a second failure raises `network_error`. |
| C45 | `create_filesystem`, `update_filesystem`, `create_webhook`, `rotate_webhook_secret` and `delete_webhook` are never sent again after a network failure. |
| C46 | API errors raise `MountableError` with their status and code, for example `403 key_limited_to_filesystem` from `create_filesystem` and `update_filesystem`, `422` validation details (`invalid_request`) from `update_filesystem`, a `403` from `list_webhooks` and `404 not_found` from `delete_webhook`. |
| C47 | An empty, `.` or `..` filesystem, session or webhook ID, or a `.` or `..` organisation ID (an empty `org_id` counts as not given), raises `invalid_request` (`status` `None`) before any request, the `/me` lookup included: URL normalisation would send it to another resource (`…/webhooks/..` is the filesystem). |

## Webhook deliveries

| ID | Case |
| --- | --- |
| C48 | `verify_webhook(secret, headers, body)` accepts a delivery whose `webhook-signature` holds a `v1` HMAC-SHA256 signature by the secret of `{webhook-id}.{webhook-timestamp}.{body}` (the shared vector `sdk/testdata/webhook.json`), with header names in any case, a body as bytes or a string, and two signatures of which only one matches, first or second (a rotation), also when a repeated header was joined with `", "` (in TypeScript, also Node's array of values). It returns `WebhookMessage`: `id` (the `webhook-id`), `type`, `timestamp`, `webhook_id`, `filesystem_id`, `events` as `Change` and `cursor`. |
| C49 | `verify_webhook` raises `invalid_webhook` (`status` `None`) for a changed body, another secret, a missing header, a timestamp more than 300 seconds before or after now or longer than 12 digits, a signature without `v1,`, a malformed secret, or a signed body that is not a message, also one whose `cursor` is not a decimal integer (ASCII digits), or whose events lack a string `type` or `path` or have a `file` that is neither `null` nor a FileInfo with string `path`, `type` and `modified_at` (a valid time), integer `size` and string or `null` `etag` (the gateway's reference signature of a non-message body passes the signature check and fails here). Its message never shows the secret. |
