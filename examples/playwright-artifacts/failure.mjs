// A failed runner stays failed even after its closed evidence is uploaded.
import { spawn } from "node:child_process";
import { mkdir, readFile, readdir, stat, writeFile } from "node:fs/promises";
import { join, resolve } from "node:path";
import { fileURLToPath } from "node:url";
import { Mountable, MountableError } from "mountable-sdk";
import { artifactPaths, assertSameBytes, assertTraceZip, MAX_BYTES, RecipeError, sha256 } from "./store.mjs";

const root = fileURLToPath(new URL(".", import.meta.url));
const names = ["trace.zip", "report.json"];

export function pathsFor(run) {
  artifactPaths(run); // Same validated run-id rule as the original companion.
  return names.map((name) => `playwright-test/${run}/${name}`);
}

export function validate(files) {
  if (files.some((file) => file.length > MAX_BYTES) || files.reduce((n, file) => n + file.length, 0) > MAX_BYTES) {
    throw new RecipeError("too_large", "Trace and report must total at most 4 MiB; nothing uploaded.");
  }
  assertTraceZip(files[0]);
  const report = JSON.parse(files[1].toString());
  const specs = report.suites.flatMap((suite) => suite.specs ?? []);
  const result = specs[0]?.tests[0]?.results[0];
  if (report.stats.unexpected !== 1 || report.errors.length || specs.length !== 1 ||
      specs[0].title !== "deliberate local assertion failure" || result?.status !== "failed" ||
      !result.error?.message?.includes("INTENTIONAL-FAILURE")) {
    throw new RecipeError("bad_report", "Expected the synthetic assertion failure; nothing uploaded.");
  }
}

export async function upload(fs, paths, files) {
  validate(files); // Check the entire pair before the first remote write.
  for (let i = 0; i < names.length; i += 1) {
    await fs.write(paths[i], files[i], { ifNoneMatch: true });
  }
}

async function boundedRead(paths) {
  const sizes = await Promise.all(paths.map(async (path) => (await stat(path)).size));
  if (sizes.some((size) => size > MAX_BYTES) || sizes.reduce((n, size) => n + size, 0) > MAX_BYTES) {
    throw new RecipeError("too_large", "Trace and report must total at most 4 MiB; nothing uploaded.");
  }
  const files = await Promise.all(paths.map((path) => readFile(path)));
  validate(files);
  return files;
}

async function runTest(dir) {
  // The browser/runner never receives the reusable storage credential.
  const env = { PATH: process.env.PATH, HOME: process.env.HOME, NO_COLOR: "1", PW_RECIPE_OUT: dir };
  for (const name of ["TMPDIR", "TEMP", "SystemRoot", "PLAYWRIGHT_BROWSERS_PATH"]) {
    if (process.env[name]) env[name] = process.env[name];
  }
  return new Promise((resolve, reject) => {
    const child = spawn(process.execPath, [join(root, "node_modules/@playwright/test/cli.js"), "test", "--config", join(root, "failure.config.mjs")], {
      cwd: root, env, stdio: "ignore",
    });
    child.on("error", reject);
    child.on("close", (code, signal) => signal ? reject(new Error("Runner interrupted.")) : resolve(code));
  });
}

async function remove(fs, path, allowOccupied = false) {
  try { await fs.delete(path); } catch (error) {
    if (error instanceof MountableError && (error.code === "not_found" || (allowOccupied && error.code === "directory_not_empty"))) return;
    throw error;
  }
}

async function main() {
  const [command, ...args] = process.argv.slice(2);
  const options = {};
  for (let i = 0; i < args.length; i += 2) {
    if (!["--filesystem", "--run", "--dir"].includes(args[i]) || !args[i + 1] || options[args[i]]) throw new Error("Invalid options.");
    options[args[i]] = args[i + 1];
  }
  if (!["run", "check", "cleanup"].includes(command) || !options["--filesystem"] || !options["--run"] || (command !== "cleanup" && !options["--dir"])) {
    throw new Error("usage: node failure.mjs run|check|cleanup --filesystem FS --run RUN [--dir NEW_DIR]");
  }
  const paths = pathsFor(options["--run"]);
  const fs = new Mountable().filesystem(options["--filesystem"], command === "check" ? "ro" : "rw");
  if (command === "cleanup") {
    for (const path of paths) await remove(fs, path);
    await remove(fs, `playwright-test/${options["--run"]}`);
    await remove(fs, "playwright-test", true);
    console.log("deleted trace.zip and report.json");
    return;
  }
  const dir = resolve(options["--dir"]);
  if (command === "check") {
    const local = await boundedRead(names.map((name) => join(dir, name)));
    for (let i = 0; i < names.length; i += 1) {
      const remote = Buffer.from((await fs.read(paths[i])).data);
      assertSameBytes(names[i], remote, local[i]);
      await mkdir(join(dir, "downloaded"), { recursive: true });
      await writeFile(join(dir, "downloaded", names[i]), remote);
      console.log(`match ${paths[i]} ${remote.length} ${sha256(remote)}`);
    }
    return;
  }
  await mkdir(dir); // Refuse an existing local output directory.
  const code = await runTest(dir);
  process.exitCode = code || 1;
  let uploadStarted = false;
  try {
    if (code !== 1) throw new RecipeError("runner_failed", "Expected runner exit 1.");
    const entries = await readdir(join(dir, "results"), { recursive: true });
    const traces = entries.filter((name) => name.endsWith("/trace.zip"));
    if (traces.length !== 1) throw new RecipeError("missing_trace", "Expected one retained trace.");
    const files = await boundedRead([join(dir, "results", traces[0]), join(dir, "report.json")]);
    await writeFile(join(dir, "trace.zip"), files[0]);
    uploadStarted = true;
    await upload(fs, paths, files);
    console.log(`retained runner_exit=${code}`);
    paths.forEach((path, i) => console.log(`${path} ${files[i].length} ${sha256(files[i])}`));
  } catch (error) {
    const detail = uploadStarted
      ? "inspect both remote paths before retrying; partial writes may exist"
      : "nothing uploaded; inspect --dir/report.json and browser setup (npx playwright install chromium)";
    console.error(`retention_failed runner_exit=${code} code=${error instanceof MountableError || error instanceof RecipeError ? error.code : "error"}; ${detail}`);
  }
}

if (process.argv[1] && fileURLToPath(import.meta.url) === process.argv[1]) {
  main().catch((error) => {
    // Do not echo provider errors, credentials, or local paths.
    console.error(`error: ${error instanceof MountableError || error instanceof RecipeError ? error.code : "command_failed"}`);
    process.exitCode ||= 1;
  });
}
