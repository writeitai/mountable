import { tool } from "ai";
import { Mountable, type Filesystem } from "mountable-sdk";
import { z } from "zod";

export const noteInput = z.object({
  text: z.string().min(1).refine(
    (text) => Buffer.byteLength(text, "utf8") <= 16 * 1024,
    "Use at most 16 KiB of UTF-8 text",
  ),
}).strict();

export function workspaceTools(writer: Filesystem, reader: Filesystem, path: string) {
  if (!/^notes\/[a-zA-Z0-9][a-zA-Z0-9_-]{0,63}\.md$/.test(path)) {
    throw new Error("Use a path like notes/brief.md");
  }
  return {
    create_note: tool({
      description: "Create the application's configured note. Never replaces an existing note.",
      inputSchema: noteInput,
      execute: async (input) => {
        const { text } = noteInput.parse(input);
        await writer.write(path, text, { ifNoneMatch: true });
        // The next step must read the file, rather than receive its body here.
        return "stored";
      },
    }),
    read_note: tool({
      description: "Read the application's configured note from its filesystem.",
      inputSchema: z.object({}).strict(),
      execute: async () => {
        const file = await reader.read(path);
        if (file.data.byteLength > 16 * 1024) throw new Error("Note exceeds 16 KiB");
        return new TextDecoder("utf-8", { fatal: true }).decode(file.data);
      },
    }),
  };
}

export function configuredTools() {
  const filesystem = process.env.MOUNTABLE_FILESYSTEM_ID;
  const path = process.env.MOUNTABLE_NOTE_PATH;
  if (!filesystem || !path || !process.env.MOUNTABLE_API_KEY) {
    throw new Error("Set the server's mountable.io environment variables");
  }
  const client = new Mountable();
  return workspaceTools(client.filesystem(filesystem), client.filesystem(filesystem, "ro"), path);
}
