import {
  assert,
  assertEquals,
  assertRejects,
  assertStringIncludes,
  assertThrows,
} from "@std/assert";
import { MountableError } from "mountable-sdk";
import {
  cleanupNote,
  createNote,
  encodeNote,
  openClient,
  readNote,
  readOnlyWriteCode,
} from "./main.ts";

const PATH = "notes/brief.md";
const TEXT = "Deno note stays.";
const SECRET = "mtbl_should_not_appear";

type Stored = { body: Uint8Array; etag: string };

function json(status: number, body: unknown): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { "content-type": "application/json" },
  });
}

function error(status: number, code: string): Response {
  return json(status, { detail: { code, message: code } });
}

/** A fresh ArrayBuffer view. TypeScript 6 rejects a SharedArrayBuffer-backed view as a response body. */
function responseBody(source: Uint8Array): Uint8Array<ArrayBuffer> {
  const bytes = new Uint8Array(source.byteLength);
  bytes.set(source);
  return bytes;
}

/** Speaks enough of the Files API for one note. */
function startFake(): {
  url: string;
  requests: string[];
  stop: () => Promise<void>;
} {
  const modes = new Map<string, string>();
  let stored: Stored | undefined;
  let tokens = 0;
  const requests: string[] = [];
  const server = Deno.serve(
    { port: 0, hostname: "127.0.0.1", onListen() {} },
    async (request) => {
      const url = new URL(request.url);
      const body = new Uint8Array(await request.arrayBuffer());
      requests.push(`${request.method} ${url.pathname}`);
      if (
        request.method === "POST" && url.pathname === "/api/v1/mount-sessions"
      ) {
        const session = JSON.parse(new TextDecoder().decode(body)) as {
          mode?: string;
        };
        tokens += 1;
        const token = `mtblst_token${tokens}`;
        modes.set(token, session.mode ?? "rw");
        return json(201, {
          token,
          expires_at: new Date(Date.now() + 3_600_000).toISOString(),
        });
      }
      const file = url.pathname === `/api/v1/filesystems/fs_demo/files/${PATH}`;
      if (!file) return error(404, "not_found");
      const token =
        request.headers.get("authorization")?.slice("Bearer ".length) ?? "";
      if (request.method === "PUT") {
        if (modes.get(token) === "ro") return error(403, "read_only");
        if (
          request.headers.get("if-none-match") === "*" && stored
        ) return error(412, "precondition_failed");
        stored = { body, etag: '"e1"' };
        return json(201, {
          path: `/${PATH}`,
          type: "file",
          size: body.byteLength,
          etag: "e1",
          modified_at: "2026-10-27T00:00:00Z",
        });
      }
      if (request.method === "GET") {
        if (!stored) return error(404, "not_found");
        return new Response(responseBody(stored.body), {
          headers: {
            "content-type": "application/octet-stream",
            "content-length": String(stored.body.byteLength),
            "last-modified": "Tue, 27 Oct 2026 00:00:00 GMT",
            etag: stored.etag,
          },
        });
      }
      if (request.method === "DELETE") {
        if (request.headers.get("if-match") !== stored?.etag) {
          return error(
            412,
            "precondition_failed",
          );
        }
        stored = undefined;
        return new Response(null, { status: 204 });
      }
      return error(404, "not_found");
    },
  );
  const port = (server.addr as Deno.NetAddr).port;
  return {
    url: `http://127.0.0.1:${port}`,
    requests,
    stop: () => server.shutdown(),
  };
}

function handles(baseUrl: string) {
  const client = openClient(SECRET, baseUrl);
  return {
    writer: client.filesystem("fs_demo", "rw"),
    reader: client.filesystem("fs_demo", "ro"),
  };
}

Deno.test("create-only write preserves the first note", async () => {
  const fake = startFake();
  try {
    const { writer, reader } = handles(fake.url);
    await createNote(writer, PATH, TEXT);
    await assertRejects(
      () => createNote(writer, PATH, "replacement"),
      MountableError,
      "precondition_failed",
    );
    assertEquals(await readNote(reader, PATH), TEXT);
  } finally {
    await fake.stop();
  }
});

Deno.test("a read-only handle cannot write", async () => {
  const fake = startFake();
  try {
    const { reader } = handles(fake.url);
    assertEquals(await readOnlyWriteCode(reader, PATH, TEXT), "read_only");
    await assertRejects(
      () => readNote(reader, PATH),
      MountableError,
      "not_found",
    );
  } finally {
    await fake.stop();
  }
});

Deno.test("cleanup deletes only the matching note", async () => {
  const fake = startFake();
  try {
    const { writer, reader } = handles(fake.url);
    await createNote(writer, PATH, TEXT);
    await assertRejects(
      () => cleanupNote(writer, PATH, "other"),
      Error,
      "Cleanup refused",
    );
    assertEquals(await readNote(reader, PATH), TEXT);
    await cleanupNote(writer, PATH, TEXT);
    await assertRejects(
      () => readNote(reader, PATH),
      MountableError,
      "not_found",
    );
  } finally {
    await fake.stop();
  }
});

Deno.test("refuses a bad path or an oversized note before any request", async () => {
  const fake = startFake();
  try {
    const { writer } = handles(fake.url);
    await assertRejects(
      () => createNote(writer, "other/brief.md", TEXT),
      Error,
      "notes/brief.md",
    );
    await assertRejects(
      () => createNote(writer, PATH, "x".repeat(16 * 1024 + 1)),
      Error,
      "16 KiB",
    );
    assertEquals(fake.requests.length, 0);
  } finally {
    await fake.stop();
  }
});

Deno.test("a fresh process reads the note the first process stored", async () => {
  const fake = startFake();
  try {
    const wrote = await command(fake.url, ["write", "--text", TEXT]);
    assertEquals(wrote.code, 0);
    assertEquals(wrote.stdout.trim(), "stored");
    const read = await command(fake.url, ["read"]);
    assertEquals(read.code, 0);
    assertEquals(read.stdout.trim(), TEXT);
    assert(!read.stdout.includes(SECRET) && !read.stderr.includes(SECRET));
  } finally {
    await fake.stop();
  }
});

Deno.test("a missing environment permission is refused before any request", async () => {
  const result = await command(undefined, ["write", "--text", TEXT], {
    allowEnv: false,
    allowNet: false,
  });
  assertEquals(result.code, 1);
  assertStringIncludes(
    result.stderr,
    'Requires env access to "MOUNTABLE_API_KEY"',
  );
  assert(!result.stdout.includes(SECRET));
  assert(!result.stderr.includes(SECRET));
});

Deno.test("an unpermitted API URL is ignored and the default host is used", async () => {
  const fake = startFake();
  try {
    const output = await new Deno.Command(Deno.execPath(), {
      args: [
        "run",
        "--no-prompt",
        "--allow-env=MOUNTABLE_API_KEY,MOUNTABLE_FILESYSTEM_ID,MOUNTABLE_NOTE_PATH",
        "--allow-net=127.0.0.1",
        new URL("./main.ts", import.meta.url).pathname,
        "write",
        "--text",
        TEXT,
      ],
      clearEnv: true,
      env: {
        MOUNTABLE_API_KEY: SECRET,
        MOUNTABLE_FILESYSTEM_ID: "fs_demo",
        MOUNTABLE_NOTE_PATH: PATH,
        MOUNTABLE_API_URL: fake.url,
      },
      stdout: "piped",
      stderr: "piped",
    }).output();
    const stderr = new TextDecoder().decode(output.stderr);
    assertEquals(output.code, 1);
    assertEquals(stderr.trim(), "network_error");
    assertEquals(fake.requests.length, 0);
    assert(!stderr.includes(SECRET));
  } finally {
    await fake.stop();
  }
});

Deno.test("a missing network permission does not reach the service", async () => {
  const fake = startFake();
  try {
    const result = await command(fake.url, ["write", "--text", TEXT], {
      allowNet: false,
    });
    assertEquals(result.code, 1);
    assertEquals(result.stderr.trim(), "network_error");
    assertEquals(fake.requests.length, 0);
    assert(!result.stderr.includes(SECRET));
  } finally {
    await fake.stop();
  }
});

Deno.test("encodeNote rejects empty text", () => {
  assertThrows(() => encodeNote(""), Error, "nonempty");
});

type CommandOptions = { allowEnv?: boolean; allowNet?: boolean };

function command(
  baseUrl: string | undefined,
  args: string[],
  options: CommandOptions = {},
) {
  const allowEnv = options.allowEnv !== false;
  const allowNet = options.allowNet !== false;
  const flags = ["run", "--no-prompt"];
  if (allowEnv) {
    flags.push(
      "--allow-env=MOUNTABLE_API_KEY,MOUNTABLE_FILESYSTEM_ID,MOUNTABLE_NOTE_PATH,MOUNTABLE_API_URL",
    );
  }
  if (allowNet) flags.push("--allow-net=127.0.0.1");
  flags.push(new URL("./main.ts", import.meta.url).pathname, ...args);
  const env: Record<string, string> = {
    MOUNTABLE_API_KEY: SECRET,
    MOUNTABLE_FILESYSTEM_ID: "fs_demo",
    MOUNTABLE_NOTE_PATH: PATH,
  };
  if (baseUrl) env.MOUNTABLE_API_URL = baseUrl;
  return new Deno.Command(Deno.execPath(), {
    args: flags,
    clearEnv: true,
    env,
    stdout: "piped",
    stderr: "piped",
  }).output().then((output) => ({
    code: output.code,
    stdout: new TextDecoder().decode(output.stdout),
    stderr: new TextDecoder().decode(output.stderr),
  }));
}
