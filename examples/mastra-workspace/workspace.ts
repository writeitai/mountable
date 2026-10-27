/** Trusted backend configuration, real Mastra tools/workflows, no model calls. */
import { parseArgs } from "node:util";
import { pathToFileURL } from "node:url";
import { createTool } from "@mastra/core/tools";
import { createStep, createWorkflow } from "@mastra/core/workflows";
import { Mountable, MountableError, type Filesystem } from "mountable-sdk";
import { z } from "zod";

export const MAX_NOTE_BYTES = 16 * 1024;
export const noteInput = z.object({
  text: z.string().min(1).refine(
    (text) => Buffer.byteLength(text, "utf8") <= MAX_NOTE_BYTES,
    "Use at most 16 KiB of UTF-8 text",
  ),
}).strict();
const emptyInput = z.object({}).strict();
const storedOutput = z.object({ stored: z.literal(true) }).strict();
const noteOutput = z.object({ text: z.string() }).strict();

function validatePath(path: string) {
  if (!/^notes\/[A-Za-z0-9][A-Za-z0-9_-]{0,63}\.md$/.test(path)) {
    throw new Error("Use a path like notes/brief.md");
  }
}

// Called once per authenticated user by trusted code. Never accept this scope
// from a model, tool arguments, or an unauthenticated request body.
export function workspaceTools(writer: Filesystem, reader: Filesystem, path: string) {
  validatePath(path);
  if (!writer.id || writer.id !== reader.id || reader.mode !== "ro") {
    throw new Error("Bind both handles to the same filesystem and use a read-only reader");
  }
  return {
    createNote: createTool({
      id: "create-note",
      description: "Create the backend-selected note. Never replaces an existing file.",
      inputSchema: noteInput,
      outputSchema: storedOutput,
      execute: async (input) => {
        const { text } = noteInput.parse(input);
        try {
          await writer.write(path, text, { ifNoneMatch: true });
        } catch (error) {
          if (error instanceof MountableError) {
            // Mastra serializes workflow errors. Keep the stable code in the
            // message, and leave server payloads out of workflow logs.
            throw new Error(`${error.code}: inspect the note before retrying.`);
          }
          throw error;
        }
        return { stored: true as const };
      },
    }),
    readNote: createTool({
      id: "read-note",
      description: "Read the backend-selected note from its filesystem.",
      inputSchema: emptyInput,
      outputSchema: noteOutput,
      execute: async (input) => {
        emptyInput.parse(input);
        const file = await reader.read(path);
        if (file.data.byteLength > MAX_NOTE_BYTES) throw new Error("Note exceeds 16 KiB");
        return { text: new TextDecoder("utf-8", { fatal: true }).decode(file.data) };
      },
    }),
  };
}

export function buildWorkflows(writer: Filesystem, reader: Filesystem, path: string) {
  const tools = workspaceTools(writer, reader, path);
  return {
    handoff: createWorkflow({
      id: "note-handoff", inputSchema: noteInput, outputSchema: noteOutput,
      retryConfig: { attempts: 0, delay: 0 },
    })
      .then(createStep(tools.createNote, { retries: 0 }))
      // The writer returned only { stored: true }. Pass no body to the reader.
      .map(async () => ({}))
      .then(createStep(tools.readNote, { retries: 0 }))
      .commit(),
    read: createWorkflow({
      id: "read-existing-note", inputSchema: emptyInput, outputSchema: noteOutput,
      retryConfig: { attempts: 0, delay: 0 },
    }).then(createStep(tools.readNote, { retries: 0 })).commit(),
  };
}

export function configuredWorkspace() {
  const filesystem = process.env.MOUNTABLE_FILESYSTEM_ID;
  const path = process.env.MOUNTABLE_NOTE_PATH;
  if (!filesystem || !path) throw new Error("Set MOUNTABLE_FILESYSTEM_ID and MOUNTABLE_NOTE_PATH");
  const client = new Mountable();
  const writer = client.filesystem(filesystem, "rw");
  const reader = client.filesystem(filesystem, "ro");
  // Validate the trusted path even for the cleanup command.
  workspaceTools(writer, reader, path);
  return { client, writer, reader, path };
}

export async function cleanupNote(writer: Filesystem, path: string, expectedText: string) {
  // Same path policy as the tools. Never recursively remove a shared directory.
  validatePath(path);
  const file = await writer.read(path);
  const expected = Buffer.from(noteInput.parse({ text: expectedText }).text, "utf8");
  if (!Buffer.from(file.data).equals(expected)) throw new Error("Cleanup refused: note contents differ");
  if (!file.info.etag) throw new Error("Cleanup refused: missing ETag");
  await writer.delete(path, { ifMatch: file.info.etag });
}

async function main() {
  const { positionals, values } = parseArgs({
    allowPositionals: true,
    options: { text: { type: "string" } },
  });
  const action = positionals[0];
  if (!["handoff", "read", "cleanup"].includes(action ?? "") || positionals.length !== 1) {
    throw new Error("Use handoff --text TEXT, read, or cleanup --text TEXT");
  }
  const { writer, reader, path } = configuredWorkspace();
  if (action === "cleanup") {
    await cleanupNote(writer, path, values.text ?? "");
    console.log("deleted");
    return;
  }
  const flows = buildWorkflows(writer, reader, path);
  const output = action === "handoff"
    ? await (await flows.handoff.createRun()).start({ inputData: { text: values.text ?? "" } })
    : await (await flows.read.createRun()).start({ inputData: {} });
  if (output.status !== "success") {
    if (output.status === "failed") throw output.error;
    throw new Error(`Workflow stopped: ${output.status}`);
  }
  console.log(output.result.text);
}

const entry = process.argv[1];
if (entry && import.meta.url === pathToFileURL(entry).href) {
  main().catch((error: unknown) => {
    // Print a short CLI error. Mastra also logs failed steps to stderr. No retry.
    console.error(error instanceof MountableError ? error.code : "Workflow failed; inspect the note before retrying.");
    process.exitCode = 1;
  });
}
