# mountable.io SDK for TypeScript

Read, write, delete, list and follow the files of a
[mountable.io](https://mountable.io) filesystem over HTTPS from a backend,
without mounting it, create one-time mount tickets for sandboxes, and manage
and verify the webhooks that call your backend when files change.

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

## Webhooks

A webhook calls your HTTPS endpoint with a filesystem's changes. Creating one
needs an API key with read-write access to the filesystem.

```ts
const hook = await client.createWebhook("fs_…", "https://example.com/hooks/mountable", { pathPrefix: "/tickets" });
await storeSecret(hook.id, hook.secret); // whsec_…, shown only now; not in console.log(hook)

await client.listWebhooks("fs_…"); // Webhook[]: .state, .disabledReason, .lastFailure
await client.rotateWebhookSecret("fs_…", hook.id); // .secret is the new one
await client.enableWebhook("fs_…", hook.id); // after it was disabled as "failing"
await client.deleteWebhook("fs_…", hook.id);
```

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
  if (BigInt(message.cursor) <= (await lastAppliedCursor())) return new Response(null, { status: 204 }); // a duplicate or a late message
  if (message.type === "files.resync") {
    // list what you need again; events after message.cursor follow
  }
  for (const event of message.events) console.log(event.type, event.path, event.file?.etag); // invalidations, as from fs.changes()
  await saveAppliedCursor(message.cursor);
  return new Response(null, { status: 204 });
}
```

`verifyWebhook` takes `Headers` or Node's `req.headers`, and the body as a
string or bytes. It accepts any of the signatures a rotation sends and
refuses a timestamp more than five minutes from now. Answer within 10
seconds; a message can arrive more than once, so apply it only when its
`cursor`, compared as an integer, is greater than the last one applied.

## Behaviour

- The handle creates a one-hour token session on its first call and keeps
  the token in memory only. A `401`, or a session within 60 seconds of its
  expiry, makes it create a new one and send the call once more.
- Every failure rejects with `MountableError` carrying `status`, the API's
  stable `code`, `message`, `currentEtag` (for `precondition_failed`) and
  `retryAfter`. Network failures have `code === "network_error"`. API keys and
  tokens never appear in errors or in `console.log` of the client.
- Only a `2xx` answer is a success; redirects are not followed.
- Only reads (`read`, `list`, `changes`, `listWebhooks`) are retried, once,
  after a network failure. `write` and `delete` are never retried: after a
  timeout the outcome is unknown, and a conditional write makes your own retry
  safe. Nor are the webhook calls that change something: after an unknown
  outcome of `createWebhook`, look for the URL in `listWebhooks` first.
- The webhook calls look up the API key's organisation once per client
  (`GET /api/v1/me`).
- A request fails with `network_error` after 60 seconds without an answer;
  `changes` waits 60 seconds plus `wait`.
- `createMountTicket` returns the ticket once, in `.ticket`; printing,
  inspecting or `JSON.stringify` of the result leaves it out. Reuse your `idempotencyKey` to
  retry after an unknown outcome; a replay returns the session with `ticket`
  `null`.
- `createWebhook` and `rotateWebhookSecret` return the signing secret once,
  in `.secret`; printing, inspecting or `JSON.stringify` of the result leaves
  it out, and webhook secrets never appear in errors.

Documentation: <https://mountable.io/docs>. The behaviour shared with the
Python SDK is listed in
[`sdk/contract.md`](https://github.com/writeitai/mountable/blob/main/sdk/contract.md).

## License

[Apache-2.0](LICENSE).
