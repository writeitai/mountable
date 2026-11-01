/**
 * A Deno script that saves one backend-selected note with the released
 * mountable-sdk package, then reads it from another process.
 */
import { type Filesystem, Mountable, MountableError } from "mountable-sdk";

export const MAX_NOTE_BYTES = 16 * 1024;
const PATH_RE = /^notes\/[A-Za-z0-9][A-Za-z0-9_-]{0,63}\.md$/;

export function requireEnv(name: string): string {
  const value = Deno.env.get(name);
  if (!value) throw new Error(`Set ${name}`);
  return value;
}

/** Optional settings stay unset when this process was not granted that name. */
export function optionalEnv(name: string): string | undefined {
  try {
    const value = Deno.env.get(name);
    return value || undefined;
  } catch (error) {
    if (error instanceof Deno.errors.NotCapable) return undefined;
    throw error;
  }
}

export function validatePath(path: string): void {
  if (!PATH_RE.test(path)) throw new Error("Use a path like notes/brief.md");
}

export function encodeNote(text: string): Uint8Array {
  if (text.length === 0) throw new Error("Use nonempty text");
  const bytes = new TextEncoder().encode(text);
  if (bytes.byteLength > MAX_NOTE_BYTES) {
    throw new Error("Use at most 16 KiB of UTF-8 text");
  }
  return bytes;
}

export function sameBytes(left: Uint8Array, right: Uint8Array): boolean {
  if (left.byteLength !== right.byteLength) return false;
  for (let index = 0; index < left.byteLength; index++) {
    if (left[index] !== right[index]) return false;
  }
  return true;
}

export function openClient(apiKey: string, baseUrl?: string): Mountable {
  return new Mountable(baseUrl ? { apiKey, baseUrl } : { apiKey });
}

export async function createNote(
  writer: Filesystem,
  path: string,
  text: string,
): Promise<void> {
  validatePath(path);
  await writer.write(path, encodeNote(text), { ifNoneMatch: true });
}

export async function readNote(
  reader: Filesystem,
  path: string,
): Promise<string> {
  validatePath(path);
  const file = await reader.read(path);
  if (file.data.byteLength > MAX_NOTE_BYTES) {
    throw new Error("Note exceeds 16 KiB");
  }
  return new TextDecoder("utf-8", { fatal: true }).decode(file.data);
}

export async function cleanupNote(
  writer: Filesystem,
  path: string,
  expectedText: string,
): Promise<void> {
  validatePath(path);
  const expected = encodeNote(expectedText);
  const file = await writer.read(path);
  if (!sameBytes(file.data, expected)) {
    throw new Error("Cleanup refused: note contents differ");
  }
  if (!file.info.etag) throw new Error("Cleanup refused: missing ETag");
  await writer.delete(path, { ifMatch: file.info.etag });
}

/** Tries one create through a read-only handle and returns the service's 403 code. */
export async function readOnlyWriteCode(
  reader: Filesystem,
  path: string,
  text: string,
): Promise<string> {
  try {
    await createNote(reader, path, text);
  } catch (error) {
    if (error instanceof MountableError && error.status === 403) {
      return error.code;
    }
    throw error;
  }
  throw new Error("Read-only write was accepted");
}

function argumentText(args: string[]): string {
  if (
    args.length !== 2 || args[0] !== "--text" || args[1] === undefined ||
    args[1].startsWith("--")
  ) {
    throw new Error("Pass --text TEXT");
  }
  return args[1];
}

async function run(args: string[]): Promise<void> {
  const action = args[0];
  if (
    !action || !["write", "read", "cleanup", "readonly-write"].includes(action)
  ) {
    throw new Error("Use write, read, cleanup, or readonly-write");
  }
  // Read the key before any network call. A missing env permission stops here.
  const apiKey = requireEnv("MOUNTABLE_API_KEY");
  const filesystemId = requireEnv("MOUNTABLE_FILESYSTEM_ID");
  const path = requireEnv("MOUNTABLE_NOTE_PATH");
  const baseUrl = optionalEnv("MOUNTABLE_API_URL");
  const client = openClient(apiKey, baseUrl);
  const writer = client.filesystem(filesystemId, "rw");
  const reader = client.filesystem(filesystemId, "ro");
  if (action === "write") {
    await createNote(writer, path, argumentText(args.slice(1)));
    console.log("stored");
    return;
  }
  if (action === "read") {
    if (args.length !== 1) throw new Error("read takes no arguments");
    console.log(await readNote(reader, path));
    return;
  }
  if (action === "cleanup") {
    await cleanupNote(writer, path, argumentText(args.slice(1)));
    console.log("deleted");
    return;
  }
  console.log(
    await readOnlyWriteCode(reader, path, argumentText(args.slice(1))),
  );
}

if (import.meta.main) {
  try {
    await run(Deno.args);
  } catch (error) {
    if (error instanceof MountableError) console.error(error.code);
    else if (error instanceof Error) console.error(error.message);
    else console.error("failed");
    Deno.exit(1);
  }
}
