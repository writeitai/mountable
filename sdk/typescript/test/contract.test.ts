// The cases of sdk/contract.md, one test per case ID.

import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { inspect } from "node:util";
import { afterEach, beforeEach, test } from "node:test";

import { Mountable, MountableError } from "../src/index.ts";
import {
  API_KEY,
  CHANGES,
  error,
  FILES,
  FS,
  fileInfo,
  LIST,
  MockAPI,
  type Reply,
  type Request,
  reply,
  SESSIONS,
} from "./mock-api.ts";

const MODIFIED = new Date("2026-10-17T12:00:00.123Z");
const READ_REPLY = reply(200, "abc", { ETag: '"e1"', "Last-Modified": "Sat, 17 Oct 2026 12:00:00 GMT" });

let api: MockAPI;
let client: Mountable;

beforeEach(async () => {
  api = new MockAPI();
  await api.start();
  client = new Mountable({ apiKey: API_KEY, baseUrl: api.url });
});

afterEach(async () => {
  await api.close();
});

async function rejects(promise: Promise<unknown> | (() => unknown)): Promise<MountableError> {
  try {
    await (typeof promise === "function" ? promise() : promise);
  } catch (e) {
    assert.ok(e instanceof MountableError, `not a MountableError: ${e}`);
    return e;
  }
  assert.fail("did not throw");
}

const text = (data: Uint8Array) => new TextDecoder().decode(data);

// --- operations ------------------------------------------------------------

test("C01 write creates and returns FileInfo", async () => {
  api.route("PUT", `${FILES}/tickets/AB-42.md`, reply(201, fileInfo("/tickets/AB-42.md")));
  const fs = client.filesystem(FS);
  const info = await fs.write("tickets/AB-42.md", new TextEncoder().encode("abc"));
  assert.deepEqual(info, { path: "/tickets/AB-42.md", type: "file", size: 3, etag: '"e1"', modifiedAt: MODIFIED });
  await fs.write("tickets/AB-42.md", "žluť");
  const [first, second] = api.operations();
  assert.equal(first.method, "PUT");
  assert.equal(first.body.toString(), "abc");
  assert.equal(first.headers["content-type"], "application/octet-stream");
  assert.equal(first.headers["content-length"], "3");
  assert.deepEqual(second.body, Buffer.from("žluť"));
});

test("C02 write replaces and returns FileInfo", async () => {
  api.route("PUT", `${FILES}/a.txt`, reply(200, fileInfo("/a.txt", { etag: '"e2"', size: 5 })));
  const info = await client.filesystem(FS).write("a.txt", "hello");
  assert.equal(info.etag, '"e2"');
  assert.equal(info.size, 5);
});

test("C03 read returns data and info from headers", async () => {
  api.route("GET", `${FILES}/tickets/AB-42.md`, READ_REPLY);
  const file = await client.filesystem(FS).read("tickets/AB-42.md");
  assert.equal(text(file.data), "abc");
  assert.deepEqual(file.info, {
    path: "/tickets/AB-42.md",
    type: "file",
    size: 3,
    etag: '"e1"',
    modifiedAt: new Date("2026-10-17T12:00:00Z"),
  });
  assert.equal(api.operations()[0].method, "GET");
});

test("C04 delete returns nothing", async () => {
  api.route("DELETE", `${FILES}/old.md`, reply(204));
  assert.equal(await client.filesystem(FS).delete("old.md"), undefined);
  assert.equal(api.calls().at(-1), `DELETE ${FILES}/old.md`);
  assert.equal(api.operations()[0].headers["if-match"], undefined);
});

test("C05 list pages through a directory", async () => {
  const page = {
    entries: [fileInfo("/tickets/a.md"), fileInfo("/tickets/sub", { etag: undefined, type: "directory", size: 0 }) /* the API omits etag for anything but a file */],
    next: "sub",
  };
  api.route("GET", `${LIST}/tickets`, reply(200, page));
  api.route("GET", `${LIST}/`, reply(200, { entries: [], next: null }));
  const fs = client.filesystem(FS);
  const result = await fs.list("tickets", { after: "a", limit: 2 });
  assert.equal(result.next, "sub");
  assert.deepEqual(result.entries[0], { path: "/tickets/a.md", type: "file", size: 3, etag: '"e1"', modifiedAt: MODIFIED });
  assert.equal(result.entries[1].type, "directory");
  assert.equal(result.entries[1].etag, null);
  const root = await fs.list();
  assert.deepEqual(root, { entries: [], next: null });
  const [paged, last] = api.operations();
  assert.deepEqual(paged.query, { after: "a", limit: "2" });
  assert.equal(last.path, `${LIST}/`);
  assert.deepEqual(last.query, {});
});

test("C06 changes without cursor returns a cursor", async () => {
  api.route("GET", CHANGES, reply(200, { events: [], cursor: "c100" }));
  const batch = await client.filesystem(FS).changes();
  assert.deepEqual(batch, { events: [], cursor: "c100" });
  assert.deepEqual(api.operations()[0].query, {});
});

test("C07 changes with cursor returns events", async () => {
  const events = [
    { type: "created", path: "/a.md", file: fileInfo("/a.md") },
    { type: "deleted", path: "/b.md", file: null },
  ];
  api.route("GET", CHANGES, reply(200, { events, cursor: "c102" }));
  const fs = client.filesystem(FS);
  const batch = await fs.changes({ cursor: "c100", wait: 5 });
  assert.equal(batch.cursor, "c102");
  assert.equal(batch.events[0].type, "created");
  assert.equal(batch.events[0].path, "/a.md");
  assert.deepEqual(batch.events[0].file, { path: "/a.md", type: "file", size: 3, etag: '"e1"', modifiedAt: MODIFIED });
  assert.equal(batch.events[1].type, "deleted");
  assert.equal(batch.events[1].file, null);
  await fs.changes({ cursor: "c102" });
  const [first, second] = api.operations();
  assert.deepEqual(first.query, { cursor: "c100", wait: "5" });
  assert.deepEqual(second.query, { cursor: "c102", wait: "25" });
});

test("C08 paths are encoded per segment", async () => {
  const encoded = `${LIST}/a%20b/c%25d%23e%3Ff/%C5%BElu%C5%A5/%21%27%28%29%2A`;
  api.route("GET", encoded, reply(200, { entries: [], next: null }));
  const fs = client.filesystem(FS);
  await fs.list("/a b/c%d#e?f/žluť/!'()*/");
  assert.equal(api.operations()[0].path, encoded);
  for (const bad of ["a/../b", "./a", ".."]) {
    const e = await rejects(() => fs.read(bad));
    assert.equal(e.code, "bad_path");
    assert.equal(e.status, null);
  }
  assert.equal(api.operations().length, 1);
});

test("C09 createMountTicket", async () => {
  const ticket = await client.createMountTicket(FS, {
    mode: "ro",
    maxDurationSeconds: 30 * 86400,
    idempotencyKey: "job-42",
  });
  assert.equal(ticket.id, "s1");
  assert.equal(ticket.ticket, "mtbltk_ticket1secret");
  assert.equal(ticket.mode, "ro");
  assert.equal(ticket.state, "requested");
  assert.equal(ticket.filesystemId, FS);
  assert.ok(ticket.expiresAt instanceof Date && ticket.ticketExpiresAt instanceof Date);
  const request = api.sessions()[0];
  assert.equal(request.headers.authorization, `Bearer ${API_KEY}`);
  assert.deepEqual(request.json(), {
    filesystem_id: FS,
    mode: "ro",
    idempotency_key: "job-42",
    max_duration_seconds: 2592000,
  });
  await client.createMountTicket(FS);
  const body = api.sessions()[1].json();
  assert.equal(body.mode, "rw");
  assert.ok(body.idempotency_key.length >= 16);
  assert.equal("max_duration_seconds" in body || "credential" in body, false);

  api.route("POST", SESSIONS, (request) => {
    const answer = api.issueSession(request);
    Object.assign(answer.body as object, { ticket: null, state: "active" });
    return answer;
  });
  assert.equal((await client.createMountTicket(FS, { idempotencyKey: "job-42" })).ticket, null);
});

// --- conditional writes ----------------------------------------------------

test("C10 write ifMatch sends the quoted ETag", async () => {
  api.route("PUT", `${FILES}/a.md`, reply(200, fileInfo("/a.md")));
  const fs = client.filesystem(FS);
  await fs.write("a.md", "x", { ifMatch: '"e1"' });
  await fs.write("a.md", "x", { ifMatch: "e1" });
  assert.deepEqual(
    api.operations().map((r) => r.headers["if-match"]),
    ['"e1"', '"e1"'],
  );
});

test("C11 write ifNoneMatch sends a star", async () => {
  api.route("PUT", `${FILES}/a.md`, reply(201, fileInfo("/a.md")));
  await client.filesystem(FS).write("a.md", "x", { ifNoneMatch: true });
  const request = api.operations()[0];
  assert.equal(request.headers["if-none-match"], "*");
  assert.equal(request.headers["if-match"], undefined);
});

test("C12 delete ifMatch sends the ETag", async () => {
  api.route("DELETE", `${FILES}/a.md`, reply(204));
  await client.filesystem(FS).delete("a.md", { ifMatch: '"e1"' });
  assert.equal(api.operations()[0].headers["if-match"], '"e1"');
});

test("C13 precondition_failed carries the current ETag", async () => {
  api.route("PUT", `${FILES}/a.md`, error(412, "precondition_failed", { current_etag: '"e9"' }));
  api.route("PUT", `${FILES}/new.md`, error(412, "precondition_failed", { current_etag: null }));
  const fs = client.filesystem(FS);
  let e = await rejects(fs.write("a.md", "x", { ifMatch: '"e1"' }));
  assert.deepEqual([e.status, e.code, e.currentEtag], [412, "precondition_failed", '"e9"']);
  e = await rejects(fs.write("new.md", "x", { ifMatch: '"e1"' }));
  assert.equal(e.code, "precondition_failed");
  assert.equal(e.currentEtag, null);
});

// --- token sessions --------------------------------------------------------

test("C14 first operation creates a token session", async () => {
  api.route("GET", `${LIST}/`, reply(200, { entries: [], next: null }));
  const fs = client.filesystem(FS, "ro");
  assert.equal(api.requests.length, 0);
  await fs.list();
  const [session, operation] = api.requests;
  assert.equal(session.method, "POST");
  assert.equal(session.path, SESSIONS);
  assert.equal(session.headers.authorization, `Bearer ${API_KEY}`);
  const { idempotency_key, ...body } = session.json();
  assert.ok(idempotency_key.length >= 16);
  assert.deepEqual(body, { filesystem_id: FS, mode: "ro", max_duration_seconds: 3600, credential: "token" });
  assert.equal(operation.headers.authorization, "Bearer mtblst_token1secret");
});

test("C15 the session is reused", async () => {
  api.route("GET", `${LIST}/`, reply(200, { entries: [], next: null }));
  const fs = client.filesystem(FS);
  await Promise.all([fs.list(), fs.list()]); // concurrent first calls share one session
  await fs.list();
  await fs.list();
  assert.equal(api.sessions().length, 1);
  assert.deepEqual(new Set(api.operations().map((r) => r.headers.authorization)), new Set(["Bearer mtblst_token1secret"]));
});

test("C16 a 401 refreshes the session and retries once", async () => {
  const refuse =
    (token: string, answer: Reply) =>
    (request: Request): Reply =>
      request.headers.authorization === `Bearer ${token}` ? error(401, "unauthenticated") : answer;
  api.route("GET", `${FILES}/a.md`, refuse("mtblst_token1secret", READ_REPLY));
  api.route("PUT", `${FILES}/a.md`, refuse("mtblst_token2secret", reply(200, fileInfo("/a.md"))));
  const fs = client.filesystem(FS);
  assert.equal(text((await fs.read("a.md")).data), "abc");
  assert.equal((await fs.write("a.md", "payload")).path, "/a.md");
  assert.equal(api.sessions().length, 3);
  assert.deepEqual(
    api.operations().map((r) => [r.method, r.headers.authorization.slice(-12), r.body.toString()]),
    [
      ["GET", "token1secret", ""],
      ["GET", "token2secret", ""],
      ["PUT", "token2secret", "payload"],
      ["PUT", "token3secret", "payload"],
    ],
  );
});

test("C17 one refresh per call", async () => {
  api.route("GET", `${FILES}/a.md`, error(401, "unauthenticated"));
  let e = await rejects(client.filesystem(FS).read("a.md"));
  assert.deepEqual([e.status, e.code], [401, "unauthenticated"]);
  assert.equal(api.sessions().length, 2);
  assert.equal(api.operations().length, 2);

  // A network retry does not grant another refresh: 401, drop, 401 fails.
  const answers: Reply[] = [error(401, "unauthenticated"), { status: 0, fault: "drop" }, error(401, "unauthenticated"), READ_REPLY];
  api.route("GET", `${FILES}/b.md`, () => answers.shift() as Reply);
  e = await rejects(client.filesystem(FS).read("b.md"));
  assert.deepEqual([e.status, e.code], [401, "unauthenticated"]);
  assert.equal(api.sessions().length, 4);
  assert.equal(api.operations().filter((r) => r.path === `${FILES}/b.md`).length, 3);
});

test("C18 a session near expiry is replaced", async () => {
  api.route("GET", `${LIST}/`, reply(200, { entries: [], next: null }));
  api.sessionLifetimeMs = 59_000;
  const fs = client.filesystem(FS);
  await fs.list();
  await fs.list();
  assert.equal(api.sessions().length, 2);
  assert.deepEqual(
    api.operations().map((r) => r.headers.authorization),
    ["Bearer mtblst_token1secret", "Bearer mtblst_token2secret"],
  );
  api.sessionLifetimeMs = 120_000;
  await fs.list();
  await fs.list();
  assert.equal(api.sessions().length, 3);
});

test("C19 a failed session creation raises", async () => {
  api.route("POST", SESSIONS, error(403, "no_grant"));
  const e = await rejects(client.filesystem(FS).write("a.md", "x"));
  assert.deepEqual([e.status, e.code], [403, "no_grant"]);
  assert.deepEqual(api.operations(), []);
});

// --- errors ----------------------------------------------------------------

const ERROR_CODES: [number, string][] = [
  [400, "bad_path"],
  [401, "unauthenticated"],
  [403, "read_only"],
  [403, "token_needs_key_or_cli"],
  [404, "not_found"],
  [409, "is_a_directory"],
  [409, "not_a_regular_file"],
  [409, "not_a_directory"],
  [409, "directory_not_empty"],
  [409, "conflict"],
  [409, "quota_exceeded"],
  [410, "cursor_expired"],
  [412, "precondition_failed"],
  [413, "too_large"],
  [429, "rate_limited"],
  [429, "download_quota"],
  [501, "unsupported_file"],
  [503, "changes_unavailable"],
];

test("C20 every error code raises MountableError", async () => {
  const fs = client.filesystem(FS);
  for (const [status, code] of ERROR_CODES) {
    api.route("PUT", `${FILES}/nested.md`, error(status, code, { message: "it failed" }));
    api.route("PUT", `${FILES}/flat.md`, reply(status, { code }));
    let e = await rejects(fs.write("nested.md", "x"));
    assert.deepEqual([e.status, e.code, e.message], [status, code, "it failed"]);
    e = await rejects(fs.write("flat.md", "x"));
    assert.deepEqual([e.status, e.code], [status, code]);
    assert.ok(e.message);
  }
});

test("C21 Retry-After is read", async () => {
  api.route("PUT", `${FILES}/a.md`, reply(429, { detail: { code: "rate_limited" } }, { "Retry-After": "7" }));
  const e = await rejects(client.filesystem(FS).write("a.md", "x"));
  assert.deepEqual([e.code, e.retryAfter], ["rate_limited", 7]);
});

test("C22 network failure is network_error", async () => {
  api.route("PUT", `${FILES}/a.md`, { status: 0, fault: "drop" });
  let e = await rejects(client.filesystem(FS).write("a.md", "x"));
  assert.deepEqual([e.status, e.code], [null, "network_error"]);
  const closed = new Mountable({ apiKey: API_KEY, baseUrl: "http://127.0.0.1:9" });
  e = await rejects(closed.filesystem(FS).list());
  assert.deepEqual([e.status, e.code], [null, "network_error"]);
});

test("C23 secrets never appear in errors", async () => {
  const echo = `refused ${API_KEY} and mtblst_token1secret`;
  api.route("GET", `${FILES}/a.md`, error(403, "read_only", { message: echo }));
  api.route("POST", SESSIONS, (r) =>
    r.json().credential ? api.issueSession(r) : error(401, "unauthenticated", { message: echo }),
  );
  const fs = client.filesystem(FS);
  const errors = [await rejects(fs.read("a.md")), await rejects(client.createMountTicket(FS))];
  for (const e of errors) {
    for (const shown of [String(e), inspect(e), e.message, e.stack ?? "", JSON.stringify(e), inspect(fs), inspect(client)]) {
      assert.ok(!shown.includes(API_KEY) && !shown.includes("token1secret"), shown);
    }
  }
  api.route("POST", SESSIONS, (r) => api.issueSession(r));
  const ticket = await client.createMountTicket(FS);
  assert.ok(ticket.ticket?.startsWith("mtbltk_"));
  for (const shown of [String(ticket), inspect(ticket), JSON.stringify(ticket), JSON.stringify({ ...ticket })]) {
    assert.ok(!shown.includes("mtbltk_"), shown);
  }
});

test("C24 errors without a code", async () => {
  api.route("PUT", `${FILES}/a.md`, reply(502, "<html>bad gateway</html>"));
  const invalid = { detail: [{ loc: ["body", "max_duration_seconds"], msg: "too large" }] };
  api.route("POST", SESSIONS, (r) => (r.json().credential ? api.issueSession(r) : reply(422, invalid)));
  let e = await rejects(client.filesystem(FS).write("a.md", "x"));
  assert.deepEqual([e.status, e.code], [502, "api_error"]);
  assert.ok(e.message);
  e = await rejects(client.createMountTicket(FS, { maxDurationSeconds: 1e9 }));
  assert.deepEqual([e.status, e.code], [422, "invalid_request"]);
  assert.ok(e.message.includes("max_duration_seconds"));
});

// --- retries ---------------------------------------------------------------

function failFirst(n: number, fault: "drop" | "short", answer: Reply) {
  let calls = 0;
  return (): Reply => (++calls <= n ? { status: 200, body: "x".repeat(10), fault } : answer);
}

test("C25 reads are retried once after a network failure", async () => {
  const fs = client.filesystem(FS);
  api.route("GET", `${FILES}/a.md`, failFirst(1, "drop", READ_REPLY));
  assert.equal(text((await fs.read("a.md")).data), "abc");
  api.route("GET", `${FILES}/short.md`, failFirst(1, "short", READ_REPLY));
  assert.equal(text((await fs.read("short.md")).data), "abc");
  // The fault is a body cut short after the headers arrived.
  api.route("GET", "/short", { status: 200, body: "x".repeat(10), fault: "short" });
  const raw = await fetch(`${api.url}/short`);
  assert.equal(raw.status, 200);
  await assert.rejects(raw.arrayBuffer());
  api.route("GET", `${LIST}/`, failFirst(1, "drop", reply(200, { entries: [], next: null })));
  assert.deepEqual((await fs.list()).entries, []);
  api.route("GET", CHANGES, failFirst(1, "drop", reply(200, { events: [], cursor: "c1" })));
  assert.equal((await fs.changes({ cursor: "c0", wait: 0 })).cursor, "c1");
  for (const path of [`${FILES}/a.md`, `${FILES}/short.md`, `${LIST}/`, CHANGES]) {
    assert.equal(api.operations().filter((r) => r.path === path).length, 2, path);
  }
  api.route("GET", `${FILES}/b.md`, failFirst(2, "drop", READ_REPLY));
  const e = await rejects(fs.read("b.md"));
  assert.equal(e.code, "network_error");
  assert.equal(api.operations().filter((r) => r.path === `${FILES}/b.md`).length, 2);
});

test("C26 writes and deletes are never retried", async () => {
  const fs = client.filesystem(FS);
  api.route("PUT", `${FILES}/a.md`, failFirst(1, "drop", reply(200, fileInfo("/a.md"))));
  api.route("DELETE", `${FILES}/a.md`, failFirst(1, "drop", reply(204)));
  assert.equal((await rejects(fs.write("a.md", "x"))).code, "network_error");
  assert.equal((await rejects(fs.delete("a.md"))).code, "network_error");
  assert.deepEqual(
    api.operations().map((r) => r.method),
    ["PUT", "DELETE"],
  );
});

test("C27 HTTP errors are not retried", async () => {
  api.route("GET", `${FILES}/a.md`, reply(500, { detail: { code: "internal" } }));
  api.route("GET", `${LIST}/`, error(503, "retry"));
  const fs = client.filesystem(FS);
  assert.equal((await rejects(fs.read("a.md"))).status, 500);
  assert.equal((await rejects(fs.list())).status, 503);
  assert.equal(api.operations().length, 2);
});

// --- change feed -----------------------------------------------------------

test("C28 the cursor is passed on", async () => {
  const pages = [
    { events: [], cursor: "c1" },
    { events: [{ type: "updated", path: "/a", file: fileInfo("/a") }], cursor: "c2" },
    { events: [], cursor: "c2" },
  ];
  api.route("GET", CHANGES, () => reply(200, pages.shift()));
  const fs = client.filesystem(FS);
  let cursor = (await fs.changes()).cursor;
  cursor = (await fs.changes({ cursor, wait: 0 })).cursor;
  assert.equal((await fs.changes({ cursor, wait: 0 })).cursor, "c2");
  assert.deepEqual(
    api.operations().map((r) => r.query.cursor),
    [undefined, "c1", "c2"],
  );
});

test("C29 an expired cursor is recovered by relisting", async () => {
  api.route("GET", CHANGES, (request) => {
    if (request.query.cursor === "old") {
      return error(410, "cursor_expired");
    }
    return reply(200, { events: [], cursor: request.query.cursor ?? "fresh" });
  });
  api.route("GET", `${LIST}/`, reply(200, { entries: [fileInfo("/a")], next: null }));
  const fs = client.filesystem(FS);
  const e = await rejects(fs.changes({ cursor: "old", wait: 0 }));
  assert.deepEqual([e.status, e.code], [410, "cursor_expired"]);
  const cursor = (await fs.changes()).cursor;
  assert.deepEqual(
    (await fs.list()).entries.map((x) => x.path),
    ["/a"],
  );
  assert.equal((await fs.changes({ cursor, wait: 0 })).cursor, "fresh");
  assert.deepEqual(
    api.operations().map((r) => `${r.path}?${r.query.cursor}`),
    [`${CHANGES}?old`, `${CHANGES}?undefined`, `${LIST}/?undefined`, `${CHANGES}?fresh`],
  );
});

// --- configuration ---------------------------------------------------------

test("C30 configuration defaults", async (t) => {
  const saved = { key: process.env.MOUNTABLE_API_KEY, url: process.env.MOUNTABLE_API_URL };
  t.after(() => {
    for (const [name, value] of [
      ["MOUNTABLE_API_KEY", saved.key],
      ["MOUNTABLE_API_URL", saved.url],
    ] as const) {
      if (value === undefined) delete process.env[name];
      else process.env[name] = value;
    }
  });
  delete process.env.MOUNTABLE_API_KEY;
  delete process.env.MOUNTABLE_API_URL;
  const e = await rejects(() => new Mountable());
  assert.equal(e.code, "unauthenticated");
  assert.equal(e.status, null);
  process.env.MOUNTABLE_API_KEY = API_KEY;
  assert.equal(new Mountable().baseUrl, "https://mountable.io");
  process.env.MOUNTABLE_API_URL = `${api.url}/`;
  const fromEnv = new Mountable();
  assert.equal(fromEnv.baseUrl, api.url);
  assert.equal(new Mountable({ baseUrl: "https://example.test" }).baseUrl, "https://example.test");
  await fromEnv.createMountTicket(FS);
  assert.equal(api.sessions()[0].headers.authorization, `Bearer ${API_KEY}`);
  await new Mountable({ apiKey: "mtbl_otherkeysecret" }).createMountTicket(FS);
  assert.equal(api.sessions()[1].headers.authorization, "Bearer mtbl_otherkeysecret");
});

test("C31 timeouts", async (t) => {
  // Shortened: the SDK's 60 s becomes 100 ms, and 60 s + wait becomes
  // 100 ms + wait.
  const original = globalThis.setTimeout;
  globalThis.setTimeout = ((...args: Parameters<typeof setTimeout>) => {
    const ms = args[1];
    if (ms !== undefined && ms >= 60_000 && ms <= 85_000) {
      args[1] = ms - 59_900;
    }
    return original(...args);
  }) as typeof setTimeout;
  t.after(() => {
    globalThis.setTimeout = original;
  });
  const fs = client.filesystem(FS);
  api.route("GET", `${LIST}/`, reply(200, { entries: [], next: null }));
  await fs.list(); // creates the session before anything is slow
  api.route("GET", `${FILES}/slow.md`, { ...reply(200, "abc"), delay: 500 });
  api.route("PUT", `${FILES}/slow.md`, { ...reply(200, fileInfo("/slow.md")), delay: 500 });
  api.route("GET", CHANGES, { ...reply(200, { events: [], cursor: "c" }), delay: 500 });
  let e = await rejects(fs.read("slow.md"));
  assert.deepEqual([e.status, e.code], [null, "network_error"]);
  e = await rejects(fs.write("slow.md", "x"));
  assert.deepEqual([e.status, e.code], [null, "network_error"]);
  assert.equal((await fs.changes({ cursor: "c", wait: 1 })).cursor, "c");
  e = await rejects(fs.changes({ cursor: "c", wait: 0 }));
  assert.deepEqual([e.status, e.code], [null, "network_error"]);
  const calls = api.operations().map((r) => `${r.method} ${r.path}`);
  assert.equal(calls.filter((c) => c === `GET ${FILES}/slow.md`).length, 2); // a read is retried once
  assert.equal(calls.filter((c) => c === `PUT ${FILES}/slow.md`).length, 1); // a write never
  assert.equal(calls.filter((c) => c === `GET ${CHANGES}`).length, 3); // one wait=1, two wait=0
});

test("C32 a redirect is an error", async () => {
  const moved = { Location: `${api.url}${FILES}/elsewhere.md` };
  api.route("DELETE", `${FILES}/a.md`, reply(302, "", moved));
  api.route("GET", `${FILES}/a.md`, reply(307, "", moved));
  api.route("POST", SESSIONS, (r) => (r.json().credential ? api.issueSession(r) : reply(303, "", moved)));
  const fs = client.filesystem(FS);
  const e = await rejects(fs.delete("a.md"));
  assert.deepEqual([e.status, e.code], [302, "api_error"]);
  assert.equal((await rejects(fs.read("a.md"))).status, 307);
  assert.equal((await rejects(client.createMountTicket(FS))).status, 303);
  assert.ok(!api.requests.some((r) => r.path.includes("elsewhere")));
});

// --- parity ----------------------------------------------------------------

test("every contract case has a test", () => {
  const contract = readFileSync(new URL("../../contract.md", import.meta.url), "utf8");
  const cases = new Set([...contract.matchAll(/^\| (C\d\d) \|/gm)].map((m) => m[1]));
  const source = readFileSync(new URL(import.meta.url), "utf8");
  const tested = new Set([...source.matchAll(/^test\("(C\d\d) /gm)].map((m) => m[1]));
  assert.ok(cases.size > 0);
  assert.deepEqual(tested, cases);
});
