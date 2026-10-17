# SDK contract cases

The Python SDK (`sdk/python`) and the TypeScript SDK (`sdk/typescript`) behave
the same way. Each case below is a test in both suites, named after its ID:
`test_c07_…` in `sdk/python/tests/test_contract.py`, `test("C07 …")` in
`sdk/typescript/test/contract.test.ts`. Both suites run against a local mock
HTTP server and fail when a case listed here has no test. A change to one
SDK's public behaviour lands in the other, and here, in the same pull request.

The API the SDKs wrap is the files API (`/api/v1/filesystems/{fs}/files/{path}`,
`/list/{path}`, `/changes`) and `POST /api/v1/mount-sessions`. Names below are
Python's; TypeScript uses the same names in camelCase and returns promises.

## Operations

| ID | Case |
| --- | --- |
| C01 | `write` sends `PUT /files/{path}` with the bytes as `application/octet-stream` and returns the answer's `FileInfo` (`201` created). A string is sent as UTF-8. |
| C02 | `write` over an existing file returns the answer's `FileInfo` (`200` replaced). |
| C03 | `read` sends `GET /files/{path}` and returns `File`: `.data` is the body, `.info` comes from `ETag`, `Content-Length` and `Last-Modified` (path with a leading `/`, type `file`). |
| C04 | `delete` sends `DELETE /files/{path}`; `204` returns nothing. |
| C05 | `list` sends `GET /list/{path}` with `after` and `limit` only when given (`list()` is `GET /list/`) and returns `ListPage`: `.entries` as `FileInfo` with parsed datetimes, `.next` the API's (`None` on the last page). |
| C06 | `changes()` without a cursor sends `GET /changes` with no query and returns no events and the API's cursor. |
| C07 | `changes(cursor, wait)` sends `cursor` and `wait` (default 25) and returns the events: `type`, `path`, and `file` as `FileInfo` or `None`. |
| C08 | Paths are percent-encoded per segment (a space, `%`, `#`, `?`, non-ASCII and `!'()*` are encoded; `/` separates segments); leading and trailing `/` are dropped. A `.` or `..` segment raises `bad_path` without a request (URL normalisation would resolve it). |
| C09 | `create_mount_ticket` sends `POST /api/v1/mount-sessions` with the API key and `filesystem_id`, `mode`, `idempotency_key` (random when not given) and `max_duration_seconds` only when given, and returns `id`, `ticket`, `expires_at` and the session's other fields. A replay's `ticket` is `None`. |

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
| C15 | Later operations on the handle reuse the session. |
| C16 | A `401` from an operation creates a new session and sends that operation once more with the new token — a read, and a write with its whole body. |
| C17 | A `401` after the new session raises `unauthenticated`: one refresh per call, no loop. |
| C18 | A session within 60 seconds of its `expires_at` is replaced before the next operation. |
| C19 | A failed session creation raises its error (for example `403 no_grant`) and sends no operation. |

## Errors

| ID | Case |
| --- | --- |
| C20 | Every error code of the files API raises `MountableError` with that `status`, `code` and a `message`: `400 bad_path`, `401 unauthenticated`, `403 read_only`, `403 token_needs_key_or_cli`, `404 not_found`, `409 is_a_directory`, `409 not_a_regular_file`, `409 not_a_directory`, `409 directory_not_empty`, `409 conflict`, `409 quota_exceeded`, `410 cursor_expired`, `412 precondition_failed`, `413 too_large`, `429 rate_limited`, `429 download_quota`, `501 unsupported_file`, `503 changes_unavailable`. The code is read from `{"detail": {"code", "message"}}` or the same fields at the top level. |
| C21 | `Retry-After` sets `retry_after` (seconds). |
| C22 | A network failure raises `network_error` with `status` `None`. |
| C23 | The API key and the session token never appear in an error's message or `repr`, even when the API echoes them. |
| C24 | An error answer without a code raises `api_error` with its status; `422` validation details raise `invalid_request`. |

## Retries

| ID | Case |
| --- | --- |
| C25 | `read`, `list` and `changes` are sent once more after a network failure, including a read whose body ends short of `Content-Length`; a second failure raises `network_error`. |
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
| C30 | `api_key` defaults to `MOUNTABLE_API_KEY`, `base_url` to `MOUNTABLE_URL`, then `https://mountable.io`; arguments win. Without a key the constructor raises `unauthenticated`. |
| C31 | Requests time out after 60 seconds; `changes` after 60 seconds plus `wait`. |
