import assert from "node:assert/strict";
import { spawn } from "node:child_process";
import { mkdtemp, readFile, rm } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { fileURLToPath } from "node:url";
import { test } from "node:test";
import { MAX_BYTES } from "../store.mjs";
import { pathsFor, upload, validate } from "../failure.mjs";
import { API_KEY, FS, startFakeApi } from "./fake-api.mjs";

const root = fileURLToPath(new URL("..", import.meta.url));
function cli(args, url, extraEnv = {}) {
  return new Promise((resolve, reject) => {
    const child = spawn(process.execPath, [join(root, "failure.mjs"), ...args], {
      cwd: root,
      env: { ...process.env, MOUNTABLE_API_KEY: API_KEY, MOUNTABLE_API_URL: url, ...extraEnv },
    });
    let stdout = "", stderr = "";
    child.stdout.on("data", (chunk) => stdout += chunk);
    child.stderr.on("data", (chunk) => stderr += chunk);
    child.on("error", reject);
    child.on("close", (code) => resolve({ code, stdout, stderr }));
  });
}

test("a real failed runner retains two closed artifacts; fresh reader matches; failures stay nonzero", { timeout: 120_000 }, async () => {
  const api = await startFakeApi();
  const dir = await mkdtemp(join(tmpdir(), "pw-test-recipe-"));
  const out = join(dir, "out");
  const args = ["--filesystem", FS, "--run", "failed1"];
  try {
    const uninstalled = await cli(["run", ...args, "--dir", join(dir, "missing-browser")], api.url, { PLAYWRIGHT_BROWSERS_PATH: join(dir, "no-browsers") });
    assert.equal(uninstalled.code, 1);
    assert.match(uninstalled.stderr, /nothing uploaded.*playwright install chromium/);
    assert.doesNotMatch(uninstalled.stderr, /partial writes/);
    assert.equal(api.requests.length, 0);
    const failed = await cli(["run", ...args, "--dir", out], api.url);
    assert.equal(failed.code, 1);
    assert.match(failed.stdout, /^retained runner_exit=1\n/);
    assert.equal(failed.stderr, "");
    const files = await Promise.all(["trace.zip", "report.json"].map((name) => readFile(join(out, name))));
    validate(files);
    assert.equal(JSON.parse(files[1]).stats.unexpected, 1);
    for (const bytes of files) assert.equal(bytes.includes(API_KEY), false);
    const checked = await cli(["check", ...args, "--dir", out], api.url);
    assert.equal(checked.code, 0, checked.stderr);
    assert.match(checked.stdout, /^match playwright-test\/failed1\/trace.zip/);
    assert.deepEqual(await readFile(join(out, "downloaded", "trace.zip")), files[0]);
    const sessions = api.requests.filter((r) => r.method === "POST" && r.path.endsWith("mount-sessions"));
    assert.deepEqual(sessions.map((r) => JSON.parse(r.body).mode), ["rw", "ro"]);

    // Per-file and combined size checks run before any write.
    let writes = 0;
    for (const pair of [[Buffer.alloc(MAX_BYTES + 1), files[1]], [files[0], Buffer.alloc(MAX_BYTES)]]) {
      await assert.rejects(() => upload({ write() { writes++; } }, pathsFor("large"), pair), { code: "too_large" });
    }
    assert.equal(writes, 0);
    const report = JSON.parse(files[1]);
    report.stats.unexpected = 0;
    assert.throws(() => validate([files[0], Buffer.from(JSON.stringify(report))]), { code: "bad_report" });
    assert.throws(() => pathsFor("../escape"), { code: "bad_run" });

    const duplicate = await cli(["run", ...args, "--dir", join(dir, "duplicate")], api.url);
    assert.equal(duplicate.code, 1);
    assert.match(duplicate.stderr, /retention_failed.*precondition_failed/);
    assert.doesNotMatch(duplicate.stdout, /retained/);
    assert.deepEqual(api.files.get(pathsFor("failed1")[0]), files[0]);

    for (const failure of [{ status: 402, code: "filesystem_spending_cap_reached" }, { status: 503, code: "unavailable" }]) {
      api.setFailTrace(failure);
      const refused = await cli(["run", "--filesystem", FS, "--run", `cap${failure.status}`, "--dir", join(dir, `cap${failure.status}`)], api.url);
      assert.equal(refused.code, 1);
      assert.match(refused.stderr, /retention_failed/);
      assert.doesNotMatch(refused.stdout, /retained/);
      assert.equal(refused.stderr.includes(API_KEY), false);
    }
    api.setFailTrace(null);
    api.files.set(pathsFor("failed1")[0], Buffer.from("changed"));
    const changed = await cli(["check", ...args, "--dir", out], api.url);
    assert.equal(changed.code, 1);
    assert.match(changed.stderr, /mismatch/);
    const clean = await cli(["cleanup", ...args], api.url);
    assert.equal(clean.code, 0, clean.stderr);
    assert.equal(api.dirs.has("playwright-test/failed1"), false);
    const missing = await cli(["check", ...args, "--dir", out], api.url);
    assert.equal(missing.code, 1);
    assert.match(missing.stderr, /not_found/);
  } finally {
    await api.close();
    await rm(dir, { recursive: true, force: true });
  }
});
