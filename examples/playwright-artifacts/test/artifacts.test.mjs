import assert from "node:assert/strict";
import { spawn } from "node:child_process";
import { mkdtemp, readFile, rm } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { test } from "node:test";
import { fileURLToPath } from "node:url";
import { inflateRawSync } from "node:zlib";

import { MountableError } from "mountable-sdk";

import {
  artifactPaths,
  assertClosedArtifacts,
  MAX_BYTES,
  RecipeError,
  removePair,
  uploadPair,
} from "../store.mjs";
import { API_KEY, FS, startFakeApi } from "./fake-api.mjs";

const root = fileURLToPath(new URL("..", import.meta.url));
const cli = join(root, "artifacts.mjs");

function png(extra = Buffer.from("ok")) {
  return Buffer.concat([Buffer.from([0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a]), extra]);
}

function zipWithTrace() {
  // A minimal stored zip: one empty entry named trace.trace.
  const name = Buffer.from("trace.trace");
  const local = Buffer.alloc(30 + name.length);
  local.writeUInt32LE(0x04034b50, 0);
  local.writeUInt16LE(name.length, 26);
  name.copy(local, 30);
  const central = Buffer.alloc(46 + name.length);
  central.writeUInt32LE(0x02014b50, 0);
  central.writeUInt16LE(name.length, 28);
  name.copy(central, 46);
  const eocd = Buffer.alloc(22);
  eocd.writeUInt32LE(0x06054b50, 0);
  eocd.writeUInt16LE(1, 8);
  eocd.writeUInt16LE(1, 10);
  eocd.writeUInt32LE(central.length, 12);
  eocd.writeUInt32LE(local.length, 16);
  return Buffer.concat([local, central, eocd]);
}

function memoryFs(files = new Map()) {
  return {
    files,
    writes: [],
    async write(path, data, options) {
      this.writes.push(path);
      if (options?.ifNoneMatch && this.files.has(path)) {
        throw new MountableError(412, "precondition_failed", "private detail");
      }
      this.files.set(path, Buffer.from(data));
    },
    async read(path) {
      if (!this.files.has(path)) {
        throw new MountableError(404, "not_found", "missing");
      }
      return { data: this.files.get(path) };
    },
    async delete(path) {
      if (!this.files.has(path)) {
        throw new MountableError(404, "not_found", "missing");
      }
      this.files.delete(path);
    },
  };
}

test("a bad run id never asks the filesystem to write", async () => {
  const fs = memoryFs();
  await assert.rejects(
    async () => uploadPair(fs, artifactPaths("../secret"), { screenshot: png(), trace: zipWithTrace() }),
    { code: "bad_run" },
  );
  assert.deepEqual(fs.writes, []);
});

test("an oversized screenshot is refused before a write", async () => {
  const fs = memoryFs();
  const screenshot = Buffer.alloc(MAX_BYTES + 1, 1);
  screenshot.set([0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a]);
  await assert.rejects(
    () => uploadPair(fs, artifactPaths("run1"), { screenshot, trace: zipWithTrace() }),
    (error) => error instanceof RecipeError && error.code === "too_large" && fs.writes.length === 0,
  );
});

test("a trace that is not a closed zip is refused before a write", async () => {
  const fs = memoryFs();
  await assert.rejects(
    () => uploadPair(fs, artifactPaths("run1"), { screenshot: png(), trace: Buffer.from("not a zip") }),
    (error) => error instanceof RecipeError && error.code === "bad_trace" && fs.writes.length === 0,
  );
});

test("a spending cap stops before the trace is uploaded", async () => {
  const fs = memoryFs();
  fs.write = async (path) => {
    fs.writes.push(path);
    throw new MountableError(402, "filesystem_spending_cap_reached", "private balance");
  };
  await assert.rejects(
    () => uploadPair(fs, artifactPaths("run1"), { screenshot: png(), trace: zipWithTrace() }),
    (error) => {
      assert.equal(error.code, "filesystem_spending_cap_reached");
      assert.equal(error.message.includes("private balance"), false);
      return true;
    },
  );
  assert.deepEqual(fs.writes, ["playwright/run1/screenshot.png"]);
});

test("an existing screenshot is left unchanged", async () => {
  const paths = artifactPaths("run1");
  const fs = memoryFs(new Map([[paths.screenshot, png(Buffer.from("kept"))]]));
  await assert.rejects(
    () => uploadPair(fs, paths, { screenshot: png(), trace: zipWithTrace() }),
    (error) => error.code === "precondition_failed",
  );
  assert.equal(fs.files.get(paths.screenshot).subarray(8).toString(), "kept");
  assert.equal(fs.files.has(paths.trace), false);
});

test("cleanup deletes both files and treats a missing file as already gone", async () => {
  const paths = artifactPaths("run1");
  const fs = memoryFs(
    new Map([
      [paths.screenshot, png()],
      [paths.trace, zipWithTrace()],
    ]),
  );
  await removePair(fs, paths);
  assert.equal(fs.files.has(paths.screenshot), false);
  assert.equal(fs.files.has(paths.trace), false);
  await removePair(fs, paths);
});

test("closed png and trace bytes pass the local checks", () => {
  assertClosedArtifacts({ screenshot: png(), trace: zipWithTrace() });
});

function zipText(bytes) {
  let eocd = -1;
  const start = Math.max(0, bytes.length - (22 + 65535));
  for (let offset = bytes.length - 22; offset >= start; offset -= 1) {
    if (bytes.readUInt32LE(offset) === 0x06054b50) {
      eocd = offset;
      break;
    }
  }
  assert.notEqual(eocd, -1);
  const count = bytes.readUInt16LE(eocd + 10);
  let offset = bytes.readUInt32LE(eocd + 16);
  const parts = [];
  for (let i = 0; i < count; i += 1) {
    const method = bytes.readUInt16LE(offset + 10);
    const compSize = bytes.readUInt32LE(offset + 20);
    const nameLen = bytes.readUInt16LE(offset + 28);
    const extraLen = bytes.readUInt16LE(offset + 30);
    const commentLen = bytes.readUInt16LE(offset + 32);
    const local = bytes.readUInt32LE(offset + 42);
    const localName = bytes.readUInt16LE(local + 26);
    const localExtra = bytes.readUInt16LE(local + 28);
    const data = bytes.subarray(local + 30 + localName + localExtra, local + 30 + localName + localExtra + compSize);
    const raw = method === 0 ? data : inflateRawSync(data);
    parts.push(raw.toString("utf8"));
    offset += 46 + nameLen + extraLen + commentLen;
  }
  return parts.join("\n");
}

function runCli(args, env) {
  return new Promise((resolve) => {
    const childEnv = { ...process.env, ...env };
    if (childEnv.NO_COLOR !== undefined && childEnv.FORCE_COLOR !== undefined) {
      delete childEnv.FORCE_COLOR;
    }
    const child = spawn(process.execPath, [cli, ...args], {
      cwd: root,
      env: childEnv,
    });
    const stdout = [];
    const stderr = [];
    child.stdout.on("data", (chunk) => stdout.push(chunk));
    child.stderr.on("data", (chunk) => stderr.push(chunk));
    child.on("close", (code) => {
      resolve({
        code,
        stdout: Buffer.concat(stdout).toString(),
        stderr: Buffer.concat(stderr).toString(),
      });
    });
  });
}

test("a fresh process downloads the same screenshot and trace bytes", { timeout: 120_000 }, async () => {
  const api = await startFakeApi();
  const dir = await mkdtemp(join(tmpdir(), "playwright-artifacts-"));
  const env = {
    MOUNTABLE_API_KEY: API_KEY,
    MOUNTABLE_API_URL: api.url,
  };
  try {
    const captured = await runCli(["capture", "--filesystem", FS, "--run", "run1", "--dir", dir], env);
    assert.equal(captured.stderr, "", captured.stderr);
    assert.equal(captured.code, 0, captured.stderr);
    assert.equal(captured.stdout.includes(API_KEY), false);
    const localShot = await readFile(join(dir, "screenshot.png"));
    const localTrace = await readFile(join(dir, "trace.zip"));
    const traceText = zipText(localTrace);
    assert.equal(traceText.includes(API_KEY), false);
    assert.equal(traceText.includes("PLAYWRIGHT-FIXTURE-OK"), true);
    assert.equal(traceText.includes("https://"), false);
    assert.match(traceText, /file:\/\//);

    const checked = await runCli(["check", "--filesystem", FS, "--run", "run1", "--dir", dir], env);
    assert.equal(checked.code, 0, checked.stderr);
    assert.match(checked.stdout, /^match playwright\/run1\/screenshot.png \d+\nmatch playwright\/run1\/trace.zip \d+\n$/);
    assert.equal(checked.stdout.includes(API_KEY), false);

    const again = await runCli(["capture", "--filesystem", FS, "--run", "run1", "--dir", dir], env);
    assert.equal(again.code, 1);
    assert.match(again.stderr, /precondition_failed/);
    assert.equal(again.stderr.includes("private"), false);
    const still = await runCli(["check", "--filesystem", FS, "--run", "run1", "--dir", dir], env);
    assert.equal(still.code, 0, still.stderr);
    assert.match(still.stdout, /^match /);

    const cleaned = await runCli(["cleanup", "--filesystem", FS, "--run", "run1"], env);
    assert.equal(cleaned.code, 0, cleaned.stderr);
    const missing = await runCli(["check", "--filesystem", FS, "--run", "run1", "--dir", dir], env);
    assert.equal(missing.code, 1);
    assert.match(missing.stderr, /not_found/);
    assert.equal(api.files.size, 0);
    assert.equal(api.dirs.has("playwright/run1"), false);
    assert.equal(api.dirs.has("playwright"), false);
    assert.equal(localShot[0], 0x89);
  } finally {
    await rm(dir, { recursive: true, force: true });
    await api.close();
  }
});
