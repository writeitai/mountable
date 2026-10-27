// Path rules, closed-artifact checks, and Files API uploads for the recipe.
// The browser capture lives in capture.mjs. This module does not launch one.

import { createHash } from "node:crypto";

import { MountableError } from "mountable-sdk";

export const MAX_BYTES = 4 * 1024 * 1024;

const RUN_ID = /^[a-z0-9][a-z0-9-]{0,39}$/;
const PNG = Buffer.from([0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a]);

export class RecipeError extends Error {
  constructor(code, message) {
    super(message);
    this.name = "RecipeError";
    this.code = code;
  }
}

export function artifactPaths(run) {
  if (!RUN_ID.test(run)) {
    throw new RecipeError(
      "bad_run",
      "The run id must be 1 to 40 lowercase letters, digits or hyphens, and start with a letter or digit.",
    );
  }
  return {
    screenshot: `playwright/${run}/screenshot.png`,
    trace: `playwright/${run}/trace.zip`,
  };
}

export function sha256(bytes) {
  return createHash("sha256").update(bytes).digest("hex");
}

export function assertClosedArtifacts(files) {
  for (const [name, bytes] of [
    ["screenshot", files.screenshot],
    ["trace", files.trace],
  ]) {
    if (!Buffer.isBuffer(bytes)) {
      throw new RecipeError("bad_artifact", `The ${name} is missing.`);
    }
    if (bytes.length > MAX_BYTES) {
      throw new RecipeError(
        "too_large",
        `The ${name} is larger than 4 MiB. This recipe does not upload it.`,
      );
    }
  }
  if (files.screenshot.length < PNG.length || !files.screenshot.subarray(0, PNG.length).equals(PNG)) {
    throw new RecipeError("bad_screenshot", "The screenshot is not a PNG.");
  }
  assertTraceZip(files.trace);
}

export function assertTraceZip(bytes) {
  if (bytes.length < 22 || bytes[0] !== 0x50 || bytes[1] !== 0x4b) {
    throw new RecipeError(
      "bad_trace",
      "The trace is not a zip file. Stop tracing and close the browser before you upload it.",
    );
  }
  const eocd = findEocd(bytes);
  if (eocd < 0) {
    throw new RecipeError(
      "bad_trace",
      "The trace zip is not closed. Stop tracing and close the browser before you upload it.",
    );
  }
  const count = bytes.readUInt16LE(eocd + 10);
  let offset = bytes.readUInt32LE(eocd + 16);
  let sawTrace = false;
  for (let i = 0; i < count; i += 1) {
    if (offset + 46 > bytes.length || bytes.readUInt32LE(offset) !== 0x02014b50) {
      throw new RecipeError("bad_trace", "The trace zip is not closed.");
    }
    const nameLen = bytes.readUInt16LE(offset + 28);
    const extraLen = bytes.readUInt16LE(offset + 30);
    const commentLen = bytes.readUInt16LE(offset + 32);
    const name = bytes.subarray(offset + 46, offset + 46 + nameLen).toString("utf8");
    if (name === "trace.trace" || name.endsWith("/trace.trace") || name.endsWith(".trace")) {
      sawTrace = true;
    }
    offset += 46 + nameLen + extraLen + commentLen;
  }
  if (!sawTrace) {
    throw new RecipeError(
      "bad_trace",
      "The zip has no Playwright trace. Stop tracing before you upload it.",
    );
  }
}

function findEocd(bytes) {
  const start = Math.max(0, bytes.length - (22 + 65535));
  for (let offset = bytes.length - 22; offset >= start; offset -= 1) {
    if (bytes.readUInt32LE(offset) === 0x06054b50) {
      return offset;
    }
  }
  return -1;
}

function explain(error, label) {
  if (error instanceof RecipeError) {
    return error;
  }
  if (error instanceof MountableError) {
    if (error.code === "filesystem_spending_cap_reached") {
      return new RecipeError(
        error.code,
        `The ${label} was not stored. That filesystem's billable work stops at its cap.`,
      );
    }
    if (error.code === "precondition_failed") {
      return new RecipeError(
        error.code,
        `The ${label} is already stored. This recipe does not replace it. Use a new run id.`,
      );
    }
    if (error.code === "network_error") {
      return new RecipeError(
        "network_error",
        `Outcome unknown for the ${label}. Read it before writing it again.`,
      );
    }
    return new RecipeError(error.code, `The ${label} was not stored.`);
  }
  return new RecipeError("error", `The ${label} was not stored.`);
}

async function writeOne(fs, path, bytes, label) {
  try {
    await fs.write(path, bytes, { ifNoneMatch: true });
  } catch (error) {
    throw explain(error, label);
  }
}

export async function uploadPair(fs, paths, files) {
  assertClosedArtifacts(files);
  await writeOne(fs, paths.screenshot, files.screenshot, "screenshot");
  try {
    await writeOne(fs, paths.trace, files.trace, "trace");
  } catch (error) {
    if (error instanceof RecipeError && error.code === "filesystem_spending_cap_reached") {
      throw new RecipeError(
        error.code,
        "The screenshot is stored. The trace was not uploaded. That filesystem's billable work stops at its cap.",
      );
    }
    if (error instanceof RecipeError && error.code === "network_error") {
      throw new RecipeError(
        "network_error",
        "The screenshot is stored. Outcome unknown for the trace. Read it before writing it again.",
      );
    }
    throw error;
  }
}

export async function downloadPair(fs, paths) {
  return {
    screenshot: await readOne(fs, paths.screenshot, "screenshot"),
    trace: await readOne(fs, paths.trace, "trace"),
  };
}

async function readOne(fs, path, label) {
  try {
    const file = await fs.read(path);
    return Buffer.from(file.data);
  } catch (error) {
    if (error instanceof MountableError && error.code === "network_error") {
      throw new RecipeError("network_error", `Outcome unknown for the ${label}. Read it again.`);
    }
    if (error instanceof MountableError) {
      throw new RecipeError(error.code, `The ${label} could not be read.`);
    }
    throw new RecipeError("error", `The ${label} could not be read.`);
  }
}

export function assertSameBytes(label, remote, local) {
  if (!Buffer.isBuffer(local)) {
    throw new RecipeError("missing_local", `The local ${label} is missing. Capture writes it into --dir.`);
  }
  if (remote.length !== local.length || !remote.equals(local)) {
    throw new RecipeError("mismatch", `The ${label} bytes differ.`);
  }
}

export async function removePair(fs, paths) {
  await removeOne(fs, paths.screenshot, "screenshot");
  await removeOne(fs, paths.trace, "trace");
  await removeDirectory(fs, paths.screenshot.slice(0, paths.screenshot.lastIndexOf("/")), false);
  await removeDirectory(fs, "playwright", true);
}

async function removeDirectory(fs, path, allowOccupied) {
  try {
    await fs.delete(path);
  } catch (error) {
    if (error instanceof MountableError && error.code === "not_found") {
      return;
    }
    if (allowOccupied && error instanceof MountableError && error.code === "directory_not_empty") {
      return;
    }
    if (error instanceof MountableError) {
      throw new RecipeError(error.code, "The files are deleted. A directory is still there.");
    }
    throw error;
  }
}

async function removeOne(fs, path, label) {
  try {
    await fs.delete(path);
  } catch (error) {
    if (error instanceof MountableError && error.code === "not_found") {
      return;
    }
    if (error instanceof MountableError) {
      throw new RecipeError(error.code, `The ${label} was not deleted.`);
    }
    throw error;
  }
}
