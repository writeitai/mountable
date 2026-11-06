# mountable.io SDK for TypeScript

Read, write, delete, list and follow the files of a
[mountable.io](https://mountable.io) filesystem over HTTPS from a backend,
without mounting it, create one-time mount tickets for sandboxes, and manage
filesystems, their limits, keys, grants, mount sessions and webhooks.

```sh
npm install mountable-sdk
```

An ES module with type declarations for Node 18+, Bun and Deno. It uses the
platform `fetch` and has no runtime dependency. API keys belong on servers:
it is not meant for browsers.

## Example

```ts
import { Mountable, MountableError } from "mountable-sdk";

const client = new Mountable(); // MOUNTABLE_API_KEY; MOUNTABLE_API_URL or https://mountable.io
const fs = client.filesystem("fs_…", "rw"); // sends nothing until first used

const info = await fs.write("tickets/AB-42.md", "# AB-42\n");
await fs.write("tickets/AB-42.md", "# AB-42, edited\n", { ifMatch: info.etag });
try {
  await fs.write("tickets/AB-42.md", "lost update", { ifMatch: info.etag });
} catch (e) {
  if (e instanceof MountableError && e.code === "precondition_failed") {
    console.log("changed meanwhile; now", e.currentEtag);
  }
}

const file = await fs.read("tickets/AB-42.md"); // file.data (Uint8Array), file.info (FileInfo)
await fs.delete("tickets/AB-42.md", { ifMatch: file.info.etag });

const ticket = await client.createMountTicket("fs_…", {
  mode: "ro",
  maxDurationSeconds: 12 * 3600,
  idempotencyKey: "job-42",
});
// hand ticket.ticket to the sandbox: mountable mount --ticket-stdin DIR

// Stay in sync: take a cursor first, then list, then follow changes from it.
async function resync(): Promise<string> {
  const { cursor } = await fs.changes(); // 1. a cursor first
  const page = await fs.list("tickets"); // 2. then list (page.next pages on)
  for (const entry of page.entries) console.log(entry.path, entry.etag);
  return cursor;
}

let cursor = await resync();
for (;;) {
  // 3. then follow changes from the cursor
  let batch;
  try {
    batch = await fs.changes({ cursor, wait: 25 });
  } catch (e) {
    if (!(e instanceof MountableError) || e.code !== "cursor_expired") throw e;
    cursor = await resync(); // history is gone: start over
    continue;
  }
  for (const event of batch.events) {
    // invalidations: reconcile each path to event.file
    console.log(event.type, event.path, event.file?.etag);
  }
  cursor = batch.cursor;
}
```

## Managing filesystems

A management key (`mtblm_…`, created by an owner or admin in the console)
creates and deletes filesystems, sets their limits, and creates API keys, S3
keys and grants. An API key (`mtbl_…`) accesses files: token sessions, mount
tickets, sessions and webhooks. Both go in `apiKey`; the API refuses the
other kind with `403 management_key_required` or `403 api_key_required`.

```ts
// orgId: MOUNTABLE_ORG, else the key's organisation
const admin = new Mountable({ apiKey: process.env.MOUNTABLE_MANAGEMENT_KEY });

const info = await admin.createFilesystem("agent-workspace", {
  spendingCapCents: 2_000,
  byteQuota: 5 * 2 ** 30,
});
await admin.updateFilesystem(info.id, { mutationsPerMinute: null }); // the platform default
console.log((await admin.filesystemUsage(info.id)).retainedBytes);
const key = await admin.createApiKey("backend", { filesystemId: info.id, mode: "rw" });
saveSecret(key.secret); // mtbl_…, returned only here
const s3 = await admin.createS3Key(info.id, "duckdb", { mode: "ro", expiresInDays: 90 });
saveSecret(s3.accessKeyId, s3.secretAccessKey); // returned only here

const client = new Mountable({ apiKey: key.secret! });
const hook = await client.createWebhook(info.id, "https://example.com/hooks/mountable", {
  pathPrefix: "/tickets",
});
saveSecret(hook.secret); // whsec_…, returned only here and by rotateWebhookSecret
```

| Method | Resolves to |
| --- | --- |
| `listFilesystems()` | `FilesystemInfo[]` |
| `createFilesystem(name, { spendingCapCents, mutationsPerMinute, byteQuota, entryQuota })` | `FilesystemInfo`; limits not given take the API's defaults |
| `getFilesystem(fsId)` | `FilesystemInfo` |
| `updateFilesystem(fsId, { spendingCapCents, mutationsPerMinute, byteQuota, entryQuota })` | `FilesystemInfo`; only the limits passed change, `null` removes the spending cap or the mutation rate |
| `filesystemUsage(fsId)` | `FilesystemUsage` |
| `listMountSessions(fsId)` | `MountSession[]` |
| `revokeMountSession(sessionId)` | `void` |
| `createWebhook(fsId, url, { pathPrefix })` | `Webhook` with `.secret` |
| `listWebhooks(fsId)` | `Webhook[]` (`.secret` `null`) |
| `deleteWebhook(fsId, webhookId)` | `void` |
| `rotateWebhookSecret(fsId, webhookId)` | `Webhook` with the new `.secret` |
| `enableWebhook(fsId, webhookId)` | `Webhook` |
| `deleteFilesystem(fsId)` | `void`; the filesystem, its files, sessions and S3 keys go at once |
| `createApiKey(name, { expiresInDays, filesystemId, mode })` | `ApiKey` with `.secret`; with `filesystemId`, limited to that filesystem |
| `listApiKeys()` | `ApiKey[]` (`.secret` `null`) |
| `revokeApiKey(keyId)` | `void` |
| `createS3Key(fsId, name, { mode, expiresInDays })` | `S3Key` with `.accessKeyId` and `.secretAccessKey` |
| `listS3Keys(fsId)` | `S3Key[]` (`.secretAccessKey` `null`) |
| `revokeS3Key(fsId, accessKeyId)` | `void` |
| `listGrants(fsId)` | `Grant[]` |
| `setGrant(fsId, principalType, principalId, mode)` | `Grant`; creates it or changes its mode |
| `deleteGrant(fsId, grantId)` | `void` |

An S3 key works with any S3 client at `https://s3.mountable.io`, path-style,
with the filesystem ID as the bucket. Key secrets are not shown when the
object is printed. Creating keys and deleting filesystems or grants are never
retried after a network failure; list them to see what happened.

These calls use the key, and the API checks what it may do:

- Creating, updating and deleting filesystems, keys and grants need a
  management key whose creator is still an owner or admin
  (`403 management_key_required` for an API key).
- Updating a filesystem and every webhook call, listing included, need
  read-write access to the filesystem.
- A key lists only its own live sessions and revokes only its own sessions.
- The API checks the ranges: `spendingCapCents` 0–1,000,000,000 or `null`
  (no cap), `mutationsPerMinute` 1–1,000 or `null` (the platform default),
  `byteQuota` 1 GiB–100 GiB, `entryQuota` 10,000–1,000,000. The two quotas
  cannot be removed.
- An empty, `.` or `..` filesystem, session, webhook, key or grant ID, or a `.` or `..`
  organisation ID, rejects with `invalid_request` before any request: URL
  normalisation would send it to another resource. An empty `orgId` counts
  as not given.

## Receiving webhooks

In the endpoint, verify the signature on the raw body before anything else:

```ts
import { MountableError, verifyWebhook } from "mountable-sdk";

export async function POST(request: Request): Promise<Response> {
  let message;
  try {
    message = await verifyWebhook(secret, request.headers, await request.text());
  } catch (e) {
    if (e instanceof MountableError && e.code === "invalid_webhook") return new Response(null, { status: 400 });
    throw e;
  }
  // A duplicate or a late message:
  if (BigInt(message.cursor) <= (await lastAppliedCursor())) return new Response(null, { status: 204 });
  if (message.type === "files.resync") {
    // list what you need again; events after message.cursor follow
  }
  for (const event of message.events) console.log(event.type, event.path, event.file?.etag);
  await saveAppliedCursor(message.cursor);
  return new Response(null, { status: 204 });
}
```

`verifyWebhook` takes `Headers` or Node's `req.headers`, and the body as a
string or bytes, and resolves to a `WebhookMessage` (`id`, the `webhook-id`
that a retry repeats; `type`; `timestamp`; `webhookId`; `filesystemId`;
`events` as `Change`; `cursor`). It accepts either signature a rotation sends
and refuses a timestamp more than five minutes from now. Answer within 10
seconds; a message can arrive more than once, so apply it only when its
`cursor`, compared as an integer, is greater than the last one applied.

## Behaviour

- The handle creates a one-hour token session on its first call and keeps
  the token in memory only. A `401`, or a session within 60 seconds of its
  expiry, makes it create a new one and send the call once more.
- Every failure rejects with `MountableError` carrying `status`, the API's
  stable `code`, `message`, `currentEtag` (for `precondition_failed`) and
  `retryAfter`. Network failures have `code === "network_error"`. API keys,
  tokens and webhook secrets never appear in errors or in `console.log` of
  the client.
- Only a `2xx` answer is a success; redirects are not followed.
- Only reads (`read`, `list`, `changes`, and the `get`, `list` and
  `filesystemUsage` calls) and the repeatable `revokeMountSession` and
  `enableWebhook` are retried, once, after a network failure. `write`,
  `delete`, `createFilesystem`, `updateFilesystem`, `createWebhook`,
  `rotateWebhookSecret` and `deleteWebhook` are never retried: after a
  timeout the outcome is unknown, and a conditional write makes your own retry
  safe.
- A request fails with `network_error` after 60 seconds without an answer;
  `changes` waits 60 seconds plus `wait`.
- `createMountTicket` returns the ticket once, in `.ticket`; printing,
  inspecting or `JSON.stringify` of the result leaves it out. Reuse your `idempotencyKey` to
  retry after an unknown outcome; a replay returns the session with `ticket`
  `null`. A `Webhook`'s `.secret` is left out the same way.

Documentation: <https://mountable.io/docs>. The behaviour shared with the
Python SDK is listed in
[`sdk/contract.md`](https://github.com/writeitai/mountable/blob/main/sdk/contract.md).

## License

[Apache-2.0](LICENSE).
