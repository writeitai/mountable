# SDK contract cases

The Python SDK (`sdk/python`) and the TypeScript SDK (`sdk/typescript`) behave
the same way. Each case below is a test in both suites, named after its ID:
`test_c07_…` in `sdk/python/tests/test_contract.py`, `test("C07 …")` in
`sdk/typescript/test/contract.test.ts`. Both suites run against a local mock
HTTP server and fail when a case listed here has no test. A change to one
SDK's public behaviour lands in the other, and here, in the same pull request.

The API the SDKs wrap is the files API (`/api/v1/filesystems/{fs}/files/{path}`,
`/list/{path}`, `/changes`), `POST /api/v1/mount-sessions` and a filesystem's
webhooks (`/api/v1/orgs/{org}/filesystems/{fs}/webhooks`). Names below are
Python's; TypeScript uses the same names in camelCase and returns promises.

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

## Webhooks

| ID | Case |
| --- | --- |
| C33 | The first webhook call asks `GET /api/v1/me` with the API key for the key's organisation (`api_key.org_id`) and keeps it for the client's later calls. `create_webhook(fs_id, url, path_prefix="/")` then sends `POST /api/v1/orgs/{org}/filesystems/{fs}/webhooks` with the API key and `{"url", "path_prefix"}`, and returns `Webhook` with the answer's fields (datetimes parsed, `null` as `None`) and its `secret`. |
| C34 | `list_webhooks` sends `GET …/webhooks` and returns a list of `Webhook`; `enable_webhook` sends `POST …/webhooks/{id}:enable` and returns `Webhook`; `delete_webhook` sends `DELETE …/webhooks/{id}`, and `204` returns nothing. Their `secret` is `None`. IDs are percent-encoded; an empty, `.` or `..` ID raises `invalid_request` (`status` `None`) without a request, since URL normalisation would send it to another resource (`…/webhooks/..` is the filesystem). |
| C35 | `rotate_webhook_secret` sends `POST …/webhooks/{id}:rotate-secret` and returns `Webhook` with the new `secret`. |
| C36 | The secret is read from `.secret` only: a returned `Webhook` does not show it when printed or inspected (nor in `JSON.stringify` in TypeScript), and an error never shows a `whsec_` secret, even when the API echoes one. |
| C37 | The webhook error codes raise `MountableError` with that `status` and `code`: `400 bad_url`, `400 bad_path`, `402 payment_required`, `403 no_grant`, `404 not_found`, `409 webhook_limit`, `409 access_revoked`, and `422` validation details as `invalid_request`. A failed `GET /me` raises its error and sends no webhook request; an answer without `api_key.org_id` raises `api_error`. |
| C38 | `list_webhooks` and the `GET /me` lookup are sent once more after a network failure; `create_webhook`, `delete_webhook`, `rotate_webhook_secret` and `enable_webhook` are never sent again. |
| C39 | `verify_webhook(secret, headers, body)` accepts a delivery whose `webhook-signature` holds a `v1` HMAC-SHA256 signature by the secret of `{webhook-id}.{webhook-timestamp}.{body}` (the shared vector `sdk/testdata/webhook.json`), with header names in any case, a body as bytes or a string, and two signatures of which only one matches, first or second (a rotation), also when a repeated header was joined with `", "` (in TypeScript, also Node's array of values). It returns `WebhookMessage`: `id` (the `webhook-id`), `type`, `timestamp`, `webhook_id`, `filesystem_id`, `events` as `Change` and `cursor`. |
| C40 | `verify_webhook` raises `invalid_webhook` (`status` `None`) for a changed body, another secret, a missing header, a timestamp more than 300 seconds before or after now or longer than 12 digits, a signature without `v1,`, a malformed secret, or a signed body that is not a message. Its message never shows the secret. |
