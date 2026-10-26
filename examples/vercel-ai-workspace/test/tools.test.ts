import assert from "node:assert/strict";
import { test } from "node:test";
import { MountableError, type Filesystem } from "mountable-sdk";
import { workspaceTools } from "../lib/tools.ts";
import { handleNote } from "../lib/request.ts";

const options = { toolCallId: "test", messages: [], context: {} };
function fixture() {
  const files = new Map<string, string>();
  let writes = 0;
  const writer = {
    write: async (path: string, text: string, opts: { ifNoneMatch: boolean }) => {
      writes++;
      assert.equal(opts.ifNoneMatch, true);
      if (files.has(path)) throw new MountableError(412, "precondition_failed", "exists");
      files.set(path, text);
      return {};
    },
  } as unknown as Filesystem;
  const reader = { read: async (path: string) => ({ data: Buffer.from(files.get(path)!) }) } as unknown as Filesystem;
  return { files, writer, reader, writes: () => writes, tools: () => workspaceTools(writer, reader, "notes/brief.md") };
}

test("a fresh read-only tool reads stored bytes and create-only never replaces", async () => {
  const f = fixture();
  assert.equal(await f.tools().create_note.execute!({ text: "persisted" }, options), "stored");
  assert.equal(await f.tools().read_note.execute!({}, options), "persisted");
  await assert.rejects(f.tools().create_note.execute!({ text: "replacement" }, options) as Promise<unknown>, { status: 412 });
  assert.equal(f.files.get("notes/brief.md"), "persisted");
});

test("paths and UTF-8 byte limit are checked before writes", async () => {
  const f = fixture();
  for (const path of ["../other.md", "notes/../other.md", "/notes/brief.md", "notes/a/b.md"]) {
    assert.throws(() => workspaceTools(f.writer, f.reader, path));
  }
  await assert.rejects(f.tools().create_note.execute!({ text: "🦊".repeat(4097) }, options) as Promise<unknown>);
  assert.equal(f.writes(), 0);
});

process.env.DEMO_ACCESS_TOKEN = "a".repeat(32);
function request(body: unknown, auth = "a".repeat(32)) {
  return new Request("http://localhost/api/note", {
    method: "POST", headers: { Authorization: `Bearer ${auth}` }, body: JSON.stringify(body),
  });
}
test("operator route rejects missing auth, model-chosen paths and large requests before opening storage", async () => {
  const noStorage = () => { throw new Error("must not open storage"); };
  assert.equal((await handleNote(request({ action: "read" }, "wrong"), noStorage)).status, 401);
  assert.equal((await handleNote(request({ action: "read", filesystem: "other" }), noStorage)).status, 400);
  assert.equal((await handleNote(request({ action: "create", text: "x".repeat(33000) }), noStorage)).status, 413);
});

test("billing refusal stops once, is sanitized, and another workspace remains usable", async () => {
  let attempts = 0;
  const blocked = {
    write: async () => { attempts++; throw new MountableError(402, "filesystem_spending_cap_reached", "private provider detail"); },
  } as unknown as Filesystem;
  const f = fixture();
  const result = await handleNote(request({ action: "create", text: "note" }), () => workspaceTools(blocked, f.reader, "notes/brief.md"));
  assert.equal(result.status, 402);
  assert.equal(attempts, 1);
  assert.equal((await result.text()).includes("private provider"), false);
  const other = await handleNote(request({ action: "create", text: "other" }), f.tools);
  assert.equal(other.status, 200);
  assert.equal(f.files.get("notes/brief.md"), "other");
});
