# Mountable SDK for TypeScript

Read, write, delete, list and follow the files of a
[Mountable](https://mountable.io) filesystem over HTTPS from a backend,
without mounting it, and create one-time mount tickets for sandboxes.

Not on npm yet. Until the first `sdk-vX.Y.Z` release, install from a
checkout of this repository:

```sh
(cd sdk/typescript && npm ci && npm run build)  # in the checkout
npm install /path/to/mountable/sdk/typescript   # in your project
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

## Behaviour

- The handle creates a one-hour token session on its first call and keeps
  the token in memory only. A `401`, or a session within 60 seconds of its
  expiry, makes it create a new one and send the call once more.
- Every failure rejects with `MountableError` carrying `status`, the API's
  stable `code`, `message`, `currentEtag` (for `precondition_failed`) and
  `retryAfter`. Network failures have `code === "network_error"`. API keys and
  tokens never appear in errors or in `console.log` of the client.
- Only a `2xx` answer is a success; redirects are not followed.
- Only reads (`read`, `list`, `changes`) are retried, once, after a network
  failure. `write` and `delete` are never retried: after a timeout the outcome
  is unknown, and a conditional write makes your own retry safe.
- A request fails with `network_error` after 60 seconds without an answer;
  `changes` waits 60 seconds plus `wait`.
- `createMountTicket` returns the ticket once, in `.ticket`; printing,
  inspecting or `JSON.stringify` of the result leaves it out. Reuse your `idempotencyKey` to
  retry after an unknown outcome; a replay returns the session with `ticket`
  `null`.

Documentation: <https://mountable.io/docs>. The behaviour shared with the
Python SDK is listed in
[`sdk/contract.md`](https://github.com/writeitai/mountable/blob/main/sdk/contract.md).

## License

[Apache-2.0](LICENSE).
