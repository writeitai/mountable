// Keep a Playwright screenshot and trace after this process exits.
//
// capture writes both closed files locally and uploads them.
// check is a fresh process: it downloads the files and compares every byte.
// cleanup deletes them.

import { mkdir, readFile, writeFile } from "node:fs/promises";
import { join } from "node:path";
import { fileURLToPath } from "node:url";

import { Mountable } from "mountable-sdk";

import { captureFixture } from "./capture.mjs";
import {
  artifactPaths,
  assertSameBytes,
  downloadPair,
  RecipeError,
  removePair,
  sha256,
  uploadPair,
} from "./store.mjs";

const fixturePath = fileURLToPath(new URL("./fixture.html", import.meta.url));

function usage() {
  return `usage:
  node artifacts.mjs capture --filesystem FS_ID --run RUN --dir DIR
  node artifacts.mjs check --filesystem FS_ID --run RUN --dir DIR
  node artifacts.mjs cleanup --filesystem FS_ID --run RUN`;
}

function parseArgs(argv) {
  const [command, ...rest] = argv;
  if (!["capture", "check", "cleanup"].includes(command)) {
    throw new RecipeError("usage", usage());
  }
  const options = { command };
  for (let i = 0; i < rest.length; i += 1) {
    const flag = rest[i];
    const value = rest[i + 1];
    if (!flag.startsWith("--") || value === undefined || value.startsWith("--")) {
      throw new RecipeError("usage", usage());
    }
    options[flag.slice(2)] = value;
    i += 1;
  }
  if (!options.filesystem || !options.run) {
    throw new RecipeError("usage", usage());
  }
  if (command !== "cleanup" && !options.dir) {
    throw new RecipeError("usage", usage());
  }
  return options;
}

function client() {
  return new Mountable();
}

async function readLocal(dir) {
  return {
    screenshot: await readFile(join(dir, "screenshot.png")),
    trace: await readFile(join(dir, "trace.zip")),
  };
}

async function writeLocal(dir, files) {
  await mkdir(dir, { recursive: true });
  await writeFile(join(dir, "screenshot.png"), files.screenshot);
  await writeFile(join(dir, "trace.zip"), files.trace);
}

export async function main(argv, env = process.env) {
  const options = parseArgs(argv);
  const paths = artifactPaths(options.run);
  if (!env.MOUNTABLE_API_KEY) {
    throw new RecipeError("unauthenticated", "Set MOUNTABLE_API_KEY in the environment.");
  }
  const mountable = client();
  if (options.command === "capture") {
    const files = await captureFixture(fixturePath);
    const fs = mountable.filesystem(options.filesystem, "rw");
    await uploadPair(fs, paths, files);
    await writeLocal(options.dir, files);
    return [
      `screenshot ${paths.screenshot} ${sha256(files.screenshot)} ${files.screenshot.length}`,
      `trace ${paths.trace} ${sha256(files.trace)} ${files.trace.length}`,
    ].join("\n");
  }
  if (options.command === "check") {
    const fs = mountable.filesystem(options.filesystem, "ro");
    const remote = await downloadPair(fs, paths);
    let local;
    try {
      local = await readLocal(options.dir);
    } catch {
      throw new RecipeError(
        "missing_local",
        "The local screenshot or trace is missing. Capture writes both into --dir.",
      );
    }
    assertSameBytes("screenshot", remote.screenshot, local.screenshot);
    assertSameBytes("trace", remote.trace, local.trace);
    return [
      `match ${paths.screenshot} ${remote.screenshot.length}`,
      `match ${paths.trace} ${remote.trace.length}`,
    ].join("\n");
  }
  const fs = mountable.filesystem(options.filesystem, "rw");
  await removePair(fs, paths);
  return [`deleted ${paths.screenshot}`, `deleted ${paths.trace}`].join("\n");
}

function redact(text) {
  const key = process.env.MOUNTABLE_API_KEY;
  if (key) {
    return text.split(key).join("[redacted]");
  }
  return text;
}

if (process.argv[1] && fileURLToPath(import.meta.url) === process.argv[1]) {
  main(process.argv.slice(2))
    .then((text) => {
      process.stdout.write(`${text}\n`);
    })
    .catch((error) => {
      const detail = error instanceof Error ? error.message : "the command failed";
      const line =
        error instanceof RecipeError ? `error: ${error.code}: ${error.message}` : `error: ${detail}`;
      process.stderr.write(`${redact(line)}\n`);
      process.exitCode = error instanceof RecipeError && error.code === "usage" ? 2 : 1;
    });
}
