# Mastra tools with a persistent mountable.io note

A real Mastra workflow creates a note, then reads it from mountable.io.
A separate process reads the same file. The first tool returns only
`{ stored: true }`, so the reader has to retrieve the file.

Maintained by mountable.io as a community example. This is not an official
Mastra integration, a Mastra storage adapter, or workflow snapshot storage.
It uses deterministic tools and workflows without a model call or hosted
Mastra service. mountable.io is a separate paid dependency; the client code
is Apache-2.0.

## Run it

Use Node 24+, an existing mountable.io account with a saved payment method,
an existing filesystem, and an API key with access to it. Set
`MOUNTABLE_API_KEY` in your private environment or secret manager, never in
command arguments or committed files. Normal [usage prices](https://mountable.io/docs/billing)
apply; this example creates no account, filesystem, credits, or provider bill.

```bash
git clone https://github.com/writeitai/mountable.git
cd mountable/examples/mastra-workspace
npm ci
export MOUNTABLE_FILESYSTEM_ID=your-filesystem-id
export MOUNTABLE_NOTE_PATH="notes/mastra-$(date +%s).md"
npm run handoff -- --text "Parser review complete."
npm run read
```

Both commands print `Parser review complete.`. They run in separate processes.
The dependencies are pinned: `@mastra/core` 1.79.0, `mountable-sdk` 0.3.0,
Zod 4.6.7. `npm run typecheck` and `npm test` run offline checks against a
loopback fake Files API using the real framework and released SDK.

## Trusted scope and permissions

Your application authenticates the user, looks up that user's filesystem,
and chooses the note path **before** constructing the tools. The environment
variables above stand in for that trusted lookup in this CLI example.
Never pass tool arguments, model output, or an unauthenticated request body
as those settings. The tool accepts only `{ text }`; the reader accepts `{}`.
Unknown fields such as `filesystemId`, `userId`, `apiKey`, or `path` are refused.

`workspaceTools(writer, reader, path)` requires handles for the same filesystem
and a read-only reader. Paths are one Markdown file directly under `notes/`;
text is nonempty UTF-8 up to 16 KiB. Both reads and writes use the Files API.
For a reader-only application, expose only `readNote` and keep the reusable
API key in trusted backend code. The SDK uses a read-only token session for
its reader handle.

Mastra creates workflow steps from the actual `createTool` definitions with
`createStep(tool)`. The handoff workflow runs create, maps the acknowledgement
to `{}`, then runs read. The read workflow starts from `{}` and only reads.
The storage and path are closed over in trusted code, not selected by workflow
input. The workflow does not persist or resume Mastra checkpoints.

## Errors and cleanup

The write is create-only (`If-None-Match: *`). Running the handoff again with
the same path fails with `precondition_failed` and keeps the first note.
Workflow/step retry counts are zero. After a network failure, the write outcome
may be unknown: run `npm run read` before deciding what to do. Do not blindly
rerun the writer with another path, which could duplicate billable work.

A read-only write is refused by the service. A `402` with
`filesystem_spending_cap_reached` stops billable work on that filesystem;
other filesystems continue. An invalid key, missing file, quota or rate limit
also stops this workflow. Model spending, if you add a model in your app,
has a separate bill and is not covered by the filesystem cap.

Remove this example's note with the exact original body:

```bash
npm run cleanup -- --text "Parser review complete."
```

Cleanup reads the file, compares its bytes and deletes with the matching ETag.
It refuses a different body or concurrent change. It never recursively removes
`notes/` or deletes a filesystem. SDK sessions expire normally; delete or revoke
your key/session in your own account when appropriate. Retained storage can
remain billable after logical deletion until reclaimed; see [billing](https://mountable.io/docs/billing).

[Full guide](https://mountable.io/docs/mastra),
[Mastra tools](https://mastra.ai/docs/agents/tools),
[Mastra workflows](https://mastra.ai/docs/workflows/overview).
