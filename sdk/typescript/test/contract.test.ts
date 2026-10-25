// The cases of sdk/contract.md, one test per case ID.

import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { inspect } from "node:util";
import { afterEach, beforeEach, test } from "node:test";

import { Mountable, MountableError, verifyWebhook, Webhook } from "../src/index.ts";
import {
  API_KEY,
  CHANGES,
  error,
  FILES,
  FILESYSTEMS,
  FS,
  fileInfo,
  filesystemView,
  LIST,
  ME,
  MockAPI,
  me,
  type Reply,
  type Request,
  reply,
  SESSIONS,
  WEBHOOK_SECRET,
  WEBHOOKS,
  webhookView,
} from "./mock-api.ts";

const MODIFIED = new Date("2026-10-17T12:00:00.123Z");
const CREATED = new Date("2026-10-01T09:30:00Z");
const READ_REPLY = reply(200, "abc", { ETag: '"e1"', "Last-Modified": "Sat, 17 Oct 2026 12:00:00 GMT" });

let api: MockAPI;
let client: Mountable;

beforeEach(async () => {
  api = new MockAPI();
  await api.start();
  delete process.env.MOUNTABLE_ORG;
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

// --- management ------------------------------------------------------------

test("C33 the organisation is resolved once", async (t) => {
  api.route("GET", FILESYSTEMS, reply(200, []));
  await Promise.all([client.listFilesystems(), client.listFilesystems()]); // concurrent first calls share one /me
  await client.listFilesystems();
  assert.deepEqual(api.calls(), [`GET ${ME}`, `GET ${FILESYSTEMS}`, `GET ${FILESYSTEMS}`, `GET ${FILESYSTEMS}`]);
  assert.equal(api.requests[0].headers.authorization, `Bearer ${API_KEY}`);
  t.after(() => {
    delete process.env.MOUNTABLE_ORG;
  });
  process.env.MOUNTABLE_ORG = "org_env";
  api.route("GET", "/api/v1/orgs/org_env/filesystems", reply(200, []));
  api.route("GET", "/api/v1/orgs/org_arg/filesystems", reply(200, []));
  await new Mountable({ apiKey: API_KEY, baseUrl: api.url }).listFilesystems();
  await new Mountable({ apiKey: API_KEY, baseUrl: api.url, orgId: "org_arg" }).listFilesystems();
  assert.deepEqual(api.calls().slice(4), ["GET /api/v1/orgs/org_env/filesystems", "GET /api/v1/orgs/org_arg/filesystems"]);
});

test("C34 no organisation raises org_required", async () => {
  api.route("GET", ME, reply(200, me({ api_key: null })));
  let e = await rejects(client.listFilesystems());
  assert.deepEqual([e.status, e.code], [null, "org_required"]);
  assert.deepEqual(api.calls(), [`GET ${ME}`]);
  api.route("GET", ME, error(401, "unauthenticated"));
  e = await rejects(client.getFilesystem(FS));
  assert.deepEqual([e.status, e.code], [401, "unauthenticated"]);
});

test("C35 list and get filesystems", async () => {
  const capped = filesystemView({ id: "fs_two", spending_cap_cents: 500, mutations_per_minute: 60 });
  api.route("GET", FILESYSTEMS, reply(200, [filesystemView(), capped]));
  api.route("GET", `${FILESYSTEMS}/${FS}`, reply(200, filesystemView()));
  const [first, second] = await client.listFilesystems();
  const expected = {
    id: FS,
    name: "notes",
    byteQuota: 2 ** 30,
    entryQuota: 10_000,
    spendingCapCents: null,
    mutationsPerMinute: null,
    createdAt: CREATED,
  };
  assert.deepEqual(first, expected);
  assert.deepEqual([second.id, second.spendingCapCents, second.mutationsPerMinute], ["fs_two", 500, 60]);
  assert.deepEqual(await client.getFilesystem(FS), expected);
  assert.deepEqual(api.calls().slice(1), [`GET ${FILESYSTEMS}`, `GET ${FILESYSTEMS}/${FS}`]);
  assert.deepEqual(new Set(api.requests.map((r) => r.headers.authorization)), new Set([`Bearer ${API_KEY}`]));
  assert.deepEqual(api.sessions(), []);
});

test("C36 createFilesystem sends only the limits given", async () => {
  api.route("POST", FILESYSTEMS, (r) => reply(201, filesystemView(r.json())));
  assert.equal((await client.createFilesystem("notes")).name, "notes");
  const info = await client.createFilesystem("capped", { spendingCapCents: 0, byteQuota: 2 ** 32 });
  assert.deepEqual([info.spendingCapCents, info.byteQuota], [0, 2 ** 32]);
  await client.createFilesystem("all", { spendingCapCents: 1, mutationsPerMinute: 2, byteQuota: 3, entryQuota: 4 });
  assert.deepEqual(
    api.operations().map((r) => [r.method, r.json()]),
    [
      ["POST", { name: "notes" }],
      ["POST", { name: "capped", spending_cap_cents: 0, byte_quota: 2 ** 32 }],
      ["POST", { name: "all", spending_cap_cents: 1, mutations_per_minute: 2, byte_quota: 3, entry_quota: 4 }],
    ],
  );
});

test("C37 updateFilesystem sends only the fields passed", async () => {
  api.route("PATCH", `${FILESYSTEMS}/${FS}`, (r) => reply(200, filesystemView(r.json())));
  let info = await client.updateFilesystem(FS, { spendingCapCents: null, entryQuota: 20_000 });
  assert.equal(info.spendingCapCents, null);
  assert.equal(info.entryQuota, 20_000);
  await client.updateFilesystem(FS, { mutationsPerMinute: null });
  info = await client.updateFilesystem(FS, { spendingCapCents: 1000, mutationsPerMinute: 120, byteQuota: 2 ** 31 });
  assert.deepEqual(info, {
    id: FS,
    name: "notes",
    byteQuota: 2 ** 31,
    entryQuota: 10_000,
    spendingCapCents: 1000,
    mutationsPerMinute: 120,
    createdAt: CREATED,
  });
  assert.deepEqual(
    api.operations().map((r) => [r.method, r.json()]),
    [
      ["PATCH", { spending_cap_cents: null, entry_quota: 20_000 }],
      ["PATCH", { mutations_per_minute: null }],
      ["PATCH", { spending_cap_cents: 1000, mutations_per_minute: 120, byte_quota: 2 ** 31 }],
    ],
  );
});

const USAGE = {
  retained_bytes: 10,
  byte_quota: 2 ** 30,
  entries: 2,
  entry_quota: 10_000,
  bytes_read_30d: 30,
  bytes_written_30d: 40,
  reserved_cents: 0,
  currency: null,
};

test("C38 filesystemUsage", async () => {
  api.route("GET", `${FILESYSTEMS}/${FS}/usage`, reply(200, USAGE));
  assert.deepEqual(await client.filesystemUsage(FS), {
    retainedBytes: 10,
    byteQuota: 2 ** 30,
    entries: 2,
    entryQuota: 10_000,
    bytesRead30d: 30,
    bytesWritten30d: 40,
    reservedCents: 0,
    currency: null,
  });
  assert.equal(api.calls().at(-1), `GET ${FILESYSTEMS}/${FS}/usage`);
});

test("C39 list and revoke mount sessions", async () => {
  const session = {
    id: "s9",
    filesystem_id: FS,
    mode: "ro",
    credential: "token",
    state: "active",
    created_by: "api key test",
    expires_at: "2026-10-17T13:00:00Z",
    created_at: "2026-10-17T12:00:00Z",
  };
  api.route("GET", `${FILESYSTEMS}/${FS}/mount-sessions`, reply(200, [session]));
  api.route("DELETE", `${SESSIONS}/s9`, reply(204));
  assert.equal(await client.revokeMountSession("s9"), undefined);
  assert.deepEqual(await client.listMountSessions(FS), [
    {
      id: "s9",
      filesystemId: FS,
      mode: "ro",
      credential: "token",
      state: "active",
      createdBy: "api key test",
      expiresAt: new Date("2026-10-17T13:00:00Z"),
      createdAt: new Date("2026-10-17T12:00:00Z"),
    },
  ]);
  assert.deepEqual(api.calls(), [`DELETE ${SESSIONS}/s9`, `GET ${ME}`, `GET ${FILESYSTEMS}/${FS}/mount-sessions`]);
});

test("C40 create and list webhooks", async () => {
  api.route("POST", WEBHOOKS, (r) => reply(201, webhookView({ secret: WEBHOOK_SECRET, ...r.json() })));
  let hook = await client.createWebhook(FS, "https://example.test/hook");
  assert.ok(hook instanceof Webhook);
  assert.deepEqual(
    [hook.id, hook.filesystemId, hook.url, hook.pathPrefix, hook.state, hook.disabledReason, hook.createdBy, hook.createdAt],
    ["wh_one", FS, "https://example.test/hook", "/", "active", null, "api key test", CREATED],
  );
  assert.deepEqual([hook.lastSuccessAt, hook.lastFailureAt, hook.lastFailure, hook.failingSince], [null, null, null, null]);
  assert.equal(hook.secret, WEBHOOK_SECRET);
  hook = await client.createWebhook(FS, "https://example.test/t", { pathPrefix: "/tickets" });
  assert.equal(hook.pathPrefix, "/tickets");
  assert.deepEqual(
    api.operations().map((r) => r.json()),
    [{ url: "https://example.test/hook" }, { url: "https://example.test/t", path_prefix: "/tickets" }],
  );
  const failing = webhookView({
    id: "wh_two",
    state: "disabled",
    disabled_reason: "failing",
    last_success_at: "2026-10-02T00:00:00Z",
    last_failure_at: "2026-10-03T00:00:00Z",
    last_failure: "HTTP 500",
    failing_since: "2026-10-02T12:00:00Z",
  });
  api.route("GET", WEBHOOKS, reply(200, [webhookView(), failing]));
  const [first, second] = await client.listWebhooks(FS);
  assert.equal(first.secret, null);
  assert.equal(first.lastFailureAt, null);
  assert.deepEqual([second.state, second.disabledReason, second.lastFailure], ["disabled", "failing", "HTTP 500"]);
  assert.deepEqual(second.lastSuccessAt, new Date("2026-10-02T00:00:00Z"));
  assert.deepEqual(second.lastFailureAt, new Date("2026-10-03T00:00:00Z"));
  assert.deepEqual(second.failingSince, new Date("2026-10-02T12:00:00Z"));
  assert.equal(second.secret, null);
});

test("C41 delete, rotate and enable webhooks", async () => {
  const hook = `${WEBHOOKS}/wh_one`;
  const rotated = "whsec_bmV3c2VjcmV0";
  api.route("DELETE", hook, reply(204));
  api.route("POST", `${hook}:rotate-secret`, reply(200, webhookView({ secret: rotated })));
  api.route("POST", `${hook}:enable`, reply(200, webhookView()));
  assert.equal(await client.deleteWebhook(FS, "wh_one"), undefined);
  assert.equal((await client.rotateWebhookSecret(FS, "wh_one")).secret, rotated);
  const enabled = await client.enableWebhook(FS, "wh_one");
  assert.equal(enabled.state, "active");
  assert.equal(enabled.secret, null);
  assert.deepEqual(
    api.operations().map((r) => `${r.method} ${r.path}`),
    [`DELETE ${hook}`, `POST ${hook}:rotate-secret`, `POST ${hook}:enable`],
  );
});

test("C42 IDs are encoded as path segments", async () => {
  const fs = "/api/v1/orgs/org%2F1/filesystems/a%20b%2F%C5%BE";
  api.route("GET", fs, reply(200, filesystemView()));
  api.route("POST", `${fs}/webhooks/wh%3F1:enable`, reply(200, webhookView()));
  api.route("DELETE", `${SESSIONS}/s%231`, reply(204));
  const c = new Mountable({ apiKey: API_KEY, baseUrl: api.url, orgId: "org/1" });
  await c.getFilesystem("a b/ž");
  await c.enableWebhook("a b/ž", "wh?1");
  await c.revokeMountSession("s#1");
  assert.deepEqual(api.calls(), [`GET ${fs}`, `POST ${fs}/webhooks/wh%3F1:enable`, `DELETE ${SESSIONS}/s%231`]);
});

test("C43 webhook secrets are hidden", async () => {
  api.route("POST", WEBHOOKS, reply(201, webhookView({ secret: WEBHOOK_SECRET })));
  const hook = await client.createWebhook(FS, "https://example.test/hook");
  assert.equal(hook.secret, WEBHOOK_SECRET);
  for (const shown of [String(hook), inspect(hook), JSON.stringify(hook), JSON.stringify({ ...hook })]) {
    assert.ok(!shown.includes("whsec_"), shown);
  }
  const echo = `refused ${WEBHOOK_SECRET}`;
  api.route("POST", `${WEBHOOKS}/wh_one:rotate-secret`, error(409, "conflict", { message: echo }));
  const e = await rejects(client.rotateWebhookSecret(FS, "wh_one"));
  assert.equal(e.code, "conflict");
  for (const shown of [String(e), inspect(e), e.message, e.stack ?? "", JSON.stringify(e)]) {
    assert.ok(!shown.includes(WEBHOOK_SECRET.slice(6)), shown);
  }
  assert.ok(e.message.includes("whsec_[redacted]"));
});

test("C44 repeatable management calls are retried once", async () => {
  const fs = `${FILESYSTEMS}/${FS}`;
  const routes: [string, string, Reply][] = [
    ["GET", ME, reply(200, me())],
    ["GET", FILESYSTEMS, reply(200, [])],
    ["GET", fs, reply(200, filesystemView())],
    ["GET", `${fs}/usage`, reply(200, USAGE)],
    ["GET", `${fs}/mount-sessions`, reply(200, [])],
    ["GET", WEBHOOKS, reply(200, [])],
    ["DELETE", `${SESSIONS}/s9`, reply(204)],
    ["POST", `${WEBHOOKS}/wh_one:enable`, reply(200, webhookView())],
  ];
  for (const [method, path, answer] of routes) {
    api.route(method, path, failFirst(1, "drop", answer));
  }
  await client.listFilesystems();
  await client.getFilesystem(FS);
  await client.filesystemUsage(FS);
  await client.listMountSessions(FS);
  await client.listWebhooks(FS);
  await client.revokeMountSession("s9");
  await client.enableWebhook(FS, "wh_one");
  const calls = api.calls();
  assert.deepEqual(
    routes.map(([method, path]) => calls.filter((c) => c === `${method} ${path}`).length),
    routes.map(() => 2),
  );
  api.route("GET", FILESYSTEMS, failFirst(2, "drop", reply(200, [])));
  assert.equal((await rejects(client.listFilesystems())).code, "network_error");
  assert.equal(api.calls().filter((c) => c === `GET ${FILESYSTEMS}`).length, 4);
});

test("C45 management writes are never retried", async () => {
  const fs = `${FILESYSTEMS}/${FS}`;
  const hook = `${WEBHOOKS}/wh_one`;
  const routes: [string, string, Reply][] = [
    ["POST", FILESYSTEMS, reply(201, filesystemView())],
    ["PATCH", fs, reply(200, filesystemView())],
    ["POST", WEBHOOKS, reply(201, webhookView({ secret: WEBHOOK_SECRET }))],
    ["POST", `${hook}:rotate-secret`, reply(200, webhookView({ secret: "whsec_x" }))],
    ["DELETE", hook, reply(204)],
  ];
  for (const [method, path, answer] of routes) {
    api.route(method, path, failFirst(1, "drop", answer));
  }
  const calls = [
    () => client.createFilesystem("notes"),
    () => client.updateFilesystem(FS, { entryQuota: 20_000 }),
    () => client.createWebhook(FS, "https://example.test/hook"),
    () => client.rotateWebhookSecret(FS, "wh_one"),
    () => client.deleteWebhook(FS, "wh_one"),
  ];
  for (const call of calls) {
    assert.equal((await rejects(call)).code, "network_error");
  }
  assert.deepEqual(
    api.operations().map((r) => `${r.method} ${r.path}`),
    routes.map(([method, path]) => `${method} ${path}`),
  );
});

test("C46 management errors propagate", async () => {
  const fs = `${FILESYSTEMS}/${FS}`;
  const invalid = { detail: [{ loc: ["body", "spending_cap_cents"], msg: "too low" }] };
  api.route("POST", FILESYSTEMS, error(403, "key_limited_to_filesystem"));
  api.route("PATCH", fs, (r) =>
    r.json().spending_cap_cents === -1 ? reply(422, invalid) : error(403, "key_limited_to_filesystem"),
  );
  api.route("GET", WEBHOOKS, error(403, "forbidden", { message: "needs read-write" }));
  api.route("DELETE", `${WEBHOOKS}/wh_gone`, error(404, "not_found"));
  let e = await rejects(client.createFilesystem("notes"));
  assert.deepEqual([e.status, e.code], [403, "key_limited_to_filesystem"]);
  e = await rejects(client.updateFilesystem(FS, { byteQuota: 2 ** 31 }));
  assert.deepEqual([e.status, e.code], [403, "key_limited_to_filesystem"]);
  e = await rejects(client.updateFilesystem(FS, { spendingCapCents: -1 }));
  assert.deepEqual([e.status, e.code], [422, "invalid_request"]);
  assert.ok(e.message.includes("spending_cap_cents"));
  e = await rejects(client.listWebhooks(FS));
  assert.deepEqual([e.status, e.code, e.message], [403, "forbidden", "needs read-write"]);
  e = await rejects(client.deleteWebhook(FS, "wh_gone"));
  assert.deepEqual([e.status, e.code], [404, "not_found"]);
});

test("C47 dot-segment IDs are refused", async () => {
  const calls: (() => Promise<unknown>)[] = [];
  for (const bad of ["", ".", ".."]) {
    calls.push(
      () => client.getFilesystem(bad),
      () => client.updateFilesystem(bad, { byteQuota: 1 }),
      () => client.filesystemUsage(bad),
      () => client.listMountSessions(bad),
      () => client.revokeMountSession(bad),
      () => client.listWebhooks(bad),
      () => client.createWebhook(bad, "https://x.example"),
      () => client.deleteWebhook(FS, bad),
      () => client.rotateWebhookSecret(FS, bad),
      () => client.enableWebhook(FS, bad),
    );
  }
  for (const call of calls) {
    const e = await rejects(call);
    assert.deepEqual([e.status, e.code], [null, "invalid_request"]);
  }
  const badOrg = new Mountable({ apiKey: API_KEY, baseUrl: api.url, orgId: ".." });
  assert.equal((await rejects(badOrg.listWebhooks(FS))).code, "invalid_request");
  assert.deepEqual(api.requests, []); // nothing sent, not even the /me lookup
});

// --- webhook deliveries ----------------------------------------------------

const HOOK = "33333333-3333-3333-3333-333333333333";
const VECTOR = JSON.parse(readFileSync(new URL("../../testdata/webhook.json", import.meta.url), "utf8")) as {
  secret: string;
  id: string;
  timestamp: number;
  reference_body: string;
  reference_signature: string;
  body: string;
  signature: string;
};

function signedHeaders(signature = VECTOR.signature): Record<string, string> {
  return {
    "Webhook-Id": VECTOR.id,
    "WEBHOOK-TIMESTAMP": String(VECTOR.timestamp),
    "webhook-signature": signature,
  };
}

test("C48 verifyWebhook accepts a signed delivery", async (t) => {
  t.mock.method(Date, "now", () => (VECTOR.timestamp + 10) * 1000);
  const message = await verifyWebhook(VECTOR.secret, signedHeaders(), new TextEncoder().encode(VECTOR.body));
  assert.deepEqual(message, {
    id: VECTOR.id,
    type: "files.changed",
    timestamp: new Date("2021-02-25T15:02:10.123Z"),
    webhookId: HOOK,
    filesystemId: FS,
    events: [
      {
        type: "created",
        path: "/tickets/AB-42.md",
        file: {
          path: "/tickets/AB-42.md",
          type: "file",
          size: 1234,
          etag: '"3f2a"',
          modifiedAt: new Date("2021-02-25T15:02:09Z"),
        },
      },
      { type: "deleted", path: "/tickets/AB-41.md", file: null },
    ],
    cursor: "48213",
  });
  // A string body, Headers, and a rotation's two signatures with ours second.
  const other = `v1,${"A".repeat(43)}=`;
  const rotation = new Headers(signedHeaders(`${other} ${VECTOR.signature}`));
  assert.deepEqual(await verifyWebhook(VECTOR.secret, rotation, VECTOR.body), message);
  // Ours first: in one header, in a repeated header (Headers joins it with
  // ", "), and as Node's array of values in either order.
  const repeated = new Headers(signedHeaders(VECTOR.signature));
  repeated.append("webhook-signature", other);
  const variants: Parameters<typeof verifyWebhook>[1][] = [
    signedHeaders(`${VECTOR.signature} ${other}`),
    repeated,
    { ...signedHeaders(), "webhook-signature": [VECTOR.signature, other] },
    { ...signedHeaders(), "webhook-signature": [other, VECTOR.signature] },
  ];
  for (const headers of variants) {
    assert.equal((await verifyWebhook(VECTOR.secret, headers, VECTOR.body)).cursor, "48213");
  }
});

test("C49 verifyWebhook refuses anything else", async (t) => {
  const now = t.mock.method(Date, "now", () => (VECTOR.timestamp + 10) * 1000);
  const { secret, body } = VECTOR;
  const headers = signedHeaders();
  const { "Webhook-Id": _, ...missing } = headers;
  const cases: [string, Record<string, string>, string][] = [
    [secret, headers, `${body} `],
    [`whsec_${"A".repeat(43)}=`, headers, body],
    [secret, missing, body],
    [secret, signedHeaders(VECTOR.signature.slice(3)), body],
    ["MfKQ9r8GKYqrTwjUPD8ILPZIo2LaLaSw", headers, body],
    ["whsec_not base64!", headers, body],
  ];
  for (const digits of [13, 400, 5000]) {
    cases.push([secret, { ...headers, "WEBHOOK-TIMESTAMP": "9".repeat(digits) }, body]);
  }
  for (const [caseSecret, caseHeaders, caseBody] of cases) {
    const e = await rejects(verifyWebhook(caseSecret, caseHeaders, caseBody));
    assert.deepEqual([e.status, e.code], [null, "invalid_webhook"]);
    assert.ok(!String(e).includes("MfKQ9r8") && !inspect(e).includes("MfKQ9r8"));
  }
  // The gateway's own signature of a body that is not a message: the
  // signature passes, the body does not.
  const reference = { ...headers, "webhook-signature": VECTOR.reference_signature };
  const e = await rejects(verifyWebhook(secret, reference, VECTOR.reference_body));
  assert.equal(e.code, "invalid_webhook");
  assert.ok(e.message.includes("not a mountable.io webhook"), e.message);
  for (const [offset, ok] of [
    [-300, true],
    [300, true],
    [-301, false],
    [301, false],
  ] as const) {
    now.mock.mockImplementation(() => (VECTOR.timestamp + offset) * 1000);
    if (ok) {
      assert.equal((await verifyWebhook(secret, headers, body)).cursor, "48213");
    } else {
      assert.equal((await rejects(verifyWebhook(secret, headers, body))).code, "invalid_webhook");
    }
  }
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
