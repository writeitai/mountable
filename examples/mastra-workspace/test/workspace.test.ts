import assert from "node:assert/strict";
import { afterEach, beforeEach, test } from "node:test";
import { Mountable, MountableError } from "mountable-sdk";
import { API_KEY, FS, FILES, MockAPI, error, fileInfo, reply } from "../../../sdk/typescript/test/mock-api.ts";
import { buildWorkflows, cleanupNote, workspaceTools } from "../workspace.ts";

let api: MockAPI;
let client: Mountable;
let saved: Buffer | undefined;
let writes: number;
let reads: number;
let failWrite: "cap" | "drop" | undefined;
let readOverride: Buffer | undefined;
const path = "notes/brief.md";
const url = `${FILES}/${path}`;

beforeEach(async () => {
  api = new MockAPI();
  await api.start();
  client = new Mountable({ apiKey: API_KEY, baseUrl: api.url });
  saved = undefined;
  readOverride = undefined;
  failWrite = undefined;
  writes = reads = 0;
  api.routes.set(`PUT ${url}`, (req) => {
    writes++;
    if (failWrite === "cap") return error(402, "filesystem_spending_cap_reached");
    if (failWrite === "drop") return { status: 200, fault: "drop" };
    // A second read-only token is issued for the reader handle in this fixture.
    if (req.headers.authorization === "Bearer mtblst_token2secret") return error(403, "permission_denied");
    assert.equal(req.headers["if-none-match"], "*");
    if (saved) return error(412, "precondition_failed");
    saved = req.body;
    return reply(201, fileInfo(path, { size: saved.length }));
  });
  api.routes.set(`GET ${url}`, () => {
    reads++;
    const data = readOverride ?? saved;
    return data
      ? reply(200, data, { "Content-Type": "application/octet-stream", "Last-Modified": "Thu, 01 Oct 2026 09:30:00 GMT", ETag: '"e1"' })
      : error(404, "not_found");
  });
  api.routes.set(`DELETE ${url}`, (req) => {
    assert.equal(req.headers["if-match"], '"e1"');
    saved = undefined;
    return reply(204);
  });
});
afterEach(async () => { await api.close(); });
function flows() {
  return buildWorkflows(client.filesystem(FS), client.filesystem(FS, "ro"), path);
}
async function handoff(text: string) {
  return (await flows().handoff.createRun()).start({ inputData: { text } });
}
async function read() {
  return (await flows().read.createRun()).start({ inputData: {} });
}

test("real Mastra tools run in sequence and the reader retrieves storage bytes", async () => {
  readOverride = Buffer.from("read from storage");
  const result = await handoff("write this");
  assert.equal(result.status, "success");
  if (result.status !== "success") return;
  assert.deepEqual(result.result, { text: "read from storage" });
  assert.equal(saved?.toString(), "write this");
  assert.equal(writes, 1);
  assert.equal(reads, 1);
  const step = result.steps["create-note"];
  assert.equal(step.status, "success");
  if (step.status === "success") assert.deepEqual(step.output, { stored: true });
});

test("a newly built workflow/client reads the existing note", async () => {
  assert.equal((await handoff("still here")).status, "success");
  client = new Mountable({ apiKey: API_KEY, baseUrl: api.url });
  const result = await read();
  assert.equal(result.status, "success");
  if (result.status === "success") assert.equal(result.result.text, "still here");
  assert.equal(writes, 1);
});

test("create-only refusal preserves the earlier note and skips the reader", async () => {
  saved = Buffer.from("original");
  const result = await handoff("replacement");
  assert.equal(result.status, "failed");
  assert.equal(saved.toString(), "original");
  assert.equal(writes, 1);
  assert.equal(reads, 0);
});

test("rejects caller-controlled authority and oversized bytes before writing", async () => {
  for (const extra of [{ path: "notes/other.md" }, { filesystemId: "other" }, { userId: "other" }, { apiKey: "other" }]) {
    await assert.rejects(() => (async () => {
      const run = await flows().handoff.createRun();
      return run.start({ inputData: { text: "hello", ...extra } as never });
    })());
  }
  await assert.rejects(() => handoff("🦊".repeat(4097)));
  assert.equal(writes, 0);
});

test("rejects mismatched handles and escaped paths before requests", () => {
  const writer = client.filesystem(FS);
  const reader = client.filesystem(FS, "ro");
  assert.throws(() => workspaceTools(writer, client.filesystem("other", "ro"), path));
  assert.throws(() => workspaceTools(writer, writer, path));
  for (const bad of ["../x.md", "notes/../x.md", "/notes/x.md", "notes/x/y.md", "notes/a.txt"]) {
    assert.throws(() => workspaceTools(writer, reader, bad));
  }
  assert.equal(api.requests.length, 0);
});

test("402 and unknown writes are not retried and never reach the reader", async () => {
  for (const fail of ["cap", "drop"] as const) {
    failWrite = fail;
    writes = 0;
    const result = await handoff("hello");
    assert.equal(result.status, "failed");
    assert.equal(writes, 1);
    assert.equal(reads, 0);
  }
});

test("read-only SDK permission reaches the server and refuses a write", async () => {
  const writer = client.filesystem(FS);
  const reader = client.filesystem(FS, "ro");
  const result = await (await buildWorkflows(writer, reader, path).handoff.createRun()).start({ inputData: { text: "original" } });
  assert.equal(result.status, "success");
  await assert.rejects(reader.write(path, "no"), (e: unknown) => e instanceof MountableError && e.status === 403);
  assert.equal(saved?.toString(), "original");
});

test("rejects oversized and invalid UTF-8 reads", async () => {
  for (const data of [Buffer.alloc(16_385), Buffer.from([0xff])]) {
    readOverride = data;
    assert.equal((await read()).status, "failed");
  }
});

test("cleanup refuses changed content, deletes matching ETag, then read is missing", async () => {
  saved = Buffer.from("keep");
  const writer = client.filesystem(FS);
  await assert.rejects(cleanupNote(writer, path, "wrong"), /contents differ/);
  assert.equal(saved.toString(), "keep");
  await cleanupNote(writer, path, "keep");
  assert.equal(saved, undefined);
  assert.equal((await read()).status, "failed");
});
