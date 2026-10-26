# AI SDK file tools on mountable.io

A minimal Next.js app gives the AI SDK two tools: create a note once and read
it back. Both use the TypeScript `mountable-sdk`. Restart Next.js and read
again: the note lives in the filesystem, independently of this app process.

The web buttons call the tools directly. `check:live` runs `generateText`
with the AI SDK's scripted test provider, which calls the same tools. Neither
makes a paid model call. You can pass these tool definitions to your own
funded model in `generateText` too.

Maintained by mountable.io, independently of Vercel. The example and clients
are open source. The hosted mountable.io service is a separate paid dependency.

## Run locally

Use Node 24+ and an existing mountable.io account with a saved payment method,
an existing filesystem and an API key with access to it. The app creates no
account, filesystem or credits. Normal [usage prices](https://mountable.io/docs/billing?utm_source=trending-ecosystem-integrations&utm_medium=other&utm_content=vercel-ai-workspace)
apply. Model-provider charges, if you add a model, are separate and are not
limited by a filesystem's spending cap.

```sh
git clone https://github.com/writeitai/mountable.git
cd mountable/examples/vercel-ai-workspace
npm ci
cp .env.example .env.local
openssl rand -hex 32
```

Set `MOUNTABLE_API_KEY`, `MOUNTABLE_FILESYSTEM_ID`, `MOUNTABLE_NOTE_PATH`
and `DEMO_ACCESS_TOKEN` in `.env.local`. Use the generated random string for
the operator token. Use a new path like `notes/brief.md`; the app refuses
existing files. Keep `.env.local` private. The API key stays on the server.

```sh
npm run dev
```

Open http://127.0.0.1:3000. Enter the operator token (never the API key),
create the note, and read it. Stop and restart the app, then read it again.
The token is held in browser memory and is required for every storage call.
The local app binds only loopback. Do not publish an anonymous paid-storage
endpoint. For an application with several users, replace the single operator
check with your authentication and a server-side user-to-filesystem mapping.

## Deploy your own copy

[![Deploy with Vercel](https://vercel.com/button)](https://vercel.com/new/clone?repository-url=https%3A%2F%2Fgithub.com%2Fwriteitai%2Fmountable%2Ftree%2Fmain%2Fexamples%2Fvercel-ai-workspace&env=MOUNTABLE_API_KEY,MOUNTABLE_FILESYSTEM_ID,MOUNTABLE_NOTE_PATH,DEMO_ACCESS_TOKEN&envLink=https%3A%2F%2Fmountable.io%2Fdocs%2Fvercel-ai-sdk)

Use your own Vercel account and check its current plan terms and charges.
This button requests environment variable names only; no secret values are
included in the link. Keep the operator token private, and use a filesystem
with a backend-selected cap for your own operator. Do not give public visitors
a shared paid-storage credential.

## Tools for your model

`lib/tools.ts` creates AI SDK `tool` definitions with Zod input schemas.
`create_note` accepts only note text and returns `stored`. `read_note` accepts
no arguments and reads through a read-only filesystem handle. The backend
chooses the filesystem and path; neither is a model argument or request field.

```ts
const result = await generateText({
  model: yourConfiguredModel,
  tools: configuredTools(),
  prompt: "Read the stored note using read_note.",
  maxRetries: 0,
});
```

Configure your own model provider separately. Keep the model's tools on your
trusted server. The filesystem read-only mode narrows the storage session;
the backend API key itself may still have broader rights.

The note must be UTF-8 text, 1 byte to 16 KiB, at `notes/<name>.md`.
Writes use `If-None-Match: *` and never retry. After an uncertain write, read
before choosing a new path. A `402` stops that user's billable storage work;
a `412` leaves the existing file untouched. The example does not delete notes.

## Checks

```sh
npm run typecheck
npm test
npm run build
```

Offline checks exercise the tool boundaries, byte limit, create-only writes,
operator authentication and a billing refusal without paid calls.

To check the scripted AI SDK pipeline against your own filesystem, export
the same mountable.io environment variables in your shell. These commands do
not read `.env.local`. Set `NOTE_TEXT` to the expected note when reading a
custom message. Use a fresh path for the create operation.

```sh
npm run check:live -- create
npm run check:live -- read
```

The commands are separate processes. `generateText` uses the AI SDK's test
provider to request one tool call and confirms the tool result. The create
result is `stored`; the read result is the file text.

Sources: [AI SDK tools](https://ai-sdk.dev/docs/ai-sdk-core/tools-and-tool-calling),
[AI SDK testing](https://ai-sdk.dev/docs/ai-sdk-core/testing),
[guide](https://mountable.io/docs/vercel-ai-sdk?utm_source=trending-ecosystem-integrations&utm_medium=other&utm_content=vercel-ai-workspace).
